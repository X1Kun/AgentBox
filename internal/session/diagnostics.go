package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Record categories only: native transport/runtime errors can contain user
// code, file contents, endpoints or credentials. The CAS token is never logged.
func (s *Service) operationEvent(ctx context.Context, id, token, phase, outcome string, err error, started time.Time) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	level := slog.LevelInfo
	code, reason := "", ""
	if err != nil {
		level = slog.LevelWarn
		code, reason = Code(err), string(apierrors.ReasonForError(err))
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		} else if errors.Is(err, context.Canceled) {
			reason = "canceled"
		}
	}
	operationID := ""
	if token != "" {
		operationID = hash(token)[:16]
	}
	logger.Log(ctx, level, "session operation", "session_id", id, "operation_id", operationID,
		"phase", phase, "outcome", outcome, "error_code", code, "reason", reason,
		"duration_ms", time.Since(started).Milliseconds())
}

func (s *Service) requestOperationClose(ctx context.Context, principal, id, token string) {
	started := time.Now()
	v, err := s.Close(ctx, principal, id)
	outcome := "pending"
	if err != nil {
		outcome = "request_failed"
	} else if v.State == "closed" && v.CleanupComplete {
		outcome = "complete"
	}
	s.operationEvent(ctx, id, token, "close", outcome, err, started)
}
