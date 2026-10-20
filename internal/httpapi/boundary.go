package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
)

const DeadlineHeader = "X-AgentBox-Deadline"

func BearerAuthorizer(token string) func(http.ResponseWriter, *http.Request) bool {
	wanted := sha256.Sum256([]byte(token))
	return func(w http.ResponseWriter, r *http.Request) bool {
		provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(provided))
		if !ok || token == "" || subtle.ConstantTimeCompare(wanted[:], got[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
			return false
		}
		return true
	}
}

// ReadRequest validates the protocol but retains the original bounded bytes.
// Forwarding those bytes avoids JSON re-encoding changing the body-size budget.
func ReadRequest(w http.ResponseWriter, r *http.Request) (api.Request, []byte, bool) {
	var request api.Request
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		WriteJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return request, nil, false
	}
	// Context cancellation alone does not bound a blocked server body read.
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(5 * time.Second)
	if parent, ok := r.Context().Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	_ = controller.SetReadDeadline(deadline)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxBodyBytes))
	if err != nil {
		decodeError(w, err)
		return request, nil, false
	}
	// Only a fully consumed body can release the read deadline. On read errors
	// keep it armed so server cleanup cannot block draining a slow body.
	_ = controller.SetReadDeadline(time.Time{})
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		decodeError(w, err)
		return request, nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		decodeError(w, err)
		return request, nil, false
	}
	request, err = request.Validate()
	if err != nil {
		WriteJSON(w, 400, map[string]string{"error": err.Error()})
		return request, nil, false
	}
	return request, raw, true
}

// DeadlineHandler is enabled only on the permission-controlled Unix endpoint.
// The gateway replaces incoming deadline headers with its own bounded deadline.
func DeadlineHandler(next http.Handler, maxBudget time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(maxBudget)
		if value := r.Header.Get(DeadlineHeader); value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				WriteJSON(w, 400, map[string]string{"error": "invalid request deadline"})
				return
			}
			if parsed.Before(deadline) {
				deadline = parsed
			}
		}
		if !deadline.After(time.Now()) {
			WriteJSON(w, 408, map[string]string{"error": "request deadline exceeded", "reason": "request_deadline_exceeded"})
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
