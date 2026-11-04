package batchapi

import (
	"context"
	"log/slog"
	"time"

	batch "github.com/X1Kun/AgentBox/internal/batchprotocol"
)

func (s *Server) RunReadiness(ctx context.Context) {
	s.readiness.Run(ctx, s.checkReadiness, s.metrics.InvalidateBackend)
}
func (s *Server) checkReadiness(ctx context.Context) {
	started := s.now()
	check, cancel := context.WithTimeout(ctx, min(s.timeout, 2*time.Second))
	defer cancel()
	err := s.backend.Ready(check)
	if err == nil {
		err = check.Err()
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "batch readiness unavailable", "code", batch.Code(err))
	}
	s.readiness.Store(err == nil, started)
	s.metrics.Backend(err == nil, started)
}
