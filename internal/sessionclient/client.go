// Package sessionclient provides bounded, non-replaying access to the v2 API.
package sessionclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/X1Kun/AgentBox/internal/session"
)

const MaxBodyBytes = 512 << 10
const MaxResponseBytes = 1 << 20

var identifier = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)

func ValidID(id string) bool { return id != "." && id != ".." && identifier.MatchString(id) }

var requestIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func ValidRequestID(id string) bool { return requestIdentifier.MatchString(id) }
func ValidPath(p string) bool {
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}

type Reply struct {
	HTTPStatus int             `json:"http_status"`
	Response   json.RawMessage `json:"response"`
}
type Client struct {
	base, token string
	http        *http.Client
	transport   *http.Transport
}

func New(base, token, caFile string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid session API URL")
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid API port")
		}
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("remote session API requires HTTPS")
		}
	}
	if len(token) < 32 || len(token) > 4096 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("API token must contain 32..4096 visible ASCII characters")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		file, e := os.Open(caFile)
		if e != nil {
			return nil, errors.New("unable to read API CA bundle")
		}
		data, e := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		_ = file.Close()
		if e != nil || len(data) > 1<<20 {
			return nil, errors.New("unable to read bounded API CA bundle")
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("invalid API CA bundle")
		}
		tlsConfig.RootCAs = roots
	}
	// No proxy, redirect, reusable connection or replay body. Credentials stay on
	// the configured API connection and mutations cannot use stale-connection retry.
	t := &http.Transport{TLSClientConfig: tlsConfig, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, DisableKeepAlives: true, DisableCompression: true, ResponseHeaderTimeout: 78 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 20}
	h := &http.Client{Transport: t, Timeout: 80 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{base: strings.TrimRight(u.String(), "/"), token: token, http: h, transport: t}, nil
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }
func Failure(status int, code string, unknown bool) Reply {
	raw, _ := json.Marshal(map[string]any{"error": code, "outcome_unknown": unknown})
	return Reply{status, raw}
}
func (c *Client) Create(ctx context.Context, r session.CreateRequest) Reply {
	if !ValidRequestID(r.RequestID) || (r.TTLSeconds != 0 && (r.TTLSeconds < 60 || r.TTLSeconds > 86400)) {
		return Failure(400, "invalid_arguments", false)
	}
	return validateView(c.call(ctx, "POST", "/v2/sessions", r, true), "")
}
func (c *Client) Get(ctx context.Context, id string) Reply {
	if !ValidID(id) {
		return Failure(400, "invalid_arguments", false)
	}
	return validateView(c.call(ctx, "GET", "/v2/sessions/"+id, nil, false), id)
}
func (c *Client) CloseSession(ctx context.Context, id string) Reply {
	if !ValidID(id) {
		return Failure(400, "invalid_arguments", false)
	}
	return validateView(c.call(ctx, "DELETE", "/v2/sessions/"+id, nil, true), id)
}
func (c *Client) Execute(ctx context.Context, id string, r session.ExecuteRequest) Reply {
	if !ValidID(id) || len(r.Code) == 0 || len(r.Code) > session.MaxCodeBytes || !utf8.ValidString(r.Code) || strings.ContainsRune(r.Code, 0) || r.IdentityRevision == "" || r.TimeoutSeconds < 0 || r.TimeoutSeconds > 60 {
		return Failure(400, "invalid_arguments", false)
	}
	return validateExecution(c.call(ctx, "POST", "/v2/sessions/"+id+"/execute", r, true), id, r.IdentityRevision)
}
func (c *Client) Write(ctx context.Context, id string, r session.FileRequest) Reply {
	if !ValidID(id) || !ValidPath(r.Path) || len(r.Content) > session.MaxFileBytes || r.IdentityRevision == "" {
		return Failure(400, "invalid_arguments", false)
	}
	body := struct {
		IdentityRevision string `json:"identity_revision"`
		Path             string `json:"path"`
		Content          []byte `json:"content_base64"`
	}{r.IdentityRevision, r.Path, r.Content}
	if body.Content == nil {
		body.Content = []byte{}
	}
	return validateFile(c.call(ctx, "POST", "/v2/sessions/"+id+"/files/write", body, true), id, r.IdentityRevision, r.Path, len(r.Content))
}
func (c *Client) Read(ctx context.Context, id string, r session.FileRequest) Reply {
	if !ValidID(id) || !ValidPath(r.Path) || r.IdentityRevision == "" || r.Content != nil {
		return Failure(400, "invalid_arguments", false)
	}
	return validateFile(c.call(ctx, "POST", "/v2/sessions/"+id+"/files/read", r, false), id, r.IdentityRevision, r.Path, -1)
}
func (c *Client) call(ctx context.Context, method, path string, body any, mutating bool) Reply {
	if ctx.Err() != nil {
		return Failure(408, "request_cancelled_before_forwarding", false)
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil || len(data) > MaxBodyBytes {
			return Failure(400, "invalid_arguments", false)
		}
	}
	var reader io.Reader
	if body != nil {
		reader = io.NopCloser(bytes.NewReader(data))
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return Failure(400, "invalid_arguments", false)
	}
	if body != nil {
		request.ContentLength = int64(len(data))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return Failure(502, "api_transport_error", mutating)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	var object map[string]json.RawMessage
	if err != nil || len(raw) > MaxResponseBytes || json.Unmarshal(raw, &object) != nil || object == nil || (response.StatusCode >= 300 && response.StatusCode < 400) {
		return Failure(502, "invalid_api_response", mutating)
	}
	return Reply{response.StatusCode, raw}
}

func validateView(reply Reply, id string) Reply {
	if reply.HTTPStatus < 200 || reply.HTTPStatus >= 300 {
		return reply
	}
	var v session.View
	var fields map[string]json.RawMessage
	if json.Unmarshal(reply.Response, &fields) != nil || string(fields["cleanup_complete"]) == "null" || len(fields["cleanup_complete"]) == 0 || len(fields["operation_active"]) == 0 || string(fields["operation_active"]) == "null" || json.Unmarshal(reply.Response, &v) != nil || !ValidID(v.ID) || (id != "" && v.ID != id) || v.ExpiresAt == "" || (v.CleanupComplete && v.State != "closed") {
		return Failure(502, "invalid_api_response", true)
	}
	switch v.State {
	case "creating", "ready", "unavailable", "invalidated", "closing", "closed", "failed":
	default:
		return Failure(502, "invalid_api_response", true)
	}
	if v.State == "ready" && v.IdentityRevision == "" {
		return Failure(502, "invalid_api_response", true)
	}
	return reply
}
func validateExecution(reply Reply, id, revision string) Reply {
	if reply.HTTPStatus >= 200 && reply.HTTPStatus < 300 && reply.HTTPStatus != 200 {
		return Failure(502, "invalid_api_response", true)
	}
	if reply.HTTPStatus != 200 {
		return reply
	}
	var v session.ExecutionView
	var fields map[string]json.RawMessage
	if json.Unmarshal(reply.Response, &v) != nil || json.Unmarshal(reply.Response, &fields) != nil || v.SessionID != id || v.IdentityRevision != revision {
		return Failure(502, "invalid_api_response", true)
	}
	for _, key := range []string{"exit_code", "stdout", "stderr"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return Failure(502, "invalid_api_response", true)
		}
	}
	return reply
}
func validateFile(reply Reply, id, revision, path string, writeSize int) Reply {
	if reply.HTTPStatus >= 200 && reply.HTTPStatus < 300 && reply.HTTPStatus != 200 {
		return Failure(502, "invalid_api_response", writeSize >= 0)
	}
	if reply.HTTPStatus != 200 {
		return reply
	}
	var v session.FileView
	var fields map[string]json.RawMessage
	if json.Unmarshal(reply.Response, &fields) != nil || len(fields["size_bytes"]) == 0 || string(fields["size_bytes"]) == "null" || (writeSize < 0 && (len(fields["content_base64"]) == 0 || string(fields["content_base64"]) == "null")) || json.Unmarshal(reply.Response, &v) != nil || v.SessionID != id || v.IdentityRevision != revision || v.Path != path || v.Size < 0 || v.Size > session.MaxFileBytes || (writeSize >= 0 && v.Size != writeSize) || (writeSize < 0 && len(v.Content) != v.Size) {
		return Failure(502, "invalid_api_response", writeSize >= 0)
	}
	return reply
}
