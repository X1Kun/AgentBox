package session

import (
	"context"
	"errors"
	"github.com/X1Kun/AgentBox/internal/requestid"
	protocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"log/slog"
	"time"
)

func backendReason(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return "forbidden"
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsTooManyRequests(err):
		return "throttled"
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return "invalid"
	}
	return "unavailable"
}

// beginBackend records identity and categories only, never native errors, CAS
// tokens, manifests, code, file bytes or credential-bearing endpoints.
func (s *Service) beginBackend(ctx context.Context, stage, name string) func(error) {
	started := time.Now()
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	id := requestid.From(ctx)
	if id != "" {
		logger.InfoContext(ctx, "session backend stage", "phase", "start", "stage", stage, "request_id", id, "object_name", name)
	}
	return func(err error) {
		reason := backendReason(err)
		s.metrics.Diagnostic(stage, reason, time.Since(started))
		if id != "" || err != nil {
			logger.InfoContext(ctx, "session backend stage", "phase", "end", "stage", stage, "request_id", id, "object_name", name, "reason", reason, "duration_ms", time.Since(started).Milliseconds())
		}
	}
}
func (s *Service) getResource(ctx context.Context, g schema.GroupVersionResource, name string) (obj *unstructured.Unstructured, err error) {
	stage := "claim_get"
	if g == Pools {
		stage = "pool_get"
	}
	if g == Sandboxes {
		stage = "sandbox_get"
	}
	done := s.beginBackend(ctx, stage, name)
	defer func() { done(err) }()
	obj, err = s.resource(g).Get(ctx, name, metav1.GetOptions{})
	if err == nil && requestid.From(ctx) != "" {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.InfoContext(ctx, "session backend object", "request_id", requestid.From(ctx), "stage", stage, "object_name", name, "uid", string(obj.GetUID()), "resource_version", obj.GetResourceVersion())
	}
	return obj, err
}
func publicBackendCode(err error) string {
	code := protocol.Code(err)
	if code != protocol.CodeBackendUnavailable {
		return code
	}
	switch backendReason(err) {
	case "timeout":
		return protocol.CodeBackendTimeout
	case "forbidden":
		return protocol.CodeBackendForbidden
	case "throttled":
		return protocol.CodeBackendThrottled
	case "conflict":
		return protocol.CodeBackendConflict
	}
	return code
}
