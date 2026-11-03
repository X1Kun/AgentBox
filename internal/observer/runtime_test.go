package observer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func runtimeFixture(cpu, throttle, memory float64) string {
	return fmt.Sprintf(`# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{namespace="work",container="image-worker",id="/leaf",pod="private-pod-id",cpu="total"} %g
container_cpu_usage_seconds_total{namespace="work",container="POD",id="/parent"} 9999
container_cpu_usage_seconds_total{namespace="foreign",container="image-worker",id="/foreign"} 9999
# TYPE container_cpu_cfs_throttled_seconds_total counter
container_cpu_cfs_throttled_seconds_total{namespace="work",container="image-worker",id="/leaf"} %g
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{namespace="work",container="image-worker",id="/leaf"} %g
# TYPE container_start_time_seconds gauge
container_start_time_seconds{namespace="work",container="image-worker",pod="private-pod-id"} 100
`, cpu, throttle, memory)
}
func TestRuntimeRatesExcludeParentsAndDoNotInventFirstSampleIdle(t *testing.T) {
	now := time.Now()
	data := runtimeFixture(10, 1, 200)
	calls := 0
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	r, err := NewRuntime([]RuntimeTarget{{Role: "worker", URL: "https://owned-node/metrics/cadvisor", Client: client}}, map[string]string{"batch": "work"}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return now }
	registry := prometheus.NewRegistry()
	registry.MustRegister(r)
	if err = r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := text(t, registry)
	if strings.Contains(body, `resource="cpu_cores"`) {
		t.Fatal("one sample invented a CPU rate")
	}
	now = now.Add(5 * time.Second)
	data = runtimeFixture(12.5, 1.5, 250)
	if err = r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	body = text(t, registry)
	if !strings.Contains(body, `agentbox_runtime_resource{domain="batch",node_role="worker",resource="cpu_cores",role="compute",source="cri"} 0.5`) {
		t.Fatal(body)
	}
	if strings.Contains(body, "private-pod-id") || strings.Contains(body, "foreign") {
		t.Fatal("unbounded/foreign identity leaked")
	}
	before := calls
	for n := 0; n < 10; n++ {
		_ = text(t, registry)
	}
	if calls != before {
		t.Fatal("scrapes touched kubelet")
	}
	now = now.Add(16 * time.Second)
	if strings.Contains(text(t, registry), "agentbox_runtime_resource{") {
		t.Fatal("stale resources appeared idle")
	}
}
func TestRuntimeCounterResetOmitsRateAndHTTPFailureInvalidatesSnapshot(t *testing.T) {
	now := time.Now()
	status := 200
	data := runtimeFixture(10, 1, 200)
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	r, _ := NewRuntime([]RuntimeTarget{{Role: "worker", URL: "https://node/metrics/cadvisor", Client: client}}, map[string]string{"batch": "work"}, 15*time.Second)
	r.now = func() time.Time { return now }
	registry := prometheus.NewRegistry()
	registry.MustRegister(r)
	_ = r.Poll(context.Background())
	now = now.Add(5 * time.Second)
	data = runtimeFixture(1, 0, 100)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text(t, registry), `resource="cpu_cores"`) {
		t.Fatal("counter reset became CPU usage")
	}
	status = 403
	if r.Poll(context.Background()) == nil {
		t.Fatal("forbidden kubelet read accepted")
	}
	body := text(t, registry)
	if !strings.Contains(body, `agentbox_runtime_observation_valid{node_role="worker",source="cri"} 0`) || strings.Contains(body, "agentbox_runtime_resource{") {
		t.Fatal(body)
	}
}

func TestRuntimeCRIReplacementStaleSamplesAndUnsupportedCAdvisor(t *testing.T) {
	now := time.Now()
	start := "100"
	cpu := 10.
	cadvisor := `# TYPE container_memory_rss gauge
container_memory_rss{namespace="work",container="sandboxd",id="/unaccounted"} 0
# TYPE container_cpu_cfs_throttled_seconds_total counter
container_cpu_cfs_throttled_seconds_total{namespace="work",container="sandboxd",id="/unaccounted"} 0
`
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		data := cadvisor
		if req.URL.Path == "/metrics/resource" {
			data = fmt.Sprintf("# TYPE container_cpu_usage_seconds_total counter\ncontainer_cpu_usage_seconds_total{namespace=\"work\",container=\"sandboxd\",pod=\"same-name\"} %g %d\n# TYPE container_start_time_seconds gauge\ncontainer_start_time_seconds{namespace=\"work\",container=\"sandboxd\",pod=\"same-name\"} %s\n", cpu, now.UnixMilli(), start)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	r, _ := NewRuntime([]RuntimeTarget{{Role: "worker", URL: "https://node/metrics/cadvisor", Client: client}}, map[string]string{"session": "work"}, 15*time.Second)
	r.now = func() time.Time { return now }
	registry := prometheus.NewRegistry()
	registry.MustRegister(r)
	_ = r.Poll(context.Background())
	now = now.Add(5 * time.Second)
	cpu = 15
	_ = r.Poll(context.Background())
	if body := text(t, registry); !strings.Contains(body, `resource="cpu_cores",role="compute",source="cri"} 1`) || strings.Contains(body, `resource="rss_bytes"`) {
		t.Fatal(body)
	}
	// A replacement with a higher counter still needs its own second sample.
	start = "200"
	now = now.Add(5 * time.Second)
	cpu = 100
	_ = r.Poll(context.Background())
	if body := text(t, registry); strings.Contains(body, `resource="cpu_cores"`) || strings.Contains(body, `resource="rss_bytes"`) || strings.Contains(body, `resource="throttled_seconds_per_second"`) {
		t.Fatal(body)
	}
	// HTTP success does not make an old explicit sample timestamp fresh.
	observed := now.Add(time.Minute)
	r.now = func() time.Time { return observed }
	_ = r.Poll(context.Background())
	if body := text(t, registry); strings.Contains(body, `resource="cpu_cores"`) {
		t.Fatal(body)
	}
}
