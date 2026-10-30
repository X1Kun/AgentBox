// Package sessionapi exposes versioned Kubernetes-backed session operations.
package sessionapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/X1Kun/AgentBox/internal/session"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Backend interface {
	Create(context.Context, string, session.CreateRequest) (session.View, bool, error)
	Get(context.Context, string, string) (session.View, error)
	Close(context.Context, string, string) (session.View, error)
	Ready(context.Context) error
}
type Operations interface {
	Execute(context.Context, string, string, session.ExecuteRequest) (session.ExecutionView, error)
	Write(context.Context, string, string, session.FileRequest) (session.FileView, error)
	Read(context.Context, string, string, session.FileRequest) (session.FileView, error)
}
type Principal struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}
type credential struct {
	principal string
	sum       [32]byte
}
type Server struct {
	backend     Backend
	credentials []credential
	timeout     time.Duration
	permits     chan struct{}
	operations  chan struct{}
}

func New(backend Backend, principals []Principal, timeout time.Duration, maxInflight int) (*Server, error) {
	if backend == nil || len(principals) == 0 || len(principals) > 64 || timeout < time.Second || timeout > 30*time.Second || maxInflight < 1 || maxInflight > 128 {
		return nil, errors.New("invalid lifecycle HTTP configuration")
	}
	s := &Server{backend: backend, timeout: timeout, permits: make(chan struct{}, maxInflight), operations: make(chan struct{}, maxInflight)}
	seen := map[[32]byte]bool{}
	for _, p := range principals {
		if !session.ValidPrincipal(p.ID) || len(p.Token) < 32 || len(p.Token) > 4096 || strings.IndexFunc(p.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return nil, errors.New("invalid configured principal credentials")
		}
		sum := sha256.Sum256([]byte(p.Token))
		if seen[sum] {
			return nil, errors.New("duplicate configured token")
		}
		seen[sum] = true
		s.credentials = append(s.credentials, credential{p.ID, sum})
	}
	return s, nil
}
func (s *Server) authenticate(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 4103 {
		return ""
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	sum := sha256.Sum256([]byte(token))
	principal := ""
	for _, c := range s.credentials {
		if subtle.ConstantTimeCompare(sum[:], c.sum[:]) == 1 {
			principal = c.principal
		}
	}
	return principal
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"error": code})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		problem(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			problem(w, 405, "method_not_allowed")
			return
		}
		write(w, 200, map[string]bool{"alive": true})
		return
	}
	if r.URL.Path == "/readyz" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			problem(w, 405, "method_not_allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), min(s.timeout, 2*time.Second))
		defer cancel()
		if err := s.backend.Ready(ctx); err != nil {
			// Report only the status category, never native messages or credentials.
			slog.Warn("lifecycle backend readiness failed", "reason", apierrors.ReasonForError(err))
			problem(w, 503, "backend_unavailable")
			return
		}
		write(w, 200, map[string]bool{"ready": true})
		return
	}
	collection := r.URL.Path == "/v2/sessions"
	id, action := "", ""
	if strings.HasPrefix(r.URL.Path, "/v2/sessions/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/sessions/"), "/")
		id = parts[0]
		if len(parts) > 1 {
			action = strings.Join(parts[1:], "/")
		}
	}
	dataOperation := action == "execute" || action == "files/write" || action == "files/read"
	if !collection && (id == "" || (action != "" && !dataOperation)) {
		problem(w, 404, "not_found")
		return
	}
	allowed := "POST"
	if !collection && !dataOperation {
		allowed = "GET, DELETE"
	}
	if ((collection || dataOperation) && r.Method != "POST") || (!collection && !dataOperation && r.Method != "GET" && r.Method != "DELETE") {
		w.Header().Set("Allow", allowed)
		problem(w, 405, "method_not_allowed")
		return
	}
	principal := s.authenticate(r)
	if principal == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, 401, "unauthorized")
		return
	}
	permits := s.permits
	if dataOperation {
		permits = s.operations
	}
	select {
	case permits <- struct{}{}:
		defer func() { <-permits }()
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, 503, "api_capacity")
		return
	}
	budget := s.timeout
	if dataOperation {
		budget = 10 * time.Second
		if action == "execute" {
			budget = 70 * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	var view session.View
	var response any
	var err error
	status := 200
	if collection || dataOperation {
		media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != "application/json" {
			problem(w, 415, "unsupported_media_type")
			return
		}
		limit := int64(4096)
		if action == "execute" {
			limit = 8 * session.MaxCodeBytes
		}
		if action == "files/write" {
			limit = 2 * session.MaxFileBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		data, e := io.ReadAll(r.Body)
		if e != nil {
			var large *http.MaxBytesError
			if errors.As(e, &large) {
				problem(w, 413, "request_too_large")
			} else {
				problem(w, 400, "invalid_request")
			}
			return
		}
		if dataOperation {
			backend, ok := s.backend.(Operations)
			if !ok {
				problem(w, 503, "execution_unavailable")
				return
			}
			switch action {
			case "execute":
				var request session.ExecuteRequest
				e = decodeOperation(data, &request, []string{"identity_revision", "code", "timeout_seconds"}, []string{"identity_revision", "code"})
				if e == nil {
					response, err = backend.Execute(ctx, principal, id, request)
				}
			case "files/write", "files/read":
				var request session.FileRequest
				keys := []string{"identity_revision", "path"}
				required := []string{"identity_revision", "path"}
				if action == "files/write" {
					keys = append(keys, "content_base64")
					required = append(required, "content_base64")
				}
				e = decodeOperation(data, &request, keys, required)
				if e == nil {
					if action == "files/write" {
						response, err = backend.Write(ctx, principal, id, request)
					} else {
						response, err = backend.Read(ctx, principal, id, request)
					}
				}
			}
			if e != nil {
				problem(w, 400, "invalid_request")
				return
			}
		} else {
			request, e := decodeCreate(data)
			if e != nil {
				problem(w, 400, "invalid_request")
				return
			}
			var replay bool
			view, replay, err = s.backend.Create(ctx, principal, request)
			if !replay {
				status = 202
			}
		}
	} else {
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			problem(w, 400, "invalid_request")
			return
		}
		if r.Method == "GET" {
			view, err = s.backend.Get(ctx, principal, id)
		} else {
			view, err = s.backend.Close(ctx, principal, id)
			if view.State != "closed" {
				status = 202
			}
		}
	}
	if err != nil {
		code := session.Code(err)
		reason := string(apierrors.ReasonForError(err))
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		}
		slog.Warn("lifecycle request failed", "code", code, "reason", reason)
		status = 503
		switch code {
		case "invalid_request", "invalid_path":
			status = 400
		case "not_found", "file_not_found":
			status = 404
		case "request_conflict", "identity_changed", "integrity_error", "state_conflict", "session_busy", "session_not_ready":
			status = 409
		case "file_access_denied":
			status = 403
		case "file_too_large", "execution_output_limit":
			status = 413
		case "execution_timeout":
			status = 504
		}
		problem(w, status, code)
		return
	}
	if response == nil {
		response = view
	}
	write(w, status, response)
}

