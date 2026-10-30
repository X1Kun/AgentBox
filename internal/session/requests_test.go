package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kt "k8s.io/client-go/testing"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// Model the real adapter's two identity checks, one before TCP establishment
// and one after it. Count application client calls, not wire retries or probes.
type countedRuntime struct{}

func (countedRuntime) validate(ctx context.Context, target Target) error {
	for i := 0; i < 2; i++ {
		if err := target.Validate(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (r countedRuntime) Execute(ctx context.Context, target Target, _ string) (Execution, error) {
	return Execution{Stdout: "ok"}, r.validate(ctx, target)
}
func (r countedRuntime) Write(ctx context.Context, target Target, _ string, _ []byte) error {
	return r.validate(ctx, target)
}
func (r countedRuntime) Read(ctx context.Context, target Target, _ string) ([]byte, error) {
	return []byte("ok"), r.validate(ctx, target)
}

func requestCounts(h *harness) map[string]int {
	counts := map[string]int{}
	for _, a := range h.client.Actions() {
		counts[a.GetVerb()+"/"+a.GetResource().Resource]++
	}
	return counts
}

func TestSessionRequestBudgets(t *testing.T) {
	results := map[string]map[string]int{}
	record := func(name string, h *harness, want int) {
		t.Helper()
		counts := requestCounts(h)
		results[name] = counts
		total := 0
		for _, n := range counts {
			total += n
		}
		t.Logf("%s: total=%d requests=%v", name, total, counts)
		if total != want {
			t.Errorf("%s: total=%d, want %d", name, total, want)
		}
	}
	h := setup(t)
	h.client.ClearActions()
	v := h.create(t)
	record("create", h, 4)
	h.client.ClearActions()
	if _, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "request-1"}); err != nil {
		t.Fatal(err)
	}
	record("replay_creating", h, 2)
	h.client.ClearActions()
	if _, err := h.service.Get(testCtx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	record("get_creating", h, 1)
	h.ready(t, v.ID)
	h.client.ClearActions()
	if _, err := h.service.Get(testCtx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	record("first_ready", h, 5)
	h.client.ClearActions()
	if _, err := h.service.Get(testCtx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	record("get_ready", h, 4)
	h.client.ClearActions()
	if _, err := h.service.Close(testCtx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	record("close_pending", h, 3)
	c := h.claim(t, v.ID)
	conditions(c, "False", ext.ClaimExpiredReason)
	h.put(t, Claims, c)
	for resource, name := range map[string]string{"sandboxes": "sandbox", "pods": "pod", "services": "service"} {
		g := Sandboxes
		if resource == "pods" {
			g = Pods
		} else if resource == "services" {
			g = Services
		}
		if err := h.client.Tracker().Delete(g, "work", name); err != nil {
			t.Fatal(err)
		}
	}
	h.client.ClearActions()
	if v, err := h.service.Get(testCtx, "alice", v.ID); err != nil || !v.CleanupComplete {
		t.Fatalf("cleanup: %#v %v", v, err)
	}
	record("first_closed", h, 5)
	h.client.ClearActions()
	if v, err := h.service.Get(testCtx, "alice", v.ID); err != nil || !v.CleanupComplete || v.ClosedAt == "" {
		t.Fatalf("retained closure: %#v %v", v, err)
	}
	record("get_closed", h, 4)
	for _, operation := range []string{"execute", "read", "write"} {
		h, v, _ := operationSetup(t)
		h.service.SetRuntime(countedRuntime{})
		h.client.ClearActions()
		var err error
		switch operation {
		case "execute":
			_, err = h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "print(1)"})
		case "read":
			_, err = h.service.Read(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "a"})
		case "write":
			_, err = h.service.Write(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "a", Content: []byte("ok")})
		}
		if err != nil {
			t.Fatal(err)
		}
		record(operation, h, 23)
	}
	// Optional artifact output allows before/after evidence from this same test.
	if directory := os.Getenv("AGENTBOX_REQUEST_COUNTS_OUTPUT"); directory != "" {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "requests.json"), append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotPreservesBackingIntegrityChecks(t *testing.T) {
	for _, change := range []string{"duplicate_pod", "duplicate_service", "selector_fanout", "runtime_class", "host_mount", "service_uid"} {
		t.Run(change, func(t *testing.T) {
			h, v, r := operationSetup(t)
			pod, err := h.client.Resource(Pods).Namespace("work").Get(testCtx, "pod", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			svc, err := h.client.Resource(Services).Namespace("work").Get(testCtx, "service", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := "integrity_error"
			switch change {
			case "duplicate_pod":
				pod.SetName("duplicate")
				pod.SetUID("other-pod")
				h.put(t, Pods, pod)
			case "duplicate_service":
				svc.SetName("duplicate")
				svc.SetUID("other-service")
				h.put(t, Services, svc)
			case "selector_fanout":
				pod.SetName("foreign")
				pod.SetUID("foreign-pod")
				pod.SetOwnerReferences(nil)
				h.put(t, Pods, pod)
			case "runtime_class":
				_ = unstructured.SetNestedField(pod.Object, "other-runtime", "spec", "runtimeClassName")
				h.put(t, Pods, pod)
			case "host_mount":
				_ = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}}, "spec", "volumes")
				h.put(t, Pods, pod)
			case "service_uid":
				svc.SetUID("replacement-service")
				h.put(t, Services, svc)
				want = "session_not_ready"
			}
			_, err = h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "must_not_run()"})
			if Code(err) != want {
				t.Fatalf("%s accepted: %v", change, err)
			}
			if r.calls.Load() != 0 {
				t.Fatal("invalid backing reached runtime")
			}
		})
	}
}

