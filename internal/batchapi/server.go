// Package batchapi serves configured-principal batch operations and bounded PNGs.
package batchapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/X1Kun/AgentBox/internal/batch"
	"github.com/X1Kun/AgentBox/internal/httpauth"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Server struct {
	backend batch.Backend
	auth    *httpauth.Authenticator
	timeout time.Duration
	permits chan struct{}
	ready   atomic.Bool
}

func New(backend batch.Backend, principals []httpauth.Principal, timeout time.Duration, maxInflight int) (*Server, error) {
	if backend == nil || timeout < time.Second || timeout > 30*time.Second || maxInflight < 1 || maxInflight > 128 {
		return nil, errors.New("invalid batch HTTP configuration")
	}
	auth, err := httpauth.New(principals)
	if err != nil {
		return nil, err
	}
	return &Server{backend: backend, auth: auth, timeout: timeout, permits: make(chan struct{}, maxInflight)}, nil
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
		problem(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if r.Method != "GET" {
			problem(w, 405, "method_not_allowed")
			return
		}
		if r.URL.Path == "/readyz" && !s.ready.Load() {
			problem(w, 503, "backend_unavailable")
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
		problem(w, 404, "not_found")
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
		var data []byte
		data, err = s.backend.Download(ctx, principal, parts[2], parts[4])
		if err == nil {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": parts[4]}))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(data)
			return
		}
	} else if collection {
		media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if e != nil || media != "application/json" {
			problem(w, 415, "unsupported_media_type")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
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
		var request batch.Request
		if decode(data, &request) != nil {
			problem(w, 400, "invalid_request")
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
		code := batch.Code(err)
		status = 503
		switch code {
		case "invalid_request":
			status = 400
		case "not_found":
			status = 404
		case "request_conflict", "artifact_not_ready":
			status = 409
		case "artifact_expired":
			status = 410
		case "record_capacity":
			status = 429
		}
		if apierrors.IsConflict(err) {
			status = 409
			code = "conflict"
		}
		problem(w, status, code)
		return
	}
	write(w, status, view)
}
func (s *Server) RunReadiness(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		budget, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.backend.Ready(budget)
		s.ready.Store(err == nil)
		if err == nil {
			_ = s.backend.Reconcile(budget)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
