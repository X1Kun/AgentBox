package observer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dfake "k8s.io/client-go/dynamic/fake"
	kt "k8s.io/client-go/testing"
)

func object(api, kind, name, uid, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": api, "kind": kind, "metadata": map[string]interface{}{"name": name, "uid": uid, "namespace": namespace, "generation": int64(1)}}}
}
func observerSetup(t *testing.T, domain string) (*Inventory, *dfake.FakeDynamicClient, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	namespace := "private-namespace"
	ns := object("v1", "Namespace", namespace, "namespace-uid", "")
	client := dfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pods: "PodList", jobs: "JobList", workloads: "WorkloadList", claims: "SandboxClaimList", pools: "SandboxWarmPoolList"}, ns)
	log := &bytes.Buffer{}
	inventory, err := New(client, map[string]string{domain: namespace}, 15*time.Second, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	inventory.Register(registry)
	return inventory, client, registry, log
}
func text(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	w := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Body.String()
}
func add(t *testing.T, client *dfake.FakeDynamicClient, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := client.Resource(gvr).Namespace(obj.GetNamespace()).Create(context.Background(), obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}
func TestScrapesNeverQueryKubernetesAndEmptyInventoryIsKnown(t *testing.T) {
	i, c, r, _ := observerSetup(t, "batch")
	before := len(c.Actions())
	for n := 0; n < 10; n++ {
		_ = text(t, r)
	}
	if len(c.Actions()) != before {
		t.Fatal("scrape touched Kubernetes")
	}
	if !strings.Contains(text(t, r), `agentbox_inventory_valid{domain="batch"} 0`) {
		t.Fatal("initial inventory was known")
	}
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	before = len(c.Actions())
	body := text(t, r)
	if !strings.Contains(body, `agentbox_inventory_valid{domain="batch"} 1`) || !strings.Contains(body, `agentbox_inventory_items{domain="batch",kind="job",state="running"} 0`) {
		t.Fatal(body)
	}
	for n := 0; n < 10; n++ {
		_ = text(t, r)
	}
	if len(c.Actions()) != before {
		t.Fatal("scrapes amplified API requests")
	}
}
func TestFailureAndStalenessOmitInventoryInsteadOfInventingIdle(t *testing.T) {
	i, c, r, _ := observerSetup(t, "session")
	now := time.Now()
	i.now = func() time.Time { return now }
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Second)
	body := text(t, r)
	if strings.Contains(body, "agentbox_inventory_items{") {
		t.Fatal("stale inventory exported zeros")
	}
	now = now.Add(time.Second)
	c.PrependReactor("list", "pods", func(kt.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(pods.GroupResource(), "pods", fmt.Errorf("denied"))
	})
	if i.Poll(context.Background()) == nil {
		t.Fatal("failed read accepted")
	}
	body = text(t, r)
	if !strings.Contains(body, `agentbox_inventory_valid{domain="session"} 0`) || strings.Contains(body, "agentbox_inventory_items{") {
		t.Fatal(body)
	}
}
func TestNamespaceRecreationIsNotAdopted(t *testing.T) {
	i, c, r, _ := observerSetup(t, "batch")
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ns := object("v1", "Namespace", "private-namespace", "replacement", "")
	_, err := c.Resource(namespacesResource).Update(context.Background(), ns, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if i.Poll(context.Background()) == nil {
		t.Fatal("namespace replacement accepted")
	}
	if strings.Contains(text(t, r), "agentbox_inventory_items{") {
		t.Fatal("replaced namespace counted")
	}
}
func TestReadInProgressCannotBlockScrapingLastSnapshot(t *testing.T) {
	i, c, r, _ := observerSetup(t, "batch")
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	c.PrependReactor("list", "pods", func(kt.Action) (bool, runtime.Object, error) { close(entered); <-release; return false, nil, nil })
	done := make(chan struct{})
	go func() { defer close(done); _ = i.Poll(context.Background()) }()
	<-entered
	scraped := make(chan struct{})
	go func() { _ = text(t, r); close(scraped) }()
	select {
	case <-scraped:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("scrape blocked on a backend request")
	}
	close(release)
	<-done
}
func TestLifecycleUsesLinkedUIDsAndDoesNotRecountPolling(t *testing.T) {
	i, c, r, logs := observerSetup(t, "batch")
	base := time.Now().UTC().Add(time.Second)
	i.started = base.Add(-time.Second)
	j := object("batch/v1", "Job", "private-job-id", "job-uid", "private-namespace")
	j.SetCreationTimestamp(metav1.NewTime(base))
	_ = unstructured.SetNestedField(j.Object, false, "spec", "suspend")
	_ = unstructured.SetNestedSlice(j.Object, []interface{}{map[string]interface{}{"type": "Complete", "status": "True", "lastTransitionTime": base.Add(10 * time.Second).Format(time.RFC3339Nano)}}, "status", "conditions")
	add(t, c, jobs, j)
	w := object("kueue.x-k8s.io/v1beta2", "Workload", "private-workload-id", "workload-uid", "private-namespace")
	controller := true
	w.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Job", UID: "job-uid", Controller: &controller}})
	_ = unstructured.SetNestedSlice(w.Object, []interface{}{map[string]interface{}{"type": "Admitted", "status": "True", "lastTransitionTime": base.Add(3 * time.Second).Format(time.RFC3339Nano)}}, "status", "conditions")
	add(t, c, workloads, w)
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := text(t, r)
	logSize := logs.Len()
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := text(t, r)
	if strings.Contains(body, "private-job-id") || strings.Contains(body, "job-uid") || strings.Contains(body, "private-namespace") {
		t.Fatal("object identifiers became labels")
	}
	if !strings.Contains(body, `agentbox_lifecycle_duration_seconds_sum{domain="batch",origin="live",stage="queue_wait"} 3`) {
		t.Fatal(body)
	}
	if logs.Len() != logSize {
		t.Fatal("polling recounted lifecycle events")
	}
	if !strings.Contains(logs.String(), "job-uid") {
		t.Fatal("event log lost UID")
	}
	_ = first
}
func TestRecoveredEventsAreSeparatedFromLiveThroughput(t *testing.T) {
	i, c, r, _ := observerSetup(t, "batch")
	j := object("batch/v1", "Job", "old", "old-uid", "private-namespace")
	j.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	add(t, c, jobs, j)
	if err := i.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := text(t, r)
	if !strings.Contains(body, `origin="initial"`) {
		t.Fatal(body)
	}
	// The same timestamps recovered by a new observer stay in the initial series.
	second, err := New(c, map[string]string{"batch": "private-namespace"}, 15*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	next := prometheus.NewRegistry()
	second.Register(next)
	if err = second.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text(t, next), `origin="initial"`) {
		t.Fatal("restart invented live arrivals")
	}
}

func TestClaimNaturalExpirationIsClosingWithoutProductConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, deadline, condition, phase string
		generation                       int64
	}{
		{"elapsed", "past", "", "closing", 1},
		{"upstream-expired", "future", "False", "closing", 1},
		{"stale-expiry-condition", "future", "False", "creating", 0},
		{"ready", "future", "True", "ready", 1},
		{"unreadable-deadline", "invalid", "", "unknown", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, c, r, logs := observerSetup(t, "session")
			now := time.Now()
			i.now = func() time.Time { return now }
			claim := object("extensions.agents.x-k8s.io/v1beta1", "SandboxClaim", "claim", "claim-uid", "private-namespace")
			deadline := now.Add(time.Hour).Format(time.RFC3339Nano)
			if tc.deadline == "past" {
				deadline = now.Add(-time.Hour).Format(time.RFC3339Nano)
			}
			if tc.deadline == "invalid" {
				deadline = "invalid"
			}
			_ = unstructured.SetNestedField(claim.Object, deadline, "spec", "lifecycle", "shutdownTime")
			if tc.condition != "" {
				_ = unstructured.SetNestedSlice(claim.Object, []interface{}{map[string]interface{}{"type": "Ready", "status": tc.condition, "reason": "ClaimExpired", "observedGeneration": tc.generation, "lastTransitionTime": now.Add(-time.Minute).Format(time.RFC3339Nano)}}, "status", "conditions")
			}
			add(t, c, claims, claim)
			if err := i.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			body := text(t, r)
			if !strings.Contains(body, `kind="claim",state="`+tc.phase+`"} 1`) || strings.Contains(body, `kind="claim",state="closed"} 1`) || strings.Contains(logs.String(), `"stage":"closed"`) {
				t.Fatal(body, logs.String())
			}
			for _, action := range c.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "create" {
					t.Fatal("observer wrote lifecycle state")
				}
			}
		})
	}
}
