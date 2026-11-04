package batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kt "k8s.io/client-go/testing"
)

func TestRestartReclaimsInterruptedRecordCreationAndContinuesSubmitting(t *testing.T) {
	h := setup(t)
	// Submit establishes installation identity before its initial atomic write.
	if err := h.service.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	name := "bat-" + strings.Repeat("f", 40)
	// This is atomicWrite's on-disk state before the initial record rename.
	path := "records/" + name + ".json." + strings.Repeat("b", 32) + ".tmp"
	if err := h.service.store.root.WriteFile(path, []byte(`{"schema_version":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	h.service, err = New(h.client, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.service.Reconcile(ctx); err != nil {
		t.Fatalf("orphan prevents restart recovery: %v", err)
	}
	if _, err = h.service.store.root.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("orphan was not reclaimed: %v", err)
	}
	_ = h.submit("after-restart")
}

func TestCatalogReclaimsActualTempAtMaximumRecordCount(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 1024; i++ {
		if err = store.root.WriteFile(fmt.Sprintf("records/bat-%040x.json", i), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := "records/bat-" + strings.Repeat("0", 40) + ".json." + strings.Repeat("a", 32) + ".tmp"
	if err = store.root.WriteFile(path, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	names, err := store.names()
	if err != nil || len(names) != 1024 {
		t.Fatalf("temporary consumed record capacity: count=%d err=%v", len(names), err)
	}
	if _, err = store.root.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan remains", err)
	}
	if err = store.root.WriteFile(fmt.Sprintf("records/bat-%040x.json", 1024), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.names(); Code(err) != "record_capacity" {
		t.Fatal("real catalog limit was lost", err)
	}
}

func TestReconcileRotatesPastSlowRecordsAndPublishesWithoutClientPolling(t *testing.T) {
	h := setup(t)
	views := map[string]View{}
	for i := 0; i < 5; i++ {
		v := h.submit(fmt.Sprintf("background-%d", i))
		views[strings.Split(v.ID, ".")[0]] = v
	}
	names, err := h.service.store.names()
	if err != nil {
		t.Fatal(err)
	}
	tail := names[len(names)-1]
	h.complete(views[tail].ID, false)
	var pass context.Context
	var cancel context.CancelFunc
	// Deterministically consume an entire pass on any retained prefix record.
	// The target's own reads and TTL patch fit within a fresh pass.
	h.client.PrependReactor("get", "jobs", func(action kt.Action) (bool, runtime.Object, error) {
		if pass != nil && action.(kt.GetAction).GetName() != tail {
			cancel()
			return true, nil, pass.Err()
		}
		return false, nil, nil
	})
	for i := 0; i < len(names)+1; i++ {
		pass, cancel = context.WithCancel(ctx)
		err = h.service.Reconcile(pass)
		cancel()
		pass = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	// Inspect durable state directly: no GET/Submit may finish the work for us.
	r := h.record(views[tail].ID)
	if r.State != "succeeded" || len(r.Artifacts) != 8 {
		t.Fatalf("tail result was starved: state=%s artifacts=%d", r.State, len(r.Artifacts))
	}
	job := h.job(views[tail].ID)
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Fatal("background completion did not set the Job TTL")
	}
}

func TestReconcileRetainsProgressAcrossCatalogBudgetsAndExpiresTail(t *testing.T) {
	h := setup(t)
	views := map[string]View{}
	for i := 0; i < 5; i++ {
		v := h.submit(fmt.Sprintf("retained-%d", i))
		views[strings.Split(v.ID, ".")[0]] = v
	}
	names, err := h.service.store.names()
	if err != nil {
		t.Fatal(err)
	}
	tail := names[len(names)-1]
	h.complete(views[tail].ID, false)
	if _, err = h.service.Get(ctx, "alice", views[tail].ID); err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if _, err = h.service.Cancel(ctx, "alice", v.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = h.service.Get(ctx, "alice", v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = h.client.Resource(Pods).Namespace(h.cfg.Namespace).Delete(ctx, "pod-success", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.service.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	var pass context.Context
	var cancel context.CancelFunc
	reads := 0
	// Installation verification takes two reads, followed by four reads for
	// one cleaned record. A second record exhausts this simulated budget.
	h.client.PrependReactor("*", "*", func(action kt.Action) (bool, runtime.Object, error) {
		if pass != nil {
			reads++
			if reads > 6 {
				cancel()
				return true, nil, pass.Err()
			}
		}
		return false, nil, nil
	})
	for i := 0; i < len(names)*2; i++ {
		reads = 0
		pass, cancel = context.WithCancel(ctx)
		err = h.service.Reconcile(pass)
		cancel()
		pass = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err = h.service.store.root.Stat("published/" + tail); !os.IsNotExist(err) {
		t.Fatalf("expired tail artifact was starved: %v", err)
	}
	if _, err = h.service.store.read(tail); err != nil {
		t.Fatalf("retention must preserve the submission record: %v", err)
	}
}
