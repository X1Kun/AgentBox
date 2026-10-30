package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

var testCtx = context.Background()

type harness struct {
	client             *fake.FakeDynamicClient
	cfg                Config
	service            *Service
	mu                 sync.Mutex
	next               int
	creates            int
	patches            int
	lostReply, unknown bool
	conflictOnce       bool
	now                time.Time
}

func setup(t *testing.T) *harness {
	t.Helper()
	kinds := map[schema.GroupVersionResource]string{Claims: "SandboxClaimList", Pools: "SandboxWarmPoolList", Sandboxes: "SandboxList", Pods: "PodList", Services: "ServiceList"}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	h := &harness{client: client, cfg: Config{Namespace: "work", Pool: "python", Instance: "test-api", CloseWait: 0}, now: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)}
	pool := object("SandboxWarmPool", "python", "pool-uid", "extensions.agents.x-k8s.io/v1beta1")
	h.put(t, Pools, pool)
	h.service = h.restart(t)
	client.PrependReactor("create", "sandboxclaims", func(action kt.Action) (bool, runtime.Object, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.creates++
		if h.unknown {
			return true, nil, io.ErrUnexpectedEOF
		}
		c := action.(kt.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		h.next++
		c.SetUID(types.UID("claim-uid-" + strconv.Itoa(h.next)))
		c.SetResourceVersion("1")
		c.SetGeneration(1)
		if err := client.Tracker().Create(Claims, c, h.cfg.Namespace); err != nil {
			return true, nil, err
		}
		if h.lostReply {
			h.lostReply = false
			return true, nil, io.ErrUnexpectedEOF
		}
		return true, c, nil
	})
	client.PrependReactor("patch", "sandboxclaims", func(action kt.Action) (bool, runtime.Object, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.patches++
		a := action.(kt.PatchAction)
		current, err := client.Tracker().Get(Claims, h.cfg.Namespace, a.GetName())
		if err != nil {
			return true, nil, err
		}
		c := current.(*unstructured.Unstructured)
		if h.conflictOnce {
			h.conflictOnce = false
			return true, nil, apierrors.NewConflict(Claims.GroupResource(), c.GetName(), errors.New("injected conflict"))
		}
		var ops []map[string]any
		_ = json.Unmarshal(a.GetPatch(), &ops)
		if len(ops) < 2 || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != string(c.GetUID()) || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != c.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(Claims.GroupResource(), c.GetName(), errors.New("failed identity/version precondition"))
		}
		handled, object, err := kt.ObjectReaction(client.Tracker())(action)
		if err != nil || !handled {
			return handled, object, err
		}
		updated := object.(*unstructured.Unstructured)
		rv, _ := strconv.Atoi(c.GetResourceVersion())
		updated.SetResourceVersion(strconv.Itoa(rv + 1))
		if !reflect.DeepEqual(c.Object["spec"], updated.Object["spec"]) {
			updated.SetGeneration(c.GetGeneration() + 1)
		}
		err = client.Tracker().Update(Claims, updated, h.cfg.Namespace)
		return true, updated, err
	})
	return h
}
func (h *harness) restart(t *testing.T) *Service {
	t.Helper()
	s, err := New(h.client, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return h.now }
	return s
}
func object(kind, name, uid, version string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": version, "kind": kind}}
	o.SetName(name)
	o.SetNamespace("work")
	o.SetUID(types.UID(uid))
	o.SetResourceVersion("1")
	o.SetGeneration(1)
	return o
}
func (h *harness) put(t *testing.T, g schema.GroupVersionResource, o *unstructured.Unstructured) {
	t.Helper()
	_, e := h.client.Tracker().Get(g, h.cfg.Namespace, o.GetName())
	var err error
	if e == nil {
		err = h.client.Tracker().Update(g, o, h.cfg.Namespace)
	} else {
		err = h.client.Tracker().Create(g, o, h.cfg.Namespace)
	}
	if err != nil {
		t.Fatal(err)
	}
}
func (h *harness) claim(t *testing.T, id string) *unstructured.Unstructured {
	t.Helper()
	parts := idPattern.FindStringSubmatch(id)
	v, e := h.client.Resource(Claims).Namespace("work").Get(testCtx, parts[1], metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func conditions(o *unstructured.Unstructured, status, reason string) {
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Ready", "status": status, "reason": reason, "observedGeneration": o.GetGeneration()}}, "status", "conditions")
}
func owner(o *unstructured.Unstructured, kind, name, uid string) {
	yes := true
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: kind, Name: name, UID: types.UID(uid), Controller: &yes}})
}
func (h *harness) ready(t *testing.T, id string) {
	c := h.claim(t, id)
	conditions(c, "True", "")
	_ = unstructured.SetNestedField(c.Object, "sandbox", "status", "sandbox", "name")
	h.put(t, Claims, c)
	box := object("Sandbox", "sandbox", "sandbox-uid", "agents.x-k8s.io/v1beta1")
	owner(box, "SandboxClaim", c.GetName(), string(c.GetUID()))
	conditions(box, "True", "")
	h.put(t, Sandboxes, box)
	pod := object("Pod", "pod", "pod-uid", "v1")
	owner(pod, "Sandbox", "sandbox", "sandbox-uid")
	pod.SetLabels(map[string]string{"sandbox": "this"})
	pod.Object["spec"] = map[string]any{"runtimeClassName": "agentbox-gvisor", "automountServiceAccountToken": false}
	pod.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	h.put(t, Pods, pod)
	service := object("Service", "service", "service-uid", "v1")
	owner(service, "Sandbox", "sandbox", "sandbox-uid")
	service.Object["spec"] = map[string]any{"clusterIP": "None", "selector": map[string]any{"sandbox": "this"}}
	h.put(t, Services, service)
}
func (h *harness) create(t *testing.T) View {
	t.Helper()
	v, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestCreateReplayResponseLossAndRestart(t *testing.T) {
	h := setup(t)
	h.lostReply = true
	v, replay, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "request-1"})
	if err != nil || !replay {
		t.Fatalf("lost reply not recovered: %v %v", replay, err)
	}
	expiry := v.ExpiresAt
	h.now = h.now.Add(time.Minute)
	fresh := h.restart(t)
	again, replay, err := fresh.Create(testCtx, "alice", CreateRequest{RequestID: "request-1", TTLSeconds: 600})
	if err != nil || !replay || again.ID != v.ID || again.ExpiresAt != expiry || h.creates != 1 {
		t.Fatalf("creation repeated/extended: %#v %v creates=%d", again, err, h.creates)
	}
	_, _, err = fresh.Create(testCtx, "alice", CreateRequest{RequestID: "request-1", TTLSeconds: 120})
	if Code(err) != "request_conflict" {
		t.Fatalf("conflicting request accepted: %v", err)
	}
}
func TestUnknownCreateNeverPostsAgain(t *testing.T) {
	h := setup(t)
	h.unknown = true
	_, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "request-1"})
	if Code(err) != "create_outcome_unknown" || h.creates != 1 {
		t.Fatalf("unknown create replayed: %v count=%d", err, h.creates)
	}
}
func TestOwnerIsolationBeforeQueryOrCloseMutation(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	for _, op := range []func() (View, error){func() (View, error) { return h.service.Get(testCtx, "bob", v.ID) }, func() (View, error) { return h.service.Close(testCtx, "bob", v.ID) }} {
		if _, e := op(); Code(e) != "not_found" {
			t.Fatalf("cross-owner request accepted: %v", e)
		}
	}
	if h.patches != 0 {
		t.Fatal("cross-owner close mutated claim")
	}
	other, _, err := h.service.Create(testCtx, "bob", CreateRequest{RequestID: "request-1"})
	if err != nil || other.ID == v.ID {
		t.Fatalf("owner names collide: %v", err)
	}
}
func TestBindingSurvivesRestartAndReplacementInvalidationIsSticky(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	h.conflictOnce = true
	first, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || first.State != "ready" || first.IdentityRevision == "" {
		t.Fatalf("binding failed: %#v %v", first, err)
	}
	h.service = h.restart(t)
	same, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || same.IdentityRevision != first.IdentityRevision {
		t.Fatal("restart forgot binding")
	}
	pod, _ := h.client.Resource(Pods).Namespace("work").Get(testCtx, "pod", metav1.GetOptions{})
	pod.SetUID("replacement-pod")
	h.put(t, Pods, pod)
	changed, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || changed.State != "invalidated" || changed.IdentityRevision != first.IdentityRevision {
		t.Fatalf("replacement adopted: %#v %v", changed, err)
	}
	pod.SetUID("pod-uid")
	h.put(t, Pods, pod)
	h.service = h.restart(t)
	again, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || again.State != "invalidated" {
		t.Fatalf("invalidation forgotten: %#v %v", again, err)
	}
}
func TestTransientBackingLookupDoesNotInvalidate(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	_, _ = h.service.Get(testCtx, "alice", v.ID)
	h.client.PrependReactor("get", "sandboxes", func(kt.Action) (bool, runtime.Object, error) { return true, nil, io.ErrUnexpectedEOF })
	if _, err := h.service.Get(testCtx, "alice", v.ID); err == nil {
		t.Fatal("backend error hidden")
	}
	if h.claim(t, v.ID).GetAnnotations()[invalidKey] != "" {
		t.Fatal("transport error invalidated identity")
	}
}
func TestCloseWaitsForCurrentExpiryAcknowledgementAndAllResources(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	_, _ = h.service.Get(testCtx, "alice", v.ID)
	closed, err := h.service.Close(testCtx, "alice", v.ID)
	if err != nil || closed.State != "closing" || closed.CleanupComplete {
		t.Fatalf("premature cleanup: %#v %v", closed, err)
	}
	c := h.claim(t, v.ID)
	conditions(c, "False", ext.ClaimExpiredReason)
	h.put(t, Claims, c)
	_ = h.client.Tracker().Delete(Sandboxes, "work", "sandbox")
	_ = h.client.Tracker().Delete(Pods, "work", "pod")
	pending, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || pending.State != "closing" {
		t.Fatal("remaining service overlooked")
	}
	_ = h.client.Tracker().Delete(Services, "work", "service")
	done, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || done.State != "closed" || !done.CleanupComplete {
		t.Fatalf("cleanup not recognized: %#v %v", done, err)
	}
	before := h.patches
	again, err := h.service.Close(testCtx, "alice", v.ID)
	if err != nil || again.State != "closed" || h.patches != before {
		t.Fatal("close not idempotent")
	}
	replay, _, err := h.restart(t).Create(testCtx, "alice", CreateRequest{RequestID: "request-1"})
	if err != nil || replay.State != "closed" || h.creates != 1 {
		t.Fatal("closed creation key reused")
	}
}
func TestEmptyResourcesWithoutControllerAckAreNotClosed(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	x, e := h.service.Close(testCtx, "alice", v.ID)
	if e != nil || x.State != "closing" || x.CleanupComplete {
		t.Fatal("absence alone counted as cleanup")
	}
}
func TestOldClaimUIDNeverTargetsReplacementRecord(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	c := h.claim(t, v.ID)
	c.SetUID("other-incarnation")
	h.put(t, Claims, c)
	_, err := h.service.Close(testCtx, "alice", v.ID)
	if Code(err) != "identity_changed" || h.patches != 0 {
		t.Fatalf("old ID changed new record: %v", err)
	}
}
func TestServiceSelectorAndRuntimeBoundaryMustMatch(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	service, _ := h.client.Resource(Services).Namespace("work").Get(testCtx, "service", metav1.GetOptions{})
	_ = unstructured.SetNestedStringMap(service.Object, map[string]string{"sandbox": "foreign"}, "spec", "selector")
	h.put(t, Services, service)
	if _, err := h.service.Get(testCtx, "alice", v.ID); Code(err) != "integrity_error" {
		t.Fatal("foreign service routing accepted")
	}
}

