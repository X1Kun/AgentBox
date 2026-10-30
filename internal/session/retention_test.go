package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kt "k8s.io/client-go/testing"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

func closedRecord(t *testing.T, h *harness, principal, key string, stamp bool) View {
	t.Helper()
	v, _, err := h.service.Create(testCtx, principal, CreateRequest{RequestID: key})
	if err != nil {
		t.Fatal(err)
	}
	c := h.claim(t, v.ID)
	_ = unstructured.SetNestedField(c.Object, h.now.Add(-time.Hour).Format(time.RFC3339), "spec", "lifecycle", "shutdownTime")
	c.SetGeneration(c.GetGeneration() + 1)
	conditions(c, "False", ext.ClaimExpiredReason)
	h.put(t, Claims, c)
	if stamp {
		v, err = h.service.Get(testCtx, principal, v.ID)
		if err != nil || !v.CleanupComplete {
			t.Fatalf("close: %#v %v", v, err)
		}
	}
	return v
}
func deleteGuard(t *testing.T, h *harness, before func(kt.DeleteAction)) *int {
	t.Helper()
	calls := new(int)
	h.client.PrependReactor("delete", "sandboxclaims", func(a kt.Action) (bool, runtime.Object, error) {
		*calls++
		action := a.(kt.DeleteAction)
		if before != nil {
			before(action)
		}
		obj, err := h.client.Tracker().Get(Claims, "work", action.GetName())
		if err != nil {
			return true, nil, err
		}
		c := obj.(*unstructured.Unstructured)
		p := action.GetDeleteOptions().Preconditions
		if p == nil || p.UID == nil || p.ResourceVersion == nil {
			t.Fatal("delete lacks both identity/version preconditions")
		}
		if *p.UID != c.GetUID() || *p.ResourceVersion != c.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(Claims.GroupResource(), c.GetName(), errors.New("changed"))
		}
		return false, nil, nil
	})
	return calls
}
func auditOK(CleanupReport) error { return nil }
func assertNoCleanupMutations(t *testing.T, h *harness) {
	t.Helper()
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "patch" || a.GetVerb() == "delete" || a.GetVerb() == "create" {
			t.Fatalf("read-only sweep mutated: %s", a.GetVerb())
		}
	}
}

func TestClosureTimestampStartsAtConfirmedCleanupAndSurvivesRestart(t *testing.T) {
	h := setup(t)
	v := closedRecord(t, h, "alice", "old", false)
	first, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || first.ClosedAt != h.now.Format(time.RFC3339Nano) {
		t.Fatalf("timestamp backdated: %#v %v", first, err)
	}
	h.now = h.now.Add(time.Hour)
	view, err := h.restart(t).Get(testCtx, "alice", v.ID)
	if err != nil || view.ClosedAt != first.ClosedAt {
		t.Fatal("restart extended retention")
	}
	// A current expiry acknowledgement cannot hide a remaining owned resource.
	c := h.claim(t, v.ID)
	pod := object("Pod", "leftover", "leftover-uid", "v1")
	pod.SetLabels(map[string]string{ext.SandboxIDLabel: string(c.GetUID())})
	h.put(t, Pods, pod)
	pending, err := h.service.Get(testCtx, "alice", v.ID)
	if err != nil || pending.CleanupComplete {
		t.Fatal("remaining resource counted as cleaned")
	}
}

func TestLegacyPreviewIsReadOnlyAndApplyHonorsFullRetention(t *testing.T) {
	h := setup(t)
	v := closedRecord(t, h, "alice", "legacy", false)
	h.client.ClearActions()
	preview, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Retention: time.Hour}, nil)
	if err != nil || len(preview.Entries) != 1 || preview.Entries[0].Outcome != "would_finalize" {
		t.Fatalf("preview: %#v %v", preview, err)
	}
	assertNoCleanupMutations(t, h)
	if h.claim(t, v.ID).GetAnnotations()[closedAtKey] != "" {
		t.Fatal("preview stamped a legacy record")
	}
	opts := CleanupOptions{Apply: true, Retention: time.Hour}
	r, err := h.service.Sweep(testCtx, "alice", opts, auditOK)
	if err != nil || r.Entries[0].Outcome != "finalized" {
		t.Fatalf("finalize: %#v %v", r, err)
	}
	r, err = h.service.Sweep(testCtx, "alice", opts, auditOK)
	if err != nil || r.Entries[0].Outcome != "retain" {
		t.Fatal("legacy retention was backdated from expiry")
	}
	h.now = h.now.Add(time.Hour)
	calls := deleteGuard(t, h, nil)
	r, err = h.service.Sweep(testCtx, "alice", opts, auditOK)
	if err != nil || r.Entries[0].Outcome != "deleted" || *calls != 1 {
		t.Fatalf("delete: %#v %v", r, err)
	}
	// Metadata removal ends creation deduplication; an old session ID remains fenced.
	newView, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "legacy"})
	if err != nil || newView.ID == v.ID {
		t.Fatal("new Claim incarnation not distinguished")
	}
	if _, err = h.service.Get(testCtx, "alice", v.ID); Code(err) != "identity_changed" {
		t.Fatal("old ID adopted new Claim")
	}
}

