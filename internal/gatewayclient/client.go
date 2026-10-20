// Package gatewayclient provides bounded, non-replaying access to the local Gateway.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/localhttp"
)

const MaxResponseBytes = 8 << 20

type Reply struct {
	HTTPStatus int             `json:"http_status"`
	Response   json.RawMessage `json:"response"`
}

type Client struct {
	base, token string
	client      *http.Client
	transport   *http.Transport
}

func New(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("Gateway URL must be http://<explicit-loopback-IP>:<port>")
	}
	port, _ := strconv.Atoi(u.Port())
	if err := localhttp.ValidateLoopback(u.Host); err != nil || port == 0 {
		return nil, errors.New("Gateway URL must use an explicit loopback IP and nonzero port; use an SSH tunnel for remote execution")
	}
	if len(token) < 32 || len(token) > 4096 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("Gateway token must contain 32..4096 visible ASCII characters")
	}
	t := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
		MaxConnsPerHost: 12, MaxIdleConnsPerHost: 12, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout: 55 * time.Second, DisableCompression: true}
	h := &http.Client{Transport: t, Timeout: 55 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{base: "http://" + u.Host, token: token, client: h, transport: t}, nil
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) Execute(ctx context.Context, request api.Request) Reply {
	id := request.RequestID
	request, err := request.Validate()
	if err != nil {
		return Failure(400, "invalid_arguments", false, id)
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > api.MaxBodyBytes {
		return Failure(413, "request_too_large", false, request.RequestID)
	}
	return c.call(ctx, "POST", "/v1/execute", data, request.RequestID)
}

func (c *Client) Status(ctx context.Context) Reply {
	return c.call(ctx, "GET", "/v1/status", nil, "")
}

func Failure(status int, reason string, unknown bool, id string) Reply {
	data, _ := json.Marshal(map[string]any{"error": reason, "reason": reason, "outcome_unknown": unknown, "request_id": id})
	return Reply{status, data}
}

func (c *Client) call(ctx context.Context, method, path string, data []byte, id string) Reply {
	if ctx.Err() != nil {
		return Failure(408, "request_cancelled_before_forwarding", false, id)
	}
	var body io.Reader
	if method == "POST" {
		// No GetBody and no idempotency headers: execution must never be replayed.
		body = io.NopCloser(bytes.NewReader(data))
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return Failure(400, "invalid_request", false, id)
	}
	if method == "POST" {
		r.ContentLength = int64(len(data))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.client.Do(r)
	if err != nil {
		code, reason := 502, "gateway_transport_error"
		var timedOut net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timedOut) && timedOut.Timeout() {
			code, reason = 504, "gateway_deadline_exceeded"
		}
		return Failure(code, reason, method == "POST", id)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	var object map[string]json.RawMessage
	if err != nil || len(raw) > MaxResponseBytes || json.Unmarshal(raw, &object) != nil || object == nil || response.StatusCode >= 300 && response.StatusCode < 400 {
		return Failure(502, "invalid_gateway_response", method == "POST", id)
	}
	return Reply{response.StatusCode, raw}
}