func TestClaimFenceAfterBackingReads(t *testing.T) {
	for _, change := range []string{"close", "holder", "invalidated"} {
		t.Run(change, func(t *testing.T) {
			h, v, _ := operationSetup(t)
			scans := 0
			h.client.PrependReactor("list", "services", func(kt.Action) (bool, runtime.Object, error) {
				scans++
				if scans == 3 { // Mutate during the post-connect observation.
					obj, err := h.client.Tracker().Get(Claims, "work", strings.Split(v.ID, ".")[0])
					if err != nil {
						t.Fatal(err)
					}
					c := obj.(*unstructured.Unstructured).DeepCopy()
					a := c.GetAnnotations()
					switch change {
					case "close":
						a[closeKey] = "true"
						_ = unstructured.SetNestedField(c.Object, h.now.UTC().Format(time.RFC3339), "spec", "lifecycle", "shutdownTime")
					case "holder":
						a[operationKey] = "other-holder"
					case "invalidated":
						a[invalidKey] = "backing_identity_changed"
					}
					c.SetAnnotations(a)
					h.put(t, Claims, c)
				}
				return false, nil, nil
			})
			r := &boundaryRuntime{change: func(int) {}}
			h.service.SetRuntime(r)
			_, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "must_not_run()"})
			if Code(err) != "operation_outcome_unknown" || r.dispatched != 0 {
				t.Fatalf("late Claim change bypassed fence: %v dispatch=%d", err, r.dispatched)
			}
		})
	}
}

type boundaryRuntime struct {
	countedRuntime
	change     func(int)
	dispatched int
}

func (r *boundaryRuntime) Execute(ctx context.Context, target Target, _ string) (Execution, error) {
	for i := 0; i < 2; i++ {
		r.change(i)
		if err := target.Validate(ctx); err != nil {
			return Execution{}, err
		}
	}
	r.dispatched++
	r.change(2)
	return Execution{Stdout: "ok"}, nil
}

func TestOperationSnapshotIsFreshAtEveryBoundary(t *testing.T) {
	for _, point := range []int{0, 1, 2} {
		for _, change := range []string{"pod_uid", "close", "expiry", "holder", "pod_ip"} {
			// IP is pinned by the connection adapter; finish checks resource
			// identity, while an established connection does not retarget.
			if point == 2 && change == "pod_ip" {
				continue
			}
			t.Run(fmt.Sprintf("%d/%s", point, change), func(t *testing.T) {
				h, v, _ := operationSetup(t)
				r := &boundaryRuntime{change: func(at int) {
					if at != point {
						return
					}
					switch change {
					case "pod_uid", "pod_ip":
						obj, err := h.client.Tracker().Get(Pods, "work", "pod")
						if err != nil {
							t.Fatal(err)
						}
						pod := obj.(*unstructured.Unstructured).DeepCopy()
						if change == "pod_uid" {
							pod.SetUID("replacement")
						} else {
							_ = unstructured.SetNestedField(pod.Object, "10.0.0.99", "status", "podIP")
						}
						h.put(t, Pods, pod)
					case "close":
						if _, err := h.service.Close(testCtx, "alice", v.ID); err != nil {
							t.Fatal(err)
						}
					case "expiry":
						h.now = h.now.Add(time.Hour)
					case "holder":
						c := h.claim(t, v.ID)
						a := c.GetAnnotations()
						a[operationKey] = "other-holder"
						c.SetAnnotations(a)
						h.put(t, Claims, c)
					}
				}}
				h.service.SetRuntime(r)
				_, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "side_effect()"})
				if Code(err) != "operation_outcome_unknown" {
					t.Fatalf("boundary change accepted: %v", err)
				}
				want := 0
				if point == 2 {
					want = 1
				}
				if r.dispatched != want {
					t.Fatalf("dispatched=%d, want %d", r.dispatched, want)
				}
				c := h.claim(t, v.ID)
				b, err := readBinding(c)
				if err != nil || revision(b) != v.IdentityRevision {
					t.Fatal("original binding was replaced")
				}
			})
		}
	}
}

func TestBindingObservationIsPublishedOnlyAfterCAS(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			h := setup(t)
			v := h.create(t)
			h.ready(t, v.ID)
			first := true
			h.client.PrependReactor("patch", "sandboxclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if !first {
					return false, nil, nil
				}
				first = false
				if applied {
					handled, _, err := kt.ObjectReaction(h.client.Tracker())(a)
					if !handled || err != nil {
						t.Fatalf("apply binding: %v", err)
					}
				}
				return true, nil, io.ErrUnexpectedEOF
			})
			view, err := h.service.Get(testCtx, "alice", v.ID)
			if err == nil || view.State == "ready" || view.IdentityRevision != "" {
				t.Fatal("unconfirmed binding was published")
			}
			view, err = h.restart(t).Get(testCtx, "alice", v.ID)
			if err != nil || view.State != "ready" || view.IdentityRevision == "" {
				t.Fatalf("binding recovery: %#v %v", view, err)
			}
			b, err := readBinding(h.claim(t, v.ID))
			if err != nil || revision(b) != view.IdentityRevision {
				t.Fatal("published revision differs from persisted binding")
			}
		})
	}
}
