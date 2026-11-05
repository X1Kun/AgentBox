package admission

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/admission/health"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"io"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fixedSource struct {
	sample health.Sample
	err    error
}

func (s *fixedSource) Read(context.Context) (health.Sample, error) { return s.sample, s.err }
func object(kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	api := "kueue.x-k8s.io/v1beta2"
	if kind == "Namespace" {
		api = "v1"
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "spec": spec}}
	o.SetName(name)
	o.SetNamespace(namespace)
	o.SetUID(types.UID(name + "-uid"))
	o.SetResourceVersion("1")
	o.SetGeneration(1)
	o.SetCreationTimestamp(metav1.NewTime(time.Unix(2000, 0)))
	o.SetLabels(map[string]string{"agentbox.dev/session": policy().Owner})
	return o
}
func workload(name string) *unstructured.Unstructured {
	cfg := policy()
	container := map[string]any{"name": "image-worker", "image": "test@sha256:abc", "resources": map[string]any{"requests": map[string]any{"cpu": "500m", "memory": "256Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "256Mi"}}}
	o := object("Workload", name, cfg.Namespace, map[string]any{"queueName": cfg.LocalQueue, "podSets": []any{map[string]any{"name": "main", "count": int64(1), "template": map[string]any{"spec": map[string]any{"runtimeClassName": "agentbox-gvisor", "automountServiceAccountToken": false, "restartPolicy": "Never", "containers": []any{container}}}}}})
	o.Object["status"] = map[string]any{"admission": map[string]any{"clusterQueue": cfg.ClusterQueue}, "conditions": []any{map[string]any{"type": "QuotaReserved", "status": "True", "reason": "QuotaReserved", "message": "fixture", "observedGeneration": int64(1), "lastTransitionTime": time.Unix(2000, 0).UTC().Format(time.RFC3339)}}, "admissionChecks": []any{map[string]any{"name": "other", "state": "Pending", "message": "owned elsewhere", "lastTransitionTime": time.Unix(2000, 0).UTC().Format(time.RFC3339)}, map[string]any{"name": cfg.Check, "state": "Pending", "message": "", "lastTransitionTime": time.Unix(2000, 0).UTC().Format(time.RFC3339)}}}
	return o
}

type fixture struct {
	client      *fake.FakeDynamicClient
	controller  *Controller
	source      *fixedSource
	now         time.Time
	mu          sync.Mutex
	beforePatch func(ktesting.PatchAction) error
	afterPatch  func(ktesting.PatchAction) error
}

func newFixture(t *testing.T, count int) *fixture {
	t.Helper()
	cfg := policy()
	ac := object("AdmissionCheck", cfg.Check, "", map[string]any{"controllerName": ControllerName})
	ac.SetAnnotations(map[string]string{RevisionAnnotation: cfg.Health.Revision})
	objs := []runtime.Object{object("Namespace", cfg.Namespace, "", map[string]any{}), object("LocalQueue", cfg.LocalQueue, cfg.Namespace, map[string]any{"clusterQueue": cfg.ClusterQueue}), object("ClusterQueue", cfg.ClusterQueue, "", map[string]any{"admissionChecksStrategy": map[string]any{"admissionChecks": []any{map[string]any{"name": cfg.Check}}}}), ac}
	for i := 0; i < count; i++ {
		objs = append(objs, workload("work-"+strconv.Itoa(i)))
	}
	cl := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{Workloads: "WorkloadList"}, objs...)
	f := &fixture{client: cl, now: time.Unix(2000, 0), source: &fixedSource{}}
	// An explicit CAS-capable reactor; client-go's object tracker is not an API
	// server and cannot independently prove the wire concurrency guarantee.
	cl.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := a.(ktesting.PatchAction)
		if f.beforePatch != nil {
			if err := f.beforePatch(p); err != nil {
				return true, nil, err
			}
		}
		obj, err := cl.Tracker().Get(p.GetResource(), p.GetNamespace(), p.GetName())
		if err != nil {
			return true, nil, err
		}
		old, _ := json.Marshal(obj)
		patch, err := jsonpatch.DecodePatch(p.GetPatch())
		if err != nil {
			return true, nil, err
		}
		data, err := patch.Apply(old)
		if err != nil {
			return true, nil, apierrors.NewConflict(p.GetResource().GroupResource(), p.GetName(), errors.New("CAS conflict"))
		}
		updated := &unstructured.Unstructured{}
		_ = json.Unmarshal(data, updated)
		n, _ := strconv.Atoi(updated.GetResourceVersion())
		updated.SetResourceVersion(strconv.Itoa(n + 1))
		if err = cl.Tracker().Update(p.GetResource(), updated, p.GetNamespace()); err != nil {
			return true, nil, err
		}
		if f.afterPatch != nil {
			if err := f.afterPatch(p); err != nil {
				return true, nil, err
			}
		}
		return true, updated, nil
	})
	backend, err := New(cl, cfg, f.source, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.controller = backend
	backend.now = func() time.Time { return f.now }
	f.source.sample = sample(f.now, 1, 100)
	return f
}
func (f *fixture) step(t *testing.T, offset int, ratio int64) error {
	t.Helper()
	f.now = time.Unix(2000, 0).Add(seconds(offset))
	f.source.sample = sample(f.now, int64(offset+1), ratio)
	return f.controller.Step(context.Background())
}
func (f *fixture) entry(t *testing.T, name string) map[string]any {
	t.Helper()
	o, err := f.client.Resource(Workloads).Namespace(policy().Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, entry, _ := checkEntry(o, policy().Check)
	return entry
}

func TestSerialPermissionPacingProtectsOtherChecksAndAdmittedWork(t *testing.T) {
	f := newFixture(t, 3)
	for _, at := range []int{0, 2, 6} {
		if err := f.step(t, at, 100); err != nil {
			t.Fatal(err)
		}
	}
	ready := 0
	for i := 0; i < 3; i++ {
		if f.entry(t, "work-"+strconv.Itoa(i))["state"] == "Ready" {
			ready++
		}
	}
	if ready != 1 {
		t.Fatal("burst", ready)
	}
	// A new leader/restarted backend rebuilds durable grant history and its
	// own cooldown, instead of immediately releasing the backlog.
	restarted, err := New(f.client, policy(), f.source, f.controller.logger)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = f.controller.now
	f.controller = restarted
	if err = f.step(t, 7, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-1")["state"] != "Pending" {
		t.Fatal("restart burst")
	}
	if err = f.step(t, 13, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-1")["state"] != "Ready" {
		t.Fatal("did not recover")
	}
	if err = f.step(t, 20, 0); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-0")["state"] != "Ready" {
		t.Fatal("revoked durable permit")
	}
	o, _ := f.client.Resource(Workloads).Namespace(policy().Namespace).Get(context.Background(), "work-0", metav1.GetOptions{})
	_, other, _ := checkEntry(o, "other")
	if other["message"] != "owned elsewhere" || other["state"] != "Pending" {
		t.Fatal(other)
	}
}
func TestPendingTimeoutSetsNativeRetryDelayAndNeverTouchesAdmitted(t *testing.T) {
	f := newFixture(t, 2)
	o, _ := f.client.Resource(Workloads).Namespace(policy().Namespace).Get(context.Background(), "work-1", metav1.GetOptions{})
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	conditions = append(conditions, map[string]any{"type": "Admitted", "status": "True"})
	_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
	_ = f.client.Tracker().Update(Workloads, o, o.GetNamespace())
	if err := f.step(t, 40, 0); err != nil {
		t.Fatal(err)
	}
	entry := f.entry(t, "work-0")
	if entry["state"] != "Retry" || entry["requeueAfterSeconds"] != int64(policy().RetrySeconds) {
		t.Fatal(entry)
	}
	if f.entry(t, "work-1")["state"] != "Pending" {
		t.Fatal("admitted mutated")
	}
}
func TestAdmittedRaceConflictsAndIsRejudgedWithoutBlindRetry(t *testing.T) {
	f := newFixture(t, 1)
	f.beforePatch = func(p ktesting.PatchAction) error {
		if p.GetResource() != Workloads {
			return nil
		}
		o, _ := f.client.Tracker().Get(Workloads, policy().Namespace, p.GetName())
		u := o.(*unstructured.Unstructured).DeepCopy()
		u.SetResourceVersion("99")
		conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		conditions = append(conditions, map[string]any{"type": "Admitted", "status": "True"})
		_ = unstructured.SetNestedSlice(u.Object, conditions, "status", "conditions")
		return f.client.Tracker().Update(Workloads, u, u.GetNamespace())
	}
	if err := f.step(t, 40, 0); !apierrors.IsConflict(err) {
		t.Fatal(err)
	}
	f.beforePatch = nil
	if err := f.step(t, 41, 0); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-0")["state"] != "Pending" {
		t.Fatal("blind Retry after Admitted race")
	}
}
func TestLostReadyReplyConsumesIntervalAndRecoversOnlySameUID(t *testing.T) {
	f := newFixture(t, 2)
	for _, at := range []int{0, 2} {
		if err := f.step(t, at, 100); err != nil {
			t.Fatal(err)
		}
	}
	f.afterPatch = func(p ktesting.PatchAction) error {
		if p.GetResource() == Workloads {
			return errors.New("lost response")
		}
		return nil
	}
	if err := f.step(t, 6, 100); err == nil {
		t.Fatal("expected lost response")
	}
	if f.entry(t, "work-0")["state"] != "Ready" {
		t.Fatal("write not committed")
	}
	f.afterPatch = nil
	if err := f.step(t, 7, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-1")["state"] != "Pending" {
		t.Fatal("unknown write caused burst")
	}
	f.now = time.Unix(2010, 0)
	o, _ := f.client.Resource(Workloads).Namespace(policy().Namespace).Get(context.Background(), "work-1", metav1.GetOptions{})
	replacement := o.DeepCopy()
	replacement.SetUID("new-uid")
	replacement.SetResourceVersion("55")
	_ = f.client.Tracker().Update(Workloads, replacement, o.GetNamespace())
	if err := f.controller.writeCheck(context.Background(), o, "Ready", "test", f.now); !apierrors.IsConflict(err) {
		t.Fatal("adopted replacement", err)
	}
}
func TestReloadRequiresNewRevisionAndFreshHealth(t *testing.T) {
	f := newFixture(t, 1)
	current := policy()
	f.controller.SetReload(func() (Config, error) { return current, nil }, func(c Config) (health.Source, error) { return f.source, nil })
	if err := f.step(t, 0, 100); err != nil {
		t.Fatal(err)
	}
	current.StableSeconds++
	if err := f.step(t, 6, 100); err == nil {
		t.Fatal("same-revision policy mutation accepted")
	}
	current.Health.Revision = "v2"
	if err := f.step(t, 7, 100); err == nil {
		t.Fatal("old admission check config accepted")
	}
	ac, _ := f.client.Resource(Checks).Get(context.Background(), current.Check, metav1.GetOptions{})
	ac.SetAnnotations(map[string]string{RevisionAnnotation: "v2"})
	_ = f.client.Tracker().Update(Checks, ac, "")
	f.now = f.now.Add(time.Second)
	f.source.sample = sample(f.now, 10, 100)
	if err := f.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-0")["state"] != "Pending" {
		t.Fatal("old source released permission")
	}
}
func TestInstallationReplacementAndSourceLossFailClosed(t *testing.T) {
	f := newFixture(t, 1)
	if err := f.step(t, 0, 100); err != nil {
		t.Fatal(err)
	}
	f.source.err = errors.New("unavailable")
	if err := f.step(t, 6, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-0")["state"] != "Pending" {
		t.Fatal("source lost")
	}
	o, _ := f.client.Resource(Namespaces).Get(context.Background(), policy().Namespace, metav1.GetOptions{})
	o.SetUID("replacement")
	_ = f.client.Tracker().Update(Namespaces, o, "")
	if err := f.step(t, 8, 100); err == nil {
		t.Fatal("installation adopted")
	}
}

func TestSlowReadyReplyDoesNotCompressNextPermission(t *testing.T) {
	f := newFixture(t, 2)
	for _, at := range []int{0, 2} {
		if err := f.step(t, at, 100); err != nil {
			t.Fatal(err)
		}
	}
	f.afterPatch = func(p ktesting.PatchAction) error {
		if p.GetResource() == Workloads {
			f.now = f.now.Add(2 * time.Second)
		}
		return nil
	}
	if err := f.step(t, 6, 100); err != nil {
		t.Fatal(err)
	}
	f.afterPatch = nil
	if err := f.step(t, 9, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-1")["state"] != "Pending" {
		t.Fatal("reply latency compressed the permission interval")
	}
	if err := f.step(t, 11, 100); err != nil {
		t.Fatal(err)
	}
	if f.entry(t, "work-1")["state"] != "Ready" {
		t.Fatal("bounded cooldown did not recover")
	}
}
