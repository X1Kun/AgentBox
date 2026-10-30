package sessionapi

import (
	"context"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const readinessInterval = 2 * time.Second
const readinessMaxAge = 5 * time.Second

type readinessResult struct {
	healthy   bool
	checkedAt time.Time
}

// RunReadiness runs at most one serial monitor per server. The caller owns its
// context and must cancel it on shutdown. Probes only read the bounded-age
// snapshot: their request volume cannot amplify Kubernetes traffic.
func (s *Server) RunReadiness(ctx context.Context) {
	if !s.readinessStarted.CompareAndSwap(false, true) {
		return
	}
	defer s.readiness.Store(nil)
	ticker := time.NewTicker(readinessInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.checkReadiness(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
	s.readiness.Store(&readinessResult{healthy: err == nil, checkedAt: started})
}

func (s *Server) isReady() bool {
	r := s.readiness.Load()
	if r == nil || !r.healthy {
		return false
	}
	age := s.now().Sub(r.checkedAt)
	return age >= 0 && age < readinessMaxAge
}
