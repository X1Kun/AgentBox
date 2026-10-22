package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/localhttp"
)

const publicToken = "gateway-test-token-with-at-least-32-characters"
const backendToken = "daemon-test-token-with-at-least-32-characters"
const validBody = `{"runtime":"python","code":"print(1)","request_id":"client-1"}`

type lateDeadlineTransport struct{ calls atomic.Int32 }

func (x *lateDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	x.calls.Add(1)
	deadline, _ := r.Context().Deadline()
	time.Sleep(max(0, time.Until(deadline)) + time.Millisecond)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

func TestGatewayClassifiesResponseAtExpiredDeadlineWithoutReplay(t *testing.T) {
	s := testGateway(t, http.NotFoundHandler(), func(c *Config) { c.RequestTimeout = 5 * time.Millisecond })
	transport := &lateDeadlineTransport{}
	s.client = &http.Client{Transport: transport}
	w := callGateway(s, "POST", "/v1/execute", validBody, publicToken)
	if w.Code != 504 || !strings.Contains(w.Body.String(), "request_deadline_exceeded") || !strings.Contains(w.Body.String(), `"outcome_unknown":true`) || transport.calls.Load() != 1 {
		t.Fatalf("deadline classification/replay: %d %s calls=%d", w.Code, w.Body.String(), transport.calls.Load())
	}
}

func testGateway(t *testing.T, backend http.Handler, mutate func(*Config)) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "d.sock")
	listener, err := localhttp.Listen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	upstream := &http.Server{Handler: httpapi.DeadlineHandler(backend, time.Minute)}
	go func() { _ = upstream.Serve(listener) }()
	t.Cleanup(func() { _ = upstream.Close() })
	c := Config{Socket: path, Token: publicToken, DaemonToken: backendToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mutate != nil {
		mutate(&c)
	}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func callGateway(s http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestGatewayBoundaryAndExactForwarding(t *testing.T) {
	var calls atomic.Int32
	s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+backendToken || r.URL.Path != "/v1/execute" || r.URL.RawQuery != "" || r.Host != "agentbox" {
			t.Error("unexpected backend identity/route")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("deadline not propagated")
		}
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("client control headers propagated")
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != validBody {
			t.Error("request bytes changed")
		}
		httpapi.WriteJSON(w, 200, api.Result{RequestID: "client-1", ExecutionID: "e1", Status: "succeeded", Stdout: "ok\n", CleanupComplete: true})
	}), nil)
	for _, tc := range []struct {
		body, token string
		want        int
	}{
		{validBody, "bad", 401}, {`{"runtime":"python","code":"1","rootfs":"/"}`, publicToken, 400},
		{validBody + `{}`, publicToken, 400}, {`{"runtime":"python","code":"1","timeout_ms":30001}`, publicToken, 400},
		{`{"code":"` + strings.Repeat("x", api.MaxBodyBytes) + `"}`, publicToken, 413},
	} {
		if w := callGateway(s, "POST", "/v1/execute", tc.body, tc.token); w.Code != tc.want {
			t.Fatalf("boundary %d want %d", w.Code, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached daemon")
	}
	r := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(validBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+publicToken)
	r.Header.Set(httpapi.DeadlineHeader, "untrusted")
	r.Header.Set("Idempotency-Key", "client-key")
	r.Header.Set("X-Forwarded-For", "untrusted")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var result api.Result
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || calls.Load() != 1 || result.ExecutionID != "e1" || !result.CleanupComplete || result.Stdout != "ok\n" {
		t.Fatalf("forwarding: %d %s", w.Code, w.Body.String())
	}
}

func TestGatewayPreservesExecutionAndAdmissionResults(t *testing.T) {
	for _, status := range []int{200, 500, 503} {
		s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"reason":"qos_pressure","queue_ms":12,"request_id":"client-1"}`))
		}), nil)
		w := callGateway(s, "POST", "/v1/execute", validBody, publicToken)
		if w.Code != status || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"reason":"qos_pressure"`) {
			t.Fatalf("mapping: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestGatewayCapacityDeadlineAndClientCancellation(t *testing.T) {
	entered, canceled := make(chan struct{}, 2), make(chan struct{}, 2)
	s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}), func(c *Config) { c.MaxInflight = 1; c.RequestTimeout = 150 * time.Millisecond })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- callGateway(s, "POST", "/v1/execute", validBody, publicToken) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend not reached")
	}
	if w := callGateway(s, "POST", "/v1/execute", validBody, publicToken); w.Code != 503 || !strings.Contains(w.Body.String(), "gateway_capacity") {
		t.Fatal("gateway queued instead of rejecting")
	}
	if w := <-done; w.Code != 504 || !strings.Contains(w.Body.String(), `"outcome_unknown":true`) {
		t.Fatalf("timeout: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("deadline did not cancel backend")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(validBody)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+publicToken)
	r.Header.Set("Content-Type", "application/json")
	finished := make(chan struct{})
	go func() { s.ServeHTTP(httptest.NewRecorder(), r); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second request not reached")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not reach backend")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("gateway did not release request")
	}
}

func TestGatewayNeverReplaysAmbiguousExecution(t *testing.T) {
	var calls atomic.Int32
	s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}), nil)
	w := callGateway(s, "POST", "/v1/execute", validBody, publicToken)
	if w.Code != 502 || calls.Load() != 1 || !strings.Contains(w.Body.String(), `"outcome_unknown":true`) {
		t.Fatalf("ambiguous result replayed/hidden: %d calls=%d %s", w.Code, calls.Load(), w.Body.String())
	}
}

func TestGatewayReadinessUnavailableAndBackendAuthFailure(t *testing.T) {
	s, err := New(Config{Socket: filepath.Join(t.TempDir(), "absent.sock"), Token: publicToken, DaemonToken: backendToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if w := callGateway(s, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatal("liveness depends on daemon")
	}
	if w := callGateway(s, "GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatal("missing daemon reported ready")
	}
	if w := callGateway(s, "GET", "/v1/status", "", ""); w.Code != 401 {
		t.Fatal("status exposed without auth")
	}
	backend := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
	}), nil)
	if w := callGateway(backend, "POST", "/v1/execute", validBody, publicToken); w.Code != 502 {
		t.Fatal("backend credential failure blamed on client")
	}
}

func TestGatewayRejectsInvalidConfiguration(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.Socket = "relative" }, func(c *Config) { c.Token = "short" }, func(c *Config) { c.DaemonToken = c.Token }, func(c *Config) { c.MaxInflight = 65 }, func(c *Config) { c.RequestTimeout = 2 * time.Minute }} {
		c := Config{Socket: "/tmp/daemon.sock", Token: publicToken, DaemonToken: backendToken}
		mutate(&c)
		if s, err := New(c); err == nil {
			s.Close()
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}

func TestGatewayBudgetBoundsSlowRequestBody(t *testing.T) {
	var calls atomic.Int32
	s := testGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		httpapi.WriteJSON(w, 200, map[string]bool{"unexpected": true})
	}), func(c *Config) { c.RequestTimeout = 40 * time.Millisecond })
	front := httptest.NewServer(s)
	defer front.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = io.WriteString(connection, "POST /v1/execute HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+publicToken+"\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 408 || calls.Load() != 0 {
		t.Fatalf("slow body reached daemon or exceeded request budget: %d", response.StatusCode)
	}
}
