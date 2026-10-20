// Package gateway forwards validated requests to one local daemon over Unix HTTP.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/localhttp"
)

const maxResponseBytes = 8 << 20 // includes worst-case JSON escaping of 1 MiB output

type Config struct {
	Socket         string
	Token          string
	DaemonToken    string
	MaxInflight    int
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

type Server struct {
	config     Config
	client     *http.Client
	transport  *http.Transport
	executions chan struct{}
	controls   chan struct{}
	authorized func(http.ResponseWriter, *http.Request) bool
}

func New(c Config) (*Server, error) {
	if err := localhttp.ValidateSocket(c.Socket); err != nil {
		return nil, err
	}
	if len(c.Token) < 32 || len(c.DaemonToken) < 32 {
		return nil, errors.New("gateway and daemon tokens must each contain at least 32 characters")
	}
	for _, token := range []string{c.Token, c.DaemonToken} {
		if len(token) > 4096 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return nil, errors.New("tokens must contain 32..4096 visible ASCII characters")
		}
	}
	if c.Token == c.DaemonToken {
		return nil, errors.New("gateway and daemon tokens must differ")
	}
	if c.MaxInflight == 0 {
		c.MaxInflight = 8
	}
	if c.MaxInflight < 1 || c.MaxInflight > 64 {
		return nil, errors.New("gateway max-inflight must be 1..64")
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 50 * time.Second
	}
	if c.RequestTimeout < time.Millisecond || c.RequestTimeout > time.Minute {
		return nil, errors.New("gateway request-timeout must be 1ms..1m")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	dialer := net.Dialer{Timeout: time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", c.Socket)
		},
		MaxIdleConns: c.MaxInflight + 4, MaxIdleConnsPerHost: c.MaxInflight + 4, MaxConnsPerHost: c.MaxInflight + 4,
		IdleConnTimeout: 30 * time.Second, DisableCompression: true,
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Server{config: c, client: client, transport: transport, executions: make(chan struct{}, c.MaxInflight), controls: make(chan struct{}, 4), authorized: httpapi.BearerAuthorizer(c.Token)}, nil
}

func (s *Server) Close() { s.transport.CloseIdleConnections() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		httpapi.WriteJSON(w, 400, map[string]string{"error": "query parameters are not supported"})
		return
	}
	method := "GET"
	switch r.URL.Path {
	case "/healthz":
		if r.Method == "GET" {
			httpapi.WriteJSON(w, 200, map[string]bool{"alive": true})
			return
		}
	case "/readyz", "/v1/status":
	case "/v1/execute":
		method = "POST"
	default:
		httpapi.WriteJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		httpapi.WriteJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if r.URL.Path != "/readyz" && !s.authorized(w, r) {
		return
	}
	permits := s.controls
	if method == "POST" {
		permits = s.executions
	}
	select {
	case permits <- struct{}{}:
		defer func() { <-permits }()
	default:
		w.Header().Set("Retry-After", "1")
		httpapi.WriteJSON(w, 503, map[string]any{"error": "gateway capacity exhausted", "reason": "gateway_capacity", "ready": false})
		return
	}
	budget := s.config.RequestTimeout
	if method == "GET" {
		budget = min(budget, 2*time.Second)
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	var raw []byte
	var requestID string
	if method == "POST" {
		request, body, ok := httpapi.ReadRequest(w, r.WithContext(ctx))
		if !ok {
			return
		}
		raw, requestID = body, request.RequestID
	}
	// A generic ReadCloser deliberately leaves GetBody nil. Together with
	// stripping idempotency headers, this prevents transport replay of execution.
	var body io.Reader
	if method == "POST" {
		body = io.NopCloser(bytes.NewReader(raw))
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agentbox"+r.URL.Path, body)
	if err != nil {
		s.fail(w, r, requestID, "daemon_unavailable", err)
		return
	}
	if method == "POST" {
		request.ContentLength = int64(len(raw))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+s.config.DaemonToken)
	deadline, _ := ctx.Deadline()
	request.Header.Set(httpapi.DeadlineHeader, deadline.UTC().Format(time.RFC3339Nano))
	response, err := s.client.Do(request)
	if err != nil {
		s.fail(w, r, requestID, "daemon_unavailable", err)
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		s.fail(w, r, requestID, "daemon_response_failed", err)
		return
	}
	if len(data) > maxResponseBytes || !json.Valid(data) {
		s.fail(w, r, requestID, "invalid_daemon_response", errors.New("invalid bounded JSON response"))
		return
	}
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode >= 300 && response.StatusCode < 400 {
		s.fail(w, r, requestID, "daemon_protocol_error", fmt.Errorf("daemon HTTP %d", response.StatusCode))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if value := response.Header.Get("Retry-After"); value != "" {
		w.Header().Set("Retry-After", value)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(data)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, id, reason string, err error) {
	if r.Context().Err() != nil {
		return
	}
	code := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		code, reason = http.StatusGatewayTimeout, "request_deadline_exceeded"
	}
	if r.URL.Path == "/readyz" {
		code = http.StatusServiceUnavailable
	}
	// Errors may include transport details; only fixed causes are returned/logged.
	s.config.Logger.Warn("gateway upstream failure", "reason", reason, "request_id", id)
	report := map[string]any{"error": "daemon request failed", "reason": reason}
	if r.URL.Path == "/readyz" {
		report["ready"] = false
	}
	if r.Method == "POST" {
		report["outcome_unknown"] = true
		report["request_id"] = id
	}
	httpapi.WriteJSON(w, code, report)
}
