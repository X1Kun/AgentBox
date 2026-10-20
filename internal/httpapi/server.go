package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

type Executor interface {
	Execute(context.Context, api.Request) (api.Result, error)
	Ready() bool
	Status() api.StatusReport
}

func Handler(executor Executor, token string, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { send(w, 200, map[string]bool{"alive": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := executor.Ready()
		code := 200
		if !ready {
			code = 503
		}
		send(w, code, map[string]bool{"ready": ready})
	})
	authorized := BearerAuthorizer(token)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		send(w, 200, executor.Status())
	})
	mux.HandleFunc("POST /v1/execute", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		request, _, ok := ReadRequest(w, r)
		if !ok {
			return
		}
		result, err := executor.Execute(r.Context(), request)
		if errors.Is(err, sandbox.ErrBusy) || errors.Is(err, sandbox.ErrUnhealthy) {
			w.Header().Set("Retry-After", "1")
			report := api.UnavailableResponse{Error: "executor unavailable", Reason: "executor_unhealthy", RequestID: request.RequestID}
			if errors.Is(err, sandbox.ErrBusy) {
				report.Reason = "capacity_exhausted"
			}
			var admission *sandbox.AdmissionError
			if errors.As(err, &admission) {
				report.Reason = admission.Reason
				if admission.Queued {
					elapsed := float64(admission.QueueWait) / float64(time.Millisecond)
					report.QueueMS = &elapsed
				}
			}
			logger.Info("execution rejected", "request_id", request.RequestID, "reason", report.Reason, "queue_ms", report.QueueMS)
			send(w, 503, report)
			return
		}
		if err != nil {
			logger.Error("execution infrastructure failure", "execution_id", result.ExecutionID, "error", err)
			if result.ExecutionID != "" {
				result.Status = "infrastructure_error"
				send(w, 500, result)
			} else {
				send(w, 500, map[string]string{"error": "execution infrastructure failure"})
			}
			return
		}
		logger.Info("execution completed", "execution_id", result.ExecutionID, "status", result.Status, "queue_ms", result.Timings.QueueMS, "setup_ms", result.Timings.SetupMS, "execution_ms", result.Timings.ExecutionMS, "cleanup_ms", result.Timings.CleanupMS, "total_ms", result.Timings.TotalMS, "cleanup_complete", result.CleanupComplete)
		send(w, 200, result)
	})
	return mux
}

func decodeError(w http.ResponseWriter, err error) {
	var timedOut net.Error
	if errors.As(err, &timedOut) && timedOut.Timeout() {
		send(w, 408, map[string]string{"error": "request body timeout"})
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		send(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	send(w, 400, map[string]string{"error": "expected one JSON request with known fields"})
}

func send(w http.ResponseWriter, status int, value any) {
	WriteJSON(w, status, value)
}
