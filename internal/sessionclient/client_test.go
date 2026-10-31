package sessionclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/session"
)

const testToken = "session-client-test-token-at-least-32-characters"

func testView() session.View {
	return session.View{ID: "ses-test.uid", State: "ready", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), IdentityRevision: "revision"}
}
func TestConfigurationRequiresTLSForRemoteAndRejectsAmbiguousURLs(t *testing.T) {
	for _, base := range []string{"http://example.com:8090", "http://localhost:8090", "http://127.0.0.1:0", "http://user:password@127.0.0.1:8090", "http://127.0.0.1:8090/path", "http://127.0.0.1:8090?x=1", "http://127.0.0.1:8090/#fragment"} {
		if c, e := New(base, testToken, ""); e == nil {
			c.Close()
			t.Errorf("accepted %s", base)
		}
	}
}
func TestTLSCustomCAAndCredentialBoundary(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing scoped credential")
		}
		_ = json.NewEncoder(w).Encode(testView())
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(server.URL, testToken, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if reply := c.Get(context.Background(), "ses-test.uid"); reply.HTTPStatus != 200 {
		t.Fatalf("trusted TLS: %+v", reply)
	}
	if c.transport.Proxy != nil || !c.transport.DisableKeepAlives {
		t.Fatal("proxy/reusable connection enabled")
	}
}
func TestLostExecutionReplyIsUnknownAndNeverReplayed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	c, err := New(server.URL, testToken, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reply := c.Execute(context.Background(), "ses-test.uid", session.ExecuteRequest{IdentityRevision: "revision", Code: "side_effect()"})
	if reply.HTTPStatus != 502 || !strings.Contains(string(reply.Response), `"outcome_unknown":true`) || calls.Load() != 1 {
		t.Fatalf("replayed/lost outcome: %+v count=%d", reply, calls.Load())
	}
}
func TestRedirectNeverForwardsCredentialOrMutation(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	c, _ := New(source.URL, testToken, "")
	defer c.Close()
	reply := c.Execute(context.Background(), "ses-test.uid", session.ExecuteRequest{IdentityRevision: "revision", Code: "print(1)"})
	if reply.HTTPStatus != 502 || targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
func TestEmptyBinaryFileAndMalformedExecutionResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files/write") {
			var in map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&in)
			if string(in["content_base64"]) != `""` {
				t.Errorf("empty file omitted: %s", in["content_base64"])
			}
			_ = json.NewEncoder(w).Encode(session.FileView{SessionID: "ses-test.uid", IdentityRevision: "revision", Path: "empty", Size: 0})
			return
		}
		var request session.ExecuteRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Code == "unexpected_status" {
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(session.ExecutionView{SessionID: "ses-test.uid", IdentityRevision: "revision"})
			return
		}
		_, _ = w.Write([]byte(`{"session_id":"ses-test.uid","identity_revision":"revision","stdout":"","stderr":"","exit_code":null}`))
	}))
	defer server.Close()
	c, _ := New(server.URL, testToken, "")
	defer c.Close()
	if r := c.Write(context.Background(), "ses-test.uid", session.FileRequest{IdentityRevision: "revision", Path: "empty"}); r.HTTPStatus != 200 {
		t.Fatalf("empty write failed: %+v", r)
	}
	if r := c.Execute(context.Background(), "ses-test.uid", session.ExecuteRequest{IdentityRevision: "revision", Code: "print(1)"}); r.HTTPStatus != 502 {
		t.Fatal("malformed success accepted")
	}
	if r := c.Execute(context.Background(), "ses-test.uid", session.ExecuteRequest{IdentityRevision: "revision", Code: "unexpected_status"}); r.HTTPStatus != 502 {
		t.Fatal("unexpected success status accepted")
	}
}