func TestCleanupScopeAndActiveRecordsAreProtectedEvenIfListIsUnfiltered(t *testing.T) {
	h := setup(t)
	eligible := closedRecord(t, h, "alice", "eligible", true)
	closedRecord(t, h, "bob", "foreign", true)
	foreignInstance := closedRecord(t, h, "alice", "other-instance", true)
	c := h.claim(t, foreignInstance.ID)
	l := c.GetLabels()
	l[instanceKey] = "other-api"
	c.SetLabels(l)
	h.put(t, Claims, c)
	if _, _, err := h.service.Create(testCtx, "alice", CreateRequest{RequestID: "active"}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	all, err := h.client.Resource(Claims).Namespace("work").List(testCtx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h.client.PrependReactor("list", "sandboxclaims", func(kt.Action) (bool, runtime.Object, error) { return true, all.DeepCopy(), nil })
	calls := deleteGuard(t, h, nil)
	r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Apply: true, Retention: time.Hour}, auditOK)
	if err != nil || *calls != 1 {
		t.Fatalf("scope sweep: %#v %v", r, err)
	}
	for _, entry := range r.Entries {
		if entry.Outcome == "deleted" && entry.ID != eligible.ID {
			t.Fatal("deleted another scope")
		}
	}
}

func TestAuditFailurePreventsMutationAndLateChangesAreRechecked(t *testing.T) {
	for _, change := range []string{"audit_failure", "claim_uid", "resource_version", "new_pod", "clock_backwards"} {
		t.Run(change, func(t *testing.T) {
			h := setup(t)
			v := closedRecord(t, h, "alice", "candidate", true)
			h.now = h.now.Add(time.Hour)
			calls := deleteGuard(t, h, nil)
			h.client.ClearActions()
			audit := func(r CleanupReport) error {
				if len(r.Entries) == 0 || r.Entries[0].Outcome != "delete_planned" {
					return nil
				}
				if change == "audit_failure" {
					return errors.New("private disk failure")
				}
				obj, err := h.client.Tracker().Get(Claims, "work", idPattern.FindStringSubmatch(v.ID)[1])
				if err != nil {
					t.Fatal(err)
				}
				c := obj.(*unstructured.Unstructured).DeepCopy()
				switch change {
				case "claim_uid":
					c.SetUID("replacement")
				case "resource_version":
					c.SetResourceVersion("updated-version")
				case "new_pod":
					p := object("Pod", "new", "new-uid", "v1")
					p.SetLabels(map[string]string{ext.SandboxIDLabel: string(c.GetUID())})
					h.put(t, Pods, p)
				case "clock_backwards":
					h.now = h.now.Add(-time.Hour)
				}
				h.put(t, Claims, c)
				return nil
			}
			r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Apply: true, Retention: time.Hour}, audit)
			if change == "audit_failure" && Code(err) != "audit_failed" {
				t.Fatalf("audit failure hidden: %v", err)
			}
			if *calls != 0 {
				t.Fatal("late change bypassed revalidation")
			}
			assertNoCleanupMutations(t, h)
			data, _ := json.Marshal(r)
			if bytes.Contains(data, []byte("private disk")) {
				t.Fatal("raw error leaked")
			}
		})
	}
}

