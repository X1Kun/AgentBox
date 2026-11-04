package sandboxd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/X1Kun/AgentBox/internal/requestid"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type protocolError struct{}

func (*protocolError) Error() string { return "invalid execution stream" }

func runtimeReason(err error) string {
	if err == nil {
		return "ok"
	}
	var protocol *protocolError
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &protocol):
		return "protocol_error"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case session.Code(err) == "execution_output_limit":
		return "output_limit"
	case session.Code(err) != "backend_unavailable":
		return session.Code(err)
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return "timeout"
	case codes.Canceled:
		return "canceled"
	case codes.PermissionDenied, codes.Unauthenticated:
		return "forbidden"
	case codes.ResourceExhausted:
		return "throttled"
	case codes.InvalidArgument:
		return "invalid"
	default:
		return "unavailable"
	}
}

func (r *Runtime) SetTelemetry(metrics *telemetry.Metrics) { r.metrics = metrics }

func (r *Runtime) begin(ctx context.Context, target session.Target, stage string, fields ...any) func(error) {
	if r == nil {
		return func(error) {}
	}
	started := time.Now()
	r.log(ctx, target, stage, "start", nil, 0, fields...)
	return func(err error) { r.end(ctx, target, stage, started, err, fields...) }
}
func (r *Runtime) end(ctx context.Context, target session.Target, stage string, started time.Time, err error, fields ...any) {
	if r == nil {
		return
	}
	elapsed := time.Since(started)
	r.metrics.Diagnostic(stage, runtimeReason(err), elapsed)
	r.log(ctx, target, stage, "end", err, elapsed, fields...)
}
func (r *Runtime) log(ctx context.Context, target session.Target, stage, phase string, err error, elapsed time.Duration, fields ...any) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	remaining := int64(-1)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = max(0, time.Until(deadline).Milliseconds())
	}
	attrs := []any{"request_id", requestid.From(ctx), "session_id", target.SessionID, "pod_uid", target.PodUID,
		"stage", stage, "phase", phase, "reason", runtimeReason(err), "duration_ms", elapsed.Milliseconds(), "remaining_budget_ms", remaining}
	logger.InfoContext(ctx, "sandboxd stage", append(attrs, fields...)...)
}
