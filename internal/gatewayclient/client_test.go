package gatewayclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/X1Kun/AgentBox/internal/api"
)

const testToken = "gateway-client-test-token-at-least-32-characters"

func TestRejectsUnsafeConfiguration(t *testing.T) {
	for _, base := range []string{"http://0.0.0.0:8080", "http://localhost:8080", "http://127.0.0.1:0", "http://127.0.0.1:000", "http://127.0.0.1:8080/path", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080?x=1", "https://127.0.0.1:8080"} {
		if c, err := New(base, testToken); err == nil {
			c.Close()
			t.Fatalf("unsafe URL accepted: %s", base)
		}
	}
}

func TestNeverReplaysAmbiguousExecution(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `{"ready":true}`)
			return
		}
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer s.Close()
	c, err := New(s.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Status(context.Background()).HTTPStatus != 200 {
		t.Fatal("connection warmup failed")
	}
	reply := c.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"})
	if calls.Load() != 1 || reply.HTTPStatus != 502 || !strings.Contains(string(reply.Response), `"outcome_unknown":true`) {
		t.Fatalf("ambiguous execution replayed/hidden: %+v calls=%d", reply, calls.Load())
	}
}

func TestRejectsBrokenResponsesAndRedirects(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"status":`, strings.Repeat(" ", MaxResponseBytes+1)} {
		t.Run("invalid", func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer s.Close()
			c, err := New(s.URL, testToken)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if reply := c.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"}); reply.HTTPStatus != 502 {
				t.Fatalf("broken response accepted: %d", reply.HTTPStatus)
			}
		})
	}
	var followed atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			followed.Add(1)
		}
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(307)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer s.Close()
	c, err := New(s.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if reply := c.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"}); reply.HTTPStatus != 502 || followed.Load() != 0 {
		t.Fatal("redirect followed")
	}
}