func TestDeletePreconditionsAndLostReplyNeverRetargetOrReplay(t *testing.T) {
	for _, change := range []string{"replace_at_delete", "lost_reply"} {
		t.Run(change, func(t *testing.T) {
			h := setup(t)
			closedRecord(t, h, "alice", "one", true)
			closedRecord(t, h, "alice", "two", true)
			h.now = h.now.Add(time.Hour)
			calls := deleteGuard(t, h, func(a kt.DeleteAction) {
				if change == "replace_at_delete" {
					obj, err := h.client.Tracker().Get(Claims, "work", a.GetName())
					if err != nil {
						t.Fatal(err)
					}
					c := obj.(*unstructured.Unstructured).DeepCopy()
					c.SetUID("new-uid")
					c.SetResourceVersion("new-rv")
					h.put(t, Claims, c)
				}
			})
			if change == "lost_reply" {
				h.client.PrependReactor("delete", "sandboxclaims", func(a kt.Action) (bool, runtime.Object, error) {
					*calls++
					_, _, err := kt.ObjectReaction(h.client.Tracker())(a)
					if err != nil {
						t.Fatal(err)
					}
					return true, nil, io.ErrUnexpectedEOF
				})
			}
			r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Apply: true, Retention: time.Hour}, auditOK)
			if change == "lost_reply" {
				if Code(err) != "cleanup_outcome_unknown" || *calls != 1 || r.Entries[len(r.Entries)-1].Outcome != "delete_outcome_unknown" {
					t.Fatalf("lost delete replayed: %#v %v", r, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range r.Entries {
					if e.Outcome != "skip_changed_or_unowned" {
						t.Fatalf("replacement adopted: %#v", e)
					}
				}
			}
		})
	}
}

func TestQuotaPressureRequiresObservedIntegerCounts(t *testing.T) {
	for _, tc := range []struct{ used, observed, expected string }{{"0", "32", "normal"}, {"26", "32", "warning"}, {"32", "32", "exhausted"}, {"-1", "32", "unknown"}, {"1", "16", "unknown"}, {"0.5", "32", "unknown"}} {
		t.Run(tc.used+"/"+tc.observed, func(t *testing.T) {
			h := setup(t)
			q := object("ResourceQuota", "budget", "quota-uid", "v1")
			q.Object["spec"] = map[string]any{"hard": map[string]any{claimQuotaKey: "32"}}
			q.Object["status"] = map[string]any{"hard": map[string]any{claimQuotaKey: tc.observed}, "used": map[string]any{claimQuotaKey: tc.used}}
			h.put(t, ResourceQuotas, q)
			r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{QuotaName: "budget"}, nil)
			if err != nil || r.Quota.Pressure != tc.expected || r.Quota.Known != (tc.expected != "unknown") {
				t.Fatalf("quota: %#v %v", r.Quota, err)
			}
		})
	}
}

func TestCleanupRejectsInvalidTimestampsAndUnauditedApply(t *testing.T) {
	h := setup(t)
	v := closedRecord(t, h, "alice", "bad-time", true)
	for _, at := range []string{"not-a-time", h.now.Add(time.Hour).Format(time.RFC3339Nano)} {
		c := h.claim(t, v.ID)
		a := c.GetAnnotations()
		a[closedAtKey] = at
		c.SetAnnotations(a)
		h.put(t, Claims, c)
		r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{}, nil)
		if err != nil || r.Entries[0].Outcome != "skip_unverifiable" {
			t.Fatal("invalid timestamp eligible")
		}
	}
	if _, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Apply: true}, nil); Code(err) != "invalid_cleanup_configuration" {
		t.Fatal("unaudited apply accepted")
	}
}

func TestCleanupHonorsRecordLimitAndCancellationBeforeDispatch(t *testing.T) {
	h := setup(t)
	for _, name := range []string{"one", "two", "three"} {
		closedRecord(t, h, "alice", name, true)
	}
	h.now = h.now.Add(time.Hour)
	h.client.ClearActions()
	r, err := h.service.Sweep(testCtx, "alice", CleanupOptions{MaxRecords: 2, Retention: time.Hour}, nil)
	if err != nil || len(r.Entries) != 2 || !r.Truncated {
		t.Fatalf("record limit: %#v %v", r, err)
	}
	assertNoCleanupMutations(t, h)
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	h.client.ClearActions()
	r, err = h.service.Sweep(ctx, "alice", CleanupOptions{Apply: true, Retention: time.Hour}, func(r CleanupReport) error {
		if len(r.Entries) > 0 && r.Entries[0].Outcome == "delete_planned" {
			cancel()
		}
		return nil
	})
	if Code(err) != "cleanup_timeout" || r.Entries[0].Outcome != "cancelled" {
		t.Fatalf("cancellation: %#v %v", r, err)
	}
	assertNoCleanupMutations(t, h)
}
