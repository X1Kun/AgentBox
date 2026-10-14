package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

type Executor interface {
	Execute(context.Context, api.Request) (api.Result, error)
	Ready() bool
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
	wanted := sha256.Sum256([]byte(token))
	mux.HandleFunc("POST /v1/execute", func(w http.ResponseWriter, r *http.Request) {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(provided))
		if !ok || token == "" || subtle.ConstantTimeCompare(wanted[:], got[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			send(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
			send(w, 415, map[string]string{"error": "Content-Type must be application/json"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, api.MaxBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request api.Request
		if err := decoder.Decode(&request); err != nil {
			decodeError(w, err)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			decodeError(w, err)
			return
		}
		request, err := request.Validate()
		if err != nil {
			send(w, 400, map[string]string{"error": err.Error()})
			return
		}
		result, err := executor.Execute(r.Context(), request)
		if errors.Is(err, sandbox.ErrBusy) || errors.Is(err, sandbox.ErrUnhealthy) {
			w.Header().Set("Retry-After", "1")
			send(w, 503, map[string]string{"error": "executor unavailable"})
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
		logger.Info("execution completed", "execution_id", result.ExecutionID, "status", result.Status, "total_ms", result.Timings.TotalMS, "cleanup_complete", result.CleanupComplete)
		send(w, 200, result)
	})
	return mux
}

func decodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		send(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	send(w, 400, map[string]string{"error": "expected one JSON request with known fields"})
}

func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