// Reject duplicate keys and trailing JSON rather than depending on last-key-wins decoding.
func decodeCreate(data []byte) (session.CreateRequest, error) {
	var request session.CreateRequest
	if !utf8.Valid(data) {
		return request, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return request, errors.New("expected object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, e := decoder.Token()
		if e != nil {
			return request, e
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return request, errors.New("duplicate key")
		}
		seen[key] = true
		switch key {
		case "request_id":
			err = decoder.Decode(&request.RequestID)
		case "ttl_seconds":
			var raw json.RawMessage
			err = decoder.Decode(&raw)
			if err == nil {
				if string(raw) == "null" {
					return request, errors.New("null TTL")
				}
				err = json.Unmarshal(raw, &request.TTLSeconds)
			}
		default:
			return request, errors.New("unknown key")
		}
		if err != nil {
			return request, err
		}
	}
	if _, err = decoder.Token(); err != nil {
		return request, err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return request, errors.New("trailing JSON")
	}
	return request, nil
}

// Exact keys, no duplicates/nulls/trailing values. Byte slices use standard
// padded base64, allowing binary artifacts without multipart machinery.
func decodeOperation(data []byte, dst any, allowed, required []string) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("expected object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		valid := false
		for _, k := range allowed {
			if key == k {
				valid = true
			}
		}
		if !ok || !valid || seen[key] {
			return errors.New("unexpected or duplicate key")
		}
		seen[key] = true
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return err
		}
		if string(raw) == "null" {
			return errors.New("null field")
		}
	}
	if _, err = decoder.Token(); err != nil {
		return err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	for _, key := range required {
		if !seen[key] {
			return errors.New("missing field")
		}
	}
	return json.Unmarshal(data, dst)
}
