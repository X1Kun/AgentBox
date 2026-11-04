package httpapi

import (
	"context"
	"github.com/X1Kun/AgentBox/internal/readiness"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const readinessMaxAge = readiness.MaxAge

func (s *Server) RunReadiness(ctx context.Context) {
	s.readiness.Run(ctx, s.checkReadiness, s.metrics.InvalidateBackend)
}

func (s *Server) checkReadiness(ctx context.Context) {
	// Date from the start, so a stalled check cannot refresh a stale success.
	started := s.now()
	check, cancel := context.WithTimeout(ctx, min(s.timeout, 2*time.Second))
	defer cancel()
	err := s.backend.Ready(check)
	if err == nil {
		err = check.Err()
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "lifecycle backend readiness failed", "reason", apierrors.ReasonForError(err))
	}
	s.readiness.Store(err == nil, started)
	s.metrics.Backend(err == nil, started)
}

func (s *Server) isReady() bool { return s.readiness.Ready(s.now()) }
