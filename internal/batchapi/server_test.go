package batchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/batch"
	"github.com/X1Kun/AgentBox/internal/httpauth"
)

type backend struct {
	calls     int
	principal string
	request   batch.Request
	err       error
}

func (b *backend) Submit(_ context.Context, p string, r batch.Request) (batch.View, bool, error) {
	b.calls++
	b.principal = p
	b.request = r
	return batch.View{ID: "task", State: "queued"}, true, b.err
}
func (b *backend) Get(_ context.Context, p, id string) (batch.View, error) {
	b.calls++
	b.principal = p
	return batch.View{ID: id, State: "succeeded"}, b.err
}
func (b *backend) Cancel(_ context.Context, p, id string) (batch.View, error) {
	b.calls++
	return batch.View{ID: id, State: "cancelling"}, b.err
}
func (b *backend) Download(_ context.Context, p, id, name string) ([]byte, error) {
	b.calls++
	return []byte("png"), b.err
}
func (b *backend) Ready(context.Context) error     { return b.err }
func (b *backend) Reconcile(context.Context) error { return b.err }
func setup(t *testing.T) (*Server, *backend) {
	t.Helper()
	b := &backend{}
	s, err := New(b, []httpauth.Principal{{ID: "alice", Token: strings.Repeat("a", 32)}}, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}
func call(s *Server, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if auth {
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndStrictBatchInputs(t *testing.T) {
	s, b := setup(t)
	if w := call(s, "POST", "/v2/batches", `{"request_id":"x","dataset_id":"tiny"}`, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`null`, `[]`, `{"request_id":"x","request_id":"y","dataset_id":"tiny"}`, `{"request_id":null,"dataset_id":"tiny"}`, `{"request_id":"x","dataset_id":"tiny","code":"evil"}`, `{"request_id":"x","passes":1.5}`, `{} {}`} {
		if w := call(s, "POST", "/v2/batches", body, true); w.Code != 400 {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if b.calls != 0 {
		t.Fatal("invalid body reached backend")
	}
	w := call(s, "POST", "/v2/batches", `{"request_id":"x","dataset_id":"tiny","passes":2}`, true)
	if w.Code != 202 || b.calls != 1 || b.principal != "alice" || b.request.Passes != 2 {
		t.Fatal(w.Code, b)
	}
}
func TestCapacityRoutesAndErrors(t *testing.T) {
	s, b := setup(t)
	s.permits <- struct{}{}
	if w := call(s, "GET", "/v2/batches/a", "", true); w.Code != 503 {
		t.Fatal(w.Code)
	}
	<-s.permits
	if w := call(s, "PUT", "/v2/batches/a", "", true); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w := call(s, "GET", "/v2/batches/a?path=secret", "", true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	for code, status := range map[string]int{"not_found": 404, "request_conflict": 409, "artifact_expired": 410, "record_capacity": 429, "artifact_invalid": 503} {
		b.err = &batch.Error{Code: code}
		if w := call(s, "GET", "/v2/batches/a", "", true); w.Code != status {
			t.Fatalf("%s %d", code, w.Code)
		}
	}
	b.err = errors.New("private internal details")
	w := call(s, "GET", "/v2/batches/a", "", true)
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("backend error leaked")
	}
}
func TestDownloadIsBoundedAuthenticatedAttachment(t *testing.T) {
	s, _ := setup(t)
	w := call(s, "GET", "/v2/batches/a/artifacts/x.png", "", true)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal(w.Code, w.Header())
	}
}
func TestReadinessIsSeparateFromLiveness(t *testing.T) {
	s, _ := setup(t)
	if w := call(s, http.MethodGet, "/healthz", "", false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := call(s, http.MethodGet, "/readyz", "", false); w.Code != 503 {
		t.Fatal(w.Code)
	}
	s.ready.Store(true)
	if w := call(s, http.MethodGet, "/readyz", "", false); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
