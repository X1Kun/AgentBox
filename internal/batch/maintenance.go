package batch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RunMaintenance is owned by the entry point, independent of HTTP readiness.
// Reconcile validates the original installation before touching existing tasks.
func (s *Service) RunMaintenance(ctx context.Context, budget time.Duration) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		pass, cancel := context.WithTimeout(ctx, budget)
		err := s.Reconcile(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			// Native errors may contain endpoints or user data: log categories only.
			reason := string(apierrors.ReasonForError(err))
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "deadline_exceeded"
			}
			if errors.Is(err, context.Canceled) {
				reason = "canceled"
			}
			slog.WarnContext(ctx, "batch maintenance failed", "code", Code(err), "reason", reason)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
