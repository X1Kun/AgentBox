package session

import (
	"context"
	"errors"
	"testing"
	"time"

	protocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kt "k8s.io/client-go/testing"
)

func TestSweepStopsAtEveryReadBarrierForAllBackendCategories(t *testing.T) {
	failures := []struct {
		name string
		err  error
	}{
		{"unavailable", errors.New("private backend detail")},
		{"deadline", context.DeadlineExceeded},
		{"server-timeout", apierrors.NewTimeoutError("private timeout", 1)},
		{"forbidden", apierrors.NewForbidden(Claims.GroupResource(), "private", errors.New("secret"))},
		{"unauthorized", apierrors.NewUnauthorized("private")},
		{"throttled", apierrors.NewTooManyRequests("private", 1)},
		{"conflict", apierrors.NewConflict(Claims.GroupResource(), "private", errors.New("secret"))},
	}
	for _, barrier := range []string{"load", "observe", "reload", "reobserve"} {
		for _, failure := range failures {
			t.Run(barrier+"/"+failure.name, func(t *testing.T) {
				h := setup(t)
				closedRecord(t, h, "alice", "eligible", true)
				h.now = h.now.Add(2 * time.Hour)
				verb, resource, at := "get", "sandboxclaims", 1
				if barrier == "observe" || barrier == "reobserve" {
					verb, resource = "list", "sandboxes"
				}
				if barrier == "reload" || barrier == "reobserve" {
					at = 2
				}
				count := 0
				h.client.PrependReactor(verb, resource, func(kt.Action) (bool, runtime.Object, error) {
					count++
					if count == at {
						return true, nil, failure.err
					}
					return false, nil, nil
				})
				h.client.ClearActions()
				audits := []CleanupReport{}
				report, err := h.service.Sweep(testCtx, "alice", CleanupOptions{Apply: true, Retention: time.Hour}, func(r CleanupReport) error { audits = append(audits, r); return nil })
				if Code(err) != codeCleanupReadFailed || report.Error != codeCleanupReadFailed || !report.Truncated || len(report.Entries) != 1 || report.Entries[0].Outcome != "read_failed" {
					t.Fatalf("silent read failure: %#v %v", report, err)
				}
				if count != at {
					t.Fatalf("continued after failing barrier: %d", count)
				}
				assertNoCleanupMutations(t, h)
				if len(audits) == 0 || audits[len(audits)-1].Error != codeCleanupReadFailed {
					t.Fatal("final audit omitted failure")
				}
			})
		}
	}
}

func TestCleanupReadClassificationPreservesDomainSkips(t *testing.T) {
	for _, code := range []string{protocol.CodeNotFound, protocol.CodeIdentityChanged, protocol.CodeIntegrityError, protocol.CodeStateConflict} {
		if cleanupReadFailed(fail(code)) {
			t.Fatal("domain rejection became backend failure", code)
		}
	}
	for _, code := range []string{protocol.CodeBackendUnavailable, protocol.CodeBackendTimeout, protocol.CodeBackendForbidden, protocol.CodeBackendThrottled, protocol.CodeBackendConflict} {
		if !cleanupReadFailed(fail(code)) {
			t.Fatal("typed backend error ignored", code)
		}
	}
	if cleanupReadFailed(nil) {
		t.Fatal("nil classified as failure")
	}
}
