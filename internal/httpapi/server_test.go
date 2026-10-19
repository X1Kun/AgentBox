package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

type stub struct {
	called  int
	err     error
	request api.Request
}

func (s *stub) Ready() bool { return true }
func (s *stub) Status() api.StatusReport {
	return api.StatusReport{Mode: "warm", Idle: 3, Total: 3, Ready: true}
}
func (s *stub) Execute(_ context.Context, r api.Request) (api.Result, error) {
	s.called++
	s.request = r
	return api.Result{Status: "succeeded", CleanupComplete: true}, s.err
}

func TestStatusRequiresAuthentication(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		h := Handler(&stub{}, "status-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
		r := httptest.NewRequest("GET", "/v1/status", nil)
		if authenticated {
			r.Header.Set("Authorization", "Bearer status-token")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if authenticated {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"idle":3`) {
				t.Fatalf("status: %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != 401 {
			t.Fatalf("unauthenticated status: %d", w.Code)
		}
	}
}

func TestHTTPBoundary(t *testing.T) {
	const token = "test-token-at-least-thirty-two-characters"
	tests := []struct {
		name, body, auth string
		want             int
	}{
		{"ok", `{"runtime":"python","code":"print(1)"}`, token, 200},
		{"unauthorized", `{}`, "wrong", 401},
		{"unknown_policy", `{"runtime":"python","code":"1","rootfs":"/"}`, token, 400},
		{"two_documents", `{"runtime":"python","code":"1"}{}`, token, 400},
		{"timeout", `{"runtime":"python","code":"1","timeout_ms":30001}`, token, 400},
		{"too_large", `{"code":"` + strings.Repeat("x", api.MaxBodyBytes) + `"}`, token, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &stub{}
			h := Handler(e, token, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r := httptest.NewRequest(http.MethodPost, "/v1/execute", strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer "+tt.auth)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status %d want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.want != 200 && e.called != 0 {
				t.Fatal("rejected request reached executor")
			}
			if tt.want == 200 && e.request.TimeoutMS != api.DefaultTimeoutMS {
				t.Fatal("missing server default")
			}
		})
	}
}

func TestCapacityRejectedWithoutRetrying(t *testing.T) {
	e := &stub{err: sandbox.ErrBusy}
	h := Handler(e, "token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{"runtime":"python","code":"1"}`))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || e.called != 1 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("unexpected admission result: %d calls=%d", w.Code, e.called)
	}
}

func TestAdmissionResponseKeepsWaitAndCause(t *testing.T) {
	e := &stub{err: &sandbox.AdmissionError{Reason: "queue_timeout", Cause: sandbox.ErrBusy, Queued: true, QueueWait: 25 * time.Millisecond}}
	h := Handler(e, "token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{"runtime":"python","code":"1","request_id":"request-1"}`))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var report api.UnavailableResponse
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || e.called != 1 || report.Reason != "queue_timeout" || report.QueueMS == nil || *report.QueueMS != 25 || report.RequestID != "request-1" {
		t.Fatalf("admission report: %d %+v", w.Code, report)
	}
}
