package sessionapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/session"
)

const aliceToken = "alice-secret-012345678901234567890123456789"
const bobToken = "bob-secret-01234567890123456789012345678901"

type backend struct {
	calls     atomic.Int32
	principal string
	request   session.CreateRequest
	err       error
	block     chan struct{}
}

func (b *backend) Create(ctx context.Context, p string, r session.CreateRequest) (session.View, bool, error) {
	b.calls.Add(1)
	b.principal = p
	b.request = r
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
			return session.View{}, false, ctx.Err()
		}
	}
	return session.View{ID: "session", State: "creating"}, false, b.err
}
func (b *backend) Get(context.Context, string, string) (session.View, error) {
	b.calls.Add(1)
	return session.View{State: "invalidated"}, b.err
}
func (b *backend) Close(context.Context, string, string) (session.View, error) {
	b.calls.Add(1)
	return session.View{State: "closing"}, b.err
}
func (b *backend) Ready(context.Context) error { return b.err }
func handler(t *testing.T, b Backend, max int) *Server {
	t.Helper()
	h, e := New(b, []Principal{{"alice", aliceToken}, {"bob", bobToken}}, time.Second, max)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func call(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestHTTPUsesConfiguredPrincipalAndAcceptedCreation(t *testing.T) {
	b := &backend{}
	h := handler(t, b, 2)
	w := call(h, "POST", "/v2/sessions", aliceToken, `{"request_id":"abc","ttl_seconds":120}`)
	if w.Code != 202 || b.principal != "alice" || b.request.RequestID != "abc" || b.request.TTLSeconds != 120 {
		t.Fatalf("bad routing: %d %#v", w.Code, b.request)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("response can be cached")
	}
}
func TestAuthenticationAndInputRejectBeforeBackend(t *testing.T) {
	b := &backend{}
	h := handler(t, b, 2)
	for _, tc := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"POST", "/v2/sessions", "", `{}`, 401}, {"POST", "/v2/sessions", "wrong", `{}`, 401},
		{"POST", "/v2/sessions", aliceToken, `{"request_id":"a","request_id":"b"}`, 400},
		{"POST", "/v2/sessions", aliceToken, `{"request_id":"a","image":"arbitrary"}`, 400},
		{"POST", "/v2/sessions", aliceToken, `{"request_id":"a"} {}`, 400},
		{"POST", "/v2/sessions?pool=other", aliceToken, `{}`, 400},
		{"POST", "/v2/sessions", aliceToken, strings.Repeat("a", 4097), 413},
		{"GET", "/v2/sessions/id", aliceToken, `{}`, 400},
		{"POST", "/v1/execute", aliceToken, `{}`, 404},
	} {
		t.Run(tc.method+tc.path+tc.body[:min(len(tc.body), 30)], func(t *testing.T) {
			w := call(h, tc.method, tc.path, tc.token, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status=%d expected=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if b.calls.Load() != 0 {
		t.Fatal("rejected request reached backend")
	}
	r := httptest.NewRequest("POST", "/v2/sessions", strings.NewReader(`{}`))
	r.Header.Add("Authorization", "Bearer "+aliceToken)
	r.Header.Add("Authorization", "Bearer "+bobToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("multiple credentials accepted")
	}
}
func TestBackendDetailsAreNeverExposed(t *testing.T) {
	b := &backend{err: errors.New("private-kubernetes-address bearer-secret")}
	h := handler(t, b, 2)
	w := call(h, "GET", "/v2/sessions/id", aliceToken, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("backend detail escaped: %s", w.Body.String())
	}
}
func TestClosingResponseDoesNotClaimCleanup(t *testing.T) {
	b := &backend{}
	h := handler(t, b, 2)
	w := call(h, "DELETE", "/v2/sessions/id", aliceToken, "")
	var v session.View
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || w.Code != 202 || v.State != "closing" || v.CleanupComplete {
		t.Fatal("close reported premature success")
	}
}
func TestCapacityRejectsImmediatelyWithoutQueue(t *testing.T) {
	b := &backend{block: make(chan struct{})}
	h := handler(t, b, 1)
	done := make(chan struct{})
	go func() { call(h, "POST", "/v2/sessions", aliceToken, `{"request_id":"a"}`); close(done) }()
	until := time.Now().Add(time.Second)
	for b.calls.Load() == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	w := call(h, "POST", "/v2/sessions", bobToken, `{"request_id":"b"}`)
	close(b.block)
	<-done
	if w.Code != 503 || !strings.Contains(w.Body.String(), "api_capacity") || b.calls.Load() != 1 {
		t.Fatal("capacity overflow queued or reached backend")
	}
}
func TestReadinessIsNotCapacityOrSessionReadiness(t *testing.T) {
	b := &backend{}
	h := handler(t, b, 1)
	h.checkReadiness(context.Background())
	if w := call(h, "GET", "/readyz", "", ""); w.Code != 200 {
		t.Fatal("readiness failed")
	}
	b.err = errors.New("backend failed")
	h.checkReadiness(context.Background())
	if w := call(h, "GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatal("unavailable backend looked ready")
	}
}
