package sessionapi

import (
	"context"
	"strings"
	"testing"

	"github.com/X1Kun/AgentBox/internal/session"
)

type operationBackend struct {
	backend
	code, revision, path string
	content              []byte
}

func (b *operationBackend) Execute(ctx context.Context, p, id string, r session.ExecuteRequest) (session.ExecutionView, error) {
	b.calls.Add(1)
	b.principal = p
	b.code = r.Code
	b.revision = r.IdentityRevision
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
		}
	}
	return session.ExecutionView{Execution: session.Execution{Stdout: "done", ExitCode: 3}}, b.err
}
func (b *operationBackend) Write(_ context.Context, p, id string, r session.FileRequest) (session.FileView, error) {
	b.calls.Add(1)
	b.principal = p
	b.path = r.Path
	b.content = r.Content
	return session.FileView{Size: len(r.Content)}, b.err
}
func (b *operationBackend) Read(_ context.Context, p, id string, r session.FileRequest) (session.FileView, error) {
	b.calls.Add(1)
	b.principal = p
	b.path = r.Path
	return session.FileView{Content: []byte{0, 255}}, b.err
}
func TestOperationRoutingAndBinaryEncoding(t *testing.T) {
	b := &operationBackend{}
	h := handler(t, b, 2)
	w := call(h, "POST", "/v2/sessions/id/execute", aliceToken, `{"identity_revision":"rev","code":"print(1)"}`)
	if w.Code != 200 || b.principal != "alice" || b.code != "print(1)" || b.revision != "rev" || !strings.Contains(w.Body.String(), `"exit_code":3`) {
		t.Fatalf("execution routing: %d %s", w.Code, w.Body.String())
	}
	w = call(h, "POST", "/v2/sessions/id/files/write", aliceToken, `{"identity_revision":"rev","path":"a.bin","content_base64":"AP8="}`)
	if w.Code != 200 || b.path != "a.bin" || len(b.content) != 2 || b.content[1] != 255 {
		t.Fatal("binary upload failed")
	}
	w = call(h, "POST", "/v2/sessions/id/files/read", aliceToken, `{"identity_revision":"rev","path":"a.bin"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content_base64":"AP8="`) {
		t.Fatal("binary download failed")
	}
}
func TestOperationInputRejectsDuplicatesUnknownNullAndMissingFields(t *testing.T) {
	b := &operationBackend{}
	h := handler(t, b, 2)
	for _, body := range []string{
		`{"identity_revision":"rev","code":"a","code":"b"}`,
		`{"identity_revision":"rev","code":"a","Code":"b"}`,
		`{"identity_revision":"rev","code":null}`,
		`{"identity_revision":"rev","code":"a","timeout_seconds":null}`,
		`{"code":"a"}`, `{"identity_revision":"rev"}`, `{} []`,
	} {
		if w := call(h, "POST", "/v2/sessions/id/execute", aliceToken, body); w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	for _, body := range []string{`{"identity_revision":"rev","path":"a","content_base64":"!"}`, `{"identity_revision":"rev","path":"a"}`} {
		if w := call(h, "POST", "/v2/sessions/id/files/write", aliceToken, body); w.Code != 400 {
			t.Fatal("invalid file body accepted")
		}
	}
	if b.calls.Load() != 0 {
		t.Fatal("invalid input reached backend")
	}
}
func TestDataCapacityLeavesCloseAndQueryAvailable(t *testing.T) {
	b := &operationBackend{}
	h := handler(t, b, 1)
	h.operations <- struct{}{}
	w := call(h, "POST", "/v2/sessions/id/execute", aliceToken, `{"identity_revision":"rev","code":"wait()"}`)
	if w.Code != 503 {
		t.Fatal("capacity not rejected")
	}
	if w = call(h, "DELETE", "/v2/sessions/id", aliceToken, ""); w.Code != 202 {
		t.Fatal("executions starved close")
	}
	if w = call(h, "GET", "/v2/sessions/id", aliceToken, ""); w.Code != 200 {
		t.Fatal("executions starved observation")
	}
	<-h.operations
}
func TestOperationErrorsHaveExplicitStatus(t *testing.T) {
	b := &operationBackend{}
	h := handler(t, b, 1)
	for _, tc := range []struct {
		code   string
		status int
	}{{"session_busy", 409}, {"identity_changed", 409}, {"execution_timeout", 504}, {"execution_output_limit", 413}, {"operation_outcome_unknown", 503}} {
		b.err = &session.Error{Code: tc.code}
		if w := call(h, "POST", "/v2/sessions/id/execute", aliceToken, `{"identity_revision":"rev","code":"a"}`); w.Code != tc.status {
			t.Fatalf("%s: %d", tc.code, w.Code)
		}
	}
}
