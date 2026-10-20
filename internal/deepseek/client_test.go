package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNonThinkingToolCallWireFormat(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-test-key" {
			t.Error("wrong provider request")
		}
		var body struct {
			Model    string            `json:"model"`
			Thinking map[string]string `json:"thinking"`
			Tools    []Tool            `json:"tools"`
			Messages []Message         `json:"messages"`
			Stream   bool              `json:"stream"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "configured-model" || body.Thinking["type"] != "disabled" || body.Stream || len(body.Tools) != 1 {
			t.Error("wrong model/transport mode")
		}
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"execute_python","arguments":"{\"code\":\"print(1)\"}"}}]}}]}`)
	}))
	defer s.Close()
	c, err := New(s.URL+"/v1", "provider-test-key", "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	message, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "calculate"}}, []Tool{{Type: "function", Function: Function{Name: "execute_python", Parameters: map[string]any{"type": "object"}}}})
	if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call-1" {
		t.Fatalf("tool call: %+v %v", message, err)
	}
}

func TestDoesNotAcceptPartialCompletions(t *testing.T) {
	for _, response := range []string{
		`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"partial"}}]}`,
		`{"choices":[{"finish_reason":"length","message":{"role":"assistant","tool_calls":[{"id":"call-1"}]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"role":"system","content":"bad"}}]}`,
		`{"choices":[]}`, `{"choices":`,
	} {
		t.Run(response, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response) }))
			defer s.Close()
			c, err := New(s.URL, "key", "test")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Complete(context.Background(), nil, nil); err == nil {
				t.Fatal("partial response accepted")
			}
		})
	}
}

func TestProviderErrorsDoNotExposeBodyOrCredentials(t *testing.T) {
	for _, status := range []int{401, 429, 500, 307} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/again")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "private-provider-body")
		}))
		c, err := New(s.URL, "private-api-key", "test")
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Complete(context.Background(), nil, nil)
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("provider details leaked")
		}
		c.Close()
		s.Close()
	}
}
