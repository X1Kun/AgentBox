// Package batchapi serves configured-principal batch operations and bounded PNGs.
package batchapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/readiness"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	batch "github.com/X1Kun/AgentBox/internal/batchprotocol"
	"github.com/X1Kun/AgentBox/internal/httpauth"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Backend interface {
	Submit(context.Context, string, batch.Request) (batch.View, bool, error)
	Get(context.Context, string, string) (batch.View, error)
	Cancel(context.Context, string, string) (batch.View, error)
	Download(context.Context, string, string, string) ([]byte, error)
	Ready(context.Context) error
}

type Server struct {
	backend   Backend
	auth      *httpauth.Authenticator
	timeout   time.Duration
	permits   chan struct{}
	readiness readiness.Snapshot
	now       func() time.Time
	metrics   *telemetry.Metrics
}

func (s *Server) SetTelemetry(metrics *telemetry.Metrics) { s.metrics = metrics }

func New(backend Backend, principals []httpauth.Principal, timeout time.Duration, maxInflight int) (*Server, error) {
	if backend == nil || timeout < time.Second || timeout > 30*time.Second || maxInflight < 1 || maxInflight > 128 {
		return nil, errors.New("invalid batch HTTP configuration")
	}
	auth, err := httpauth.New(principals)
	if err != nil {
		return nil, err
	}
	return &Server{backend: backend, auth: auth, timeout: timeout, permits: make(chan struct{}, maxInflight), now: time.Now}, nil
}
func write(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"error": code})
}
func decode(data []byte, request *batch.Request) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("expected object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("duplicate key")
		}
		seen[key] = true
		var raw json.RawMessage
		if err = d.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("invalid field")
		}
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if err = d.Decode(new(interface{})); err != io.EOF {
		return errors.New("trailing data")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(request)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		problem(w, 400, batch.CodeInvalidRequest)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if r.Method != "GET" {
			problem(w, 405, "method_not_allowed")
			return
		}
		if r.URL.Path == "/readyz" && !s.readiness.Ready(s.now()) {
			problem(w, 503, batch.CodeBackendUnavailable)
			return
		}
		write(w, 200, map[string]bool{"alive": true})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	collection := r.URL.Path == "/v2/batches"
	item := len(parts) == 3 && parts[0] == "v2" && parts[1] == "batches"
	artifact := len(parts) == 5 && parts[0] == "v2" && parts[1] == "batches" && parts[3] == "artifacts"
	if !collection && !item && !artifact {
		problem(w, 404, batch.CodeNotFound)
		return
	}
	if collection && r.Method != "POST" || item && r.Method != "GET" && r.Method != "DELETE" || artifact && r.Method != "GET" {
		problem(w, 405, "method_not_allowed")
		return
	}
	principal := s.auth.Authenticate(r)
	if principal == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, 401, "unauthorized")
		return
	}
	select {
	case s.permits <- struct{}{}:
		defer func() { <-s.permits }()
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, 503, "api_capacity")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	var view batch.View
	var err error
	status := 200
	if artifact {
		s.download(w, ctx, principal, parts[2], parts[4])
		return
	}
	if collection {
		request, ok := readSubmission(w, r)
		if !ok {
			return
		}
		var created bool
		view, created, err = s.backend.Submit(ctx, principal, request)
		if created {
			status = 202
		}
	} else if r.Method == "DELETE" {
		view, err = s.backend.Cancel(ctx, principal, parts[2])
		if !view.CleanupComplete {
			status = 202
		}
	} else {
		view, err = s.backend.Get(ctx, principal, parts[2])
	}
	if err != nil {
		writeError(w, err)
		return
	}

	write(w, status, view)
}

func writeError(w http.ResponseWriter, err error) {
	code := batch.Code(err)
	status := 503
	switch code {
	case batch.CodeInvalidRequest:
		status = 400
	case batch.CodeNotFound:
		status = 404
	case batch.CodeRequestConflict, batch.CodeArtifactNotReady:
		status = 409
	case batch.CodeArtifactExpired:
		status = 410
	case batch.CodeRecordCapacity:
		status = 429
	}
	if apierrors.IsConflict(err) {
		status = 409
		code = "conflict"
	}
	problem(w, status, code)
}
func (s *Server) download(w http.ResponseWriter, ctx context.Context, principal, id, name string) {
	data, err := s.backend.Download(ctx, principal, id, name)
	if err == nil {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
		return
	}

	writeError(w, err)
}
func readSubmission(w http.ResponseWriter, r *http.Request) (batch.Request, bool) {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		problem(w, 415, "unsupported_media_type")
		return batch.Request{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	data, e := io.ReadAll(r.Body)
	if e != nil {
		var large *http.MaxBytesError
		if errors.As(e, &large) {
			problem(w, 413, "request_too_large")
		} else {
			problem(w, 400, batch.CodeInvalidRequest)
		}
		return batch.Request{}, false
	}
	var request batch.Request
	if decode(data, &request) != nil {
		problem(w, 400, batch.CodeInvalidRequest)
		return batch.Request{}, false
	}

	return request, true
}
