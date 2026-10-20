// Package deepseek implements the non-streaming, non-thinking Tool Calls API.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Client struct {
	endpoint, key, model string
	http                 *http.Client
	transport            *http.Transport
}

func New(base, key, model string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("invalid DeepSeek base URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("DeepSeek requires HTTPS; HTTP is allowed only for loopback test servers")
		}
	}
	if key == "" || len(key) > 4096 || strings.IndexFunc(key, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("DEEPSEEK_API_KEY must be a nonempty visible ASCII token")
	}
	if model == "" || len(model) > 128 || strings.IndexFunc(model, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("invalid DeepSeek model")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxConnsPerHost = 2
	t.MaxIdleConnsPerHost = 2
	t.DisableCompression = true
	h := &http.Client{Transport: t, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{endpoint: strings.TrimRight(u.String(), "/") + "/chat/completions", key: key, model: model, http: h, transport: t}, nil
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	body, err := json.Marshal(map[string]any{"model": c.model, "messages": messages, "tools": tools, "stream": false, "max_tokens": 4096, "thinking": map[string]string{"type": "disabled"}})
	if err != nil {
		return Message{}, errors.New("cannot encode DeepSeek request")
	}
	if len(body) > 2<<20 {
		return Message{}, errors.New("DeepSeek conversation exceeds 2MiB request budget")
	}
	// A failed completion is not automatically retried. No replay body is installed.
	r, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return Message{}, errors.New("invalid DeepSeek request")
	}
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.key)
	response, err := c.http.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, errors.New("DeepSeek transport failed; no automatic retry")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return Message{}, fmt.Errorf("DeepSeek returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return Message{}, errors.New("DeepSeek returned an incomplete or oversized response")
	}
	var decoded struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &decoded) != nil || len(decoded.Choices) != 1 {
		return Message{}, errors.New("invalid DeepSeek completion")
	}
	choice := decoded.Choices[0]
	if choice.Message.Role != "assistant" {
		return Message{}, errors.New("DeepSeek response role must be assistant")
	}
	if len(choice.Message.ToolCalls) > 0 {
		if choice.FinishReason != "tool_calls" {
			return Message{}, errors.New("incomplete DeepSeek tool call")
		}
	} else if choice.FinishReason != "stop" || choice.Message.Content == "" {
		return Message{}, errors.New("DeepSeek did not return a complete answer")
	}
	return choice.Message, nil
}
