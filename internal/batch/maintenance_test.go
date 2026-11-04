package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	kt "k8s.io/client-go/testing"
)

func inactiveQueue(t *testing.T, h *harness) {
	t.Helper()
	q, err := h.client.Resource(Queues).Namespace(h.cfg.Namespace).Get(ctx, "batch", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(q.Object, []interface{}{map[string]interface{}{"type": "Active", "status": "False"}}, "status", "conditions")
	if _, err = h.client.Resource(Queues).Namespace(h.cfg.Namespace).Update(ctx, q, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
func TestInactiveQueueStillPublishesRetriesCancellationAndExpiresData(t *testing.T) {
	h := setup(t)
	publication := h.submit("background-publication")
	cancelled := h.submit("background-cancel")
	h.complete(publication.ID, false)
	failedDelete := false
	h.client.PrependReactor("delete", "jobs", func(a kt.Action) (bool, runtime.Object, error) {
		if !failedDelete && a.(kt.DeleteAction).GetName() == strings.Split(cancelled.ID, ".")[0] {
			failedDelete = true
			return true, nil, errors.New("temporary deletion failure")
		}
		return false, nil, nil
	})
	if _, err := h.service.Cancel(ctx, "alice", cancelled.ID); err == nil {
		t.Fatal("expected interrupted cancel")
	}
	inactiveQueue(t, h)
	if Code(h.service.Ready(ctx)) != "queue_unavailable" {
		t.Fatal("queue should reject new submissions")
	}
	for range 2 {
		if err := h.service.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r := h.record(publication.ID); r.State != "succeeded" || len(r.Artifacts) != 8 {
		t.Fatal("publication stopped with admission", r)
	}
	if r := h.record(cancelled.ID); r.State != "cancelled" {
		t.Fatal("persisted cancel did not converge", r.State)
	}
	if h.job(publication.ID).Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("published Job has no TTL")
	}
	_ = h.client.Resource(Jobs).Namespace(h.cfg.Namespace).Delete(ctx, strings.Split(publication.ID, ".")[0], metav1.DeleteOptions{})
	_ = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Delete(ctx, "pod-success", metav1.DeleteOptions{})
	h.service.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := h.service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.store.root.Stat("published/" + strings.Split(publication.ID, ".")[0]); !os.IsNotExist(err) {
		t.Fatal("expired data retained", err)
	}
}
func TestMaintenanceRefusesReplacedOrUnboundInstallation(t *testing.T) {
	for _, replacement := range []string{"queue", "pvc", "storage", "binding"} {
		t.Run(replacement, func(t *testing.T) {
			h := setup(t)
			v := h.submit("identity")
			h.complete(v.ID, false)
			inactiveQueue(t, h)
			if replacement == "binding" {
				_ = h.service.store.root.Remove("installation.json")
			} else {
				gvr, name := PVCs, "batch-artifacts"
				if replacement == "queue" {
					gvr, name = Queues, "batch"
				}
				o, _ := h.client.Resource(gvr).Namespace(h.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
				if replacement == "storage" {
					_ = unstructured.SetNestedField(o.Object, "Lost", "status", "phase")
				} else {
					o.SetUID("replacement")
				}
				_, _ = h.client.Resource(gvr).Namespace(h.cfg.Namespace).Update(ctx, o, metav1.UpdateOptions{})
			}
			if err := h.service.Reconcile(ctx); err == nil {
				t.Fatal("unsafe maintenance was accepted")
			}
			if h.record(v.ID).State == "succeeded" {
				t.Fatal("identity failure published a result")
			}
		})
	}
}

// Block outside the fake client's global reactor mutex, as an actual slow API
// call would; unrelated requests must remain free to access Kubernetes.
type slowDynamic struct {
	dynamic.Interface
	name             string
	entered, release chan struct{}
}
type slowNamespace struct {
	dynamic.NamespaceableResourceInterface
	parent *slowDynamic
}
type slowResource struct {
	dynamic.ResourceInterface
	parent *slowDynamic
}

func (s *slowDynamic) Resource(g schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	r := s.Interface.Resource(g)
	if g == Jobs {
		return &slowNamespace{r, s}
	}
	return r
}
func (s *slowNamespace) Namespace(ns string) dynamic.ResourceInterface {
	return &slowResource{s.NamespaceableResourceInterface.Namespace(ns), s.parent}
}
func (s *slowResource) Create(ctx context.Context, o *unstructured.Unstructured, opts metav1.CreateOptions, sub ...string) (*unstructured.Unstructured, error) {
	if o.GetName() == s.parent.name {
		close(s.parent.entered)
		select {
		case <-s.parent.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ResourceInterface.Create(ctx, o, opts, sub...)
}
func TestSlowSubmitDoesNotHoldCatalogOrBlockOtherRequests(t *testing.T) {
	h := setup(t)
	blocked := "bat-" + hash([]byte(h.cfg.Instance + "\x00alice\x00slow"))[:40]
	slow := &slowDynamic{Interface: h.client, name: blocked, entered: make(chan struct{}), release: make(chan struct{})}
	h.service.client = slow
	done := make(chan error, 1)
	go func() {
		_, _, err := h.service.Submit(ctx, "alice", Request{RequestID: "slow", DatasetID: "tiny-v1"})
		done <- err
	}()
	defer func() {
		close(slow.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("slow submit did not enter Kubernetes")
	}
	// The test blocks the first call until this function returns. Allow local
	// durable writes time under concurrent race tests; the old catalog lock
	// would prevent the independent request from finishing for any such budget.
	budget, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := h.service.Submit(budget, "bob", Request{RequestID: "independent", DatasetID: "tiny-v1"}); err != nil {
		t.Fatal("unrelated submit blocked", err)
	}
	if _, _, err := h.service.Submit(budget, "alice", Request{RequestID: "slow", DatasetID: "tiny-v1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("same record did not serialize", err)
	}
}

func TestGoneChecksEveryWorkloadPageAndFailsClosed(t *testing.T) {
	for _, kind := range []string{"absent", "related", "failure", "repeated"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			v := h.submit("pages")
			r := h.record(v.ID)
			_ = h.client.Resource(Jobs).Namespace(h.cfg.Namespace).Delete(ctx, r.Name, metav1.DeleteOptions{})
			calls := 0
			h.client.PrependReactor("list", "workloads", func(a kt.Action) (bool, runtime.Object, error) {
				calls++
				opts := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
				out := &unstructured.UnstructuredList{}
				if calls == 1 {
					out.Items = make([]unstructured.Unstructured, 100)
					out.SetContinue("second")
					return true, out, nil
				}
				if opts.Continue != "second" {
					t.Error("continuation lost")
				}
				if kind == "failure" {
					return true, nil, apierrors.NewForbidden(Workloads.GroupResource(), "", errors.New("denied"))
				}
				if kind == "repeated" {
					out.SetContinue("second")
				}
				if kind == "related" {
					controller := true
					o := unstructured.Unstructured{}
					o.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Job", UID: types.UID(r.JobUID), Controller: &controller}})
					out.Items = append(out.Items, o)
				}
				return true, out, nil
			})
			if got := h.service.gone(ctx, r); got != (kind == "absent") {
				t.Fatal("wrong cleanup confirmation", got)
			}
			if calls != 2 {
				t.Fatal("unexpected page count", calls)
			}
		})
	}
}

func TestParallelNewReservationsCannotOversellRecordCapacity(t *testing.T) {
	h := setup(t)
	h.service.cfg.MaxRecords = 2
	results := make(chan error, 8)
	for n := range 8 {
		go func() {
			_, _, err := h.service.Submit(ctx, "alice", Request{RequestID: fmt.Sprintf("capacity-%d", n), DatasetID: "tiny-v1"})
			results <- err
		}()
	}
	accepted := 0
	for range 8 {
		err := <-results
		if err == nil {
			accepted++
		} else if Code(err) != "record_capacity" {
			t.Fatal(err)
		}
	}
	names, err := h.service.store.names()
	if err != nil || accepted != 2 || len(names) != 2 || h.created.Load() != 2 {
		t.Fatal("capacity reservation was not atomic", accepted, len(names), h.created.Load(), err)
	}
	dir, err := h.service.store.root.Open("locks")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	locks, err := dir.ReadDir(-1)
	if err != nil || len(locks) != 4 {
		t.Fatal("capacity rejection allocated record locks", len(locks), err)
	}
}
