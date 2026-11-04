package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestSlowInventoryDomainCannotConsumePeerBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/slow" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces/fast" {
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"fast","uid":"fast-uid"}}`)
		} else {
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`)
		}
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, QPS: 100, Burst: 100})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	i, err := New(client, map[string]string{"batch": "slow", "session": "fast"}, time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	i.budget = 80 * time.Millisecond
	r := prometheus.NewRegistry()
	i.Register(r)
	if err = i.Poll(context.Background()); err == nil {
		t.Fatal("timeout disappeared")
	}
	body := text(t, r)
	if !strings.Contains(body, `agentbox_inventory_valid{domain="session"} 1`) || !strings.Contains(body, `agentbox_inventory_valid{domain="batch"} 0`) {
		t.Fatal("healthy peer became unknown", body)
	}
	if !strings.Contains(body, `agentbox_inventory_requests_total{domain="batch",reason="timeout",resource="namespaces",verb="get"} 1`) {
		t.Fatal("missing timeout classification", body)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			t.Fatal("invalid JSON")
		}
		if event["msg"] == "inventory read" && event["phase"] == "end" {
			if event["poll_id"] == "" || event["remaining_budget_ms"] == nil || event["duration_ms"] == nil || event["resource"] == nil {
				t.Fatal("unattributed read", event)
			}
		}
	}
}

func TestRuntimeSourcesHaveIndependentDeadlines(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/cadvisor") {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(runtimeFixture(1, 0, 200)))}, nil
	})}
	r, err := NewRuntime([]RuntimeTarget{{Role: "worker", URL: "https://owned/metrics/cadvisor", Client: client}}, map[string]string{"batch": "work"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.budget = 50 * time.Millisecond
	registry := prometheus.NewRegistry()
	registry.MustRegister(r)
	if r.Poll(context.Background()) == nil {
		t.Fatal("source failure disappeared")
	}
	body := text(t, registry)
	if !strings.Contains(body, `agentbox_runtime_observation_valid{node_role="worker",source="cri"} 1`) || !strings.Contains(body, `agentbox_runtime_polls_total{node_role="worker",reason="timeout",source="cadvisor"} 1`) {
		t.Fatal("source failure contaminated peer", body)
	}
}

func TestReadErrorsNeverExposeNativeDetails(t *testing.T) {
	i, _, _, logs := observerSetup(t, "session")
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), pollKey{}, pollContext{"poll", "session"}), time.Second)
	defer cancel()
	done := i.beginRead(ctx, "get", "namespaces", "private-namespace")
	done(fmt.Errorf("Bearer private-token and arbitrary native data"))
	if strings.Contains(logs.String(), "private-token") || !strings.Contains(logs.String(), `"reason":"unavailable"`) {
		t.Fatal("native error leaked", logs.String())
	}
}
