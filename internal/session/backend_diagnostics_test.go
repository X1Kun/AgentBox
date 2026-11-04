package session

import (
	"context"
	"errors"
	protocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"testing"
)

func TestBackendFailureCategoriesPreservePublicMeaning(t *testing.T) {
	for _, c := range []struct {
		err          error
		reason, code string
	}{
		{context.DeadlineExceeded, "timeout", protocol.CodeBackendTimeout},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "secret-name", errors.New("private details")), "forbidden", protocol.CodeBackendForbidden},
		{apierrors.NewTooManyRequests("private", 1), "throttled", protocol.CodeBackendThrottled},
		{apierrors.NewConflict(schema.GroupResource{Resource: "claims"}, "private", errors.New("private")), "conflict", protocol.CodeBackendConflict},
		{fail(protocol.CodeSessionBusy), "unavailable", protocol.CodeSessionBusy},
	} {
		if backendReason(c.err) != c.reason || Code(c.err) != c.code {
			t.Fatal(c.reason, Code(c.err))
		}
	}
}