func TestConcurrentCreateAndBindingUseSameObjects(t *testing.T) {
	h := setup(t)
	var views [2]View
	var errs [2]error
	var wg sync.WaitGroup
	for i := range views {
		wg.Go(func() {
			views[i], _, errs[i] = h.service.Create(testCtx, "alice", CreateRequest{RequestID: "concurrent"})
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || views[0].ID != views[1].ID {
		t.Fatalf("concurrent creation split: %#v %v", views, errs)
	}
	objects, e := h.client.Resource(Claims).Namespace("work").List(testCtx, metav1.ListOptions{})
	if e != nil || len(objects.Items) != 1 {
		t.Fatal("duplicate creation")
	}
	h.ready(t, views[0].ID)
	other := h.restart(t)
	wg.Go(func() { views[0], errs[0] = h.service.Get(testCtx, "alice", views[0].ID) })
	wg.Go(func() { views[1], errs[1] = other.Get(testCtx, "alice", views[1].ID) })
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || views[0].IdentityRevision == "" || views[0].IdentityRevision != views[1].IdentityRevision {
		t.Fatalf("binding split: %#v %v", views, errs)
	}
}
func TestNaturalExpiryRequiresCurrentGeneration(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.now = h.now.Add(11 * time.Minute)
	pending, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || pending.State != "closing" || pending.CleanupComplete {
		t.Fatal("time alone counted as cleanup")
	}
	c := h.claim(t, v.ID)
	conditions(c, "False", ext.ClaimExpiredReason)
	h.put(t, Claims, c)
	done, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || done.State != "closed" {
		t.Fatalf("retained expiry not closed: %#v %v", done, err)
	}
}

func TestDefiniteAdmissionRejectionIsNotAnUnknownCreate(t *testing.T) {
	h := setup(t)
	h.client.PrependReactor("create", "sandboxclaims", func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(Claims.GroupResource(), "claim", errors.New("quota exceeded"))
	})
	_, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "request-1"})
	if Code(err) != "admission_rejected" {
		t.Fatalf("definite rejection misclassified: %v", err)
	}
}

func TestBoundTerminalPodInvalidatesBeforeUIDReplacement(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	before, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	pod, _ := h.client.Resource(Pods).Namespace("work").Get(testCtx, "pod", metav1.GetOptions{})
	_ = unstructured.SetNestedField(pod.Object, "Succeeded", "status", "phase")
	h.put(t, Pods, pod)
	after, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || after.State != "invalidated" || after.IdentityRevision != before.IdentityRevision {
		t.Fatalf("terminal environment accepted: %#v %v", after, err)
	}
}
func TestReadinessFlapDoesNotDestroyBinding(t *testing.T) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	before, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	pod, _ := h.client.Resource(Pods).Namespace("work").Get(testCtx, "pod", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
	h.put(t, Pods, pod)
	transient, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || transient.State != "unavailable" {
		t.Fatal("readiness loss not visible")
	}
	_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	h.put(t, Pods, pod)
	restored, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || restored.State != "ready" || restored.IdentityRevision != before.IdentityRevision {
		t.Fatal("readiness flap invalidated identity")
	}
}
