package observer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type RuntimeTarget struct {
	Role, URL string
	Client    *http.Client
}
type counter struct {
	value float64
	at    time.Time
}
type resourceKey struct{ domain, role, resource, source string }
type runtimeSample struct {
	at     time.Time
	valid  bool
	values map[resourceKey]float64
}
type Runtime struct {
	targets                        []RuntimeTarget
	namespaces                     map[string]string
	maxAge                         time.Duration
	now                            func() time.Time
	mu                             sync.RWMutex
	pollMu                         sync.Mutex
	values                         map[string]runtimeSample
	previous                       map[string]map[string]counter
	validDesc, timeDesc, valueDesc *prometheus.Desc
	checks                         *prometheus.CounterVec
	durations                      *prometheus.HistogramVec
	budget                         time.Duration
}

func NewRuntime(targets []RuntimeTarget, namespaces map[string]string, maxAge time.Duration) (*Runtime, error) {
	if len(targets) > 2 || len(targets) == 0 || maxAge < time.Second {
		return nil, errors.New("invalid runtime observer scope")
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if (t.Role != "worker" && t.Role != "control-plane") || seen[t.Role] || !strings.HasPrefix(t.URL, "https://") || t.Client == nil {
			return nil, errors.New("runtime targets require unique owned-node roles and verified HTTPS")
		}
		seen[t.Role] = true
	}
	r := &Runtime{targets: targets, namespaces: map[string]string{}, maxAge: maxAge, now: time.Now, values: map[string]runtimeSample{}, previous: map[string]map[string]counter{}}
	r.budget = pollBudget
	for domain, namespace := range namespaces {
		r.namespaces[namespace] = domain
	}
	r.validDesc = prometheus.NewDesc("agentbox_runtime_observation_valid", "Fresh complete kubelet response per source; unsupported resources remain absent.", []string{"node_role", "source"}, nil)
	r.timeDesc = prometheus.NewDesc("agentbox_runtime_observation_timestamp_seconds", "Start time of last successful source response.", []string{"node_role", "source"}, nil)
	r.valueDesc = prometheus.NewDesc("agentbox_runtime_resource", "Container resources by source; rates require two fresh samples. Excludes POD/parent cgroups and does not measure outside-Pod runtime cost.", []string{"node_role", "domain", "role", "resource", "source"}, nil)
	r.checks = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_runtime_polls_total", Help: "Kubelet reads by finite source and reason."}, []string{"node_role", "source", "reason"})
	r.durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentbox_runtime_poll_duration_seconds", Help: "Kubelet source read duration.", Buckets: prometheus.DefBuckets}, []string{"node_role", "source", "reason"})
	return r, nil
}
func (r *Runtime) Describe(ch chan<- *prometheus.Desc) {
	r.checks.Describe(ch)
	r.durations.Describe(ch)
	ch <- r.validDesc
	ch <- r.timeDesc
	ch <- r.valueDesc
}
func (r *Runtime) Collect(ch chan<- prometheus.Metric) {
	r.checks.Collect(ch)
	r.durations.Collect(ch)
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.now()
	for _, target := range r.targets {
		for _, source := range []string{"cri", "cadvisor"} {
			s := r.values[target.Role+"/"+source]
			age := now.Sub(s.at)
			valid := s.valid && !s.at.IsZero() && age >= 0 && age < r.maxAge
			flag := 0.
			if valid {
				flag = 1
			}
			ch <- prometheus.MustNewConstMetric(r.validDesc, prometheus.GaugeValue, flag, target.Role, source)
			stamp := 0.
			if !s.at.IsZero() {
				stamp = float64(s.at.UnixNano()) / 1e9
			}
			ch <- prometheus.MustNewConstMetric(r.timeDesc, prometheus.GaugeValue, stamp, target.Role, source)
			if valid {
				for key, value := range s.values {
					ch <- prometheus.MustNewConstMetric(r.valueDesc, prometheus.GaugeValue, value, target.Role, key.domain, key.role, key.resource, key.source)
				}
			}
		}
	}
}
func (r *Runtime) Poll(ctx context.Context) error {
	r.pollMu.Lock()
	defer r.pollMu.Unlock()
	failures := make(chan error, len(r.targets)*2)
	var workers sync.WaitGroup
	for _, target := range r.targets {
		for _, source := range []string{"cri", "cadvisor"} {
			workers.Add(1)
			go func(target RuntimeTarget, source string) {
				defer workers.Done()
				call, cancel := context.WithTimeout(ctx, r.budget)
				defer cancel()
				started := r.now()
				cache := target.Role + "/" + source
				values, err := r.read(call, target, source, started)
				reason, status := readReason(err)
				r.checks.WithLabelValues(target.Role, source, reason).Inc()
				r.durations.WithLabelValues(target.Role, source, reason).Observe(r.now().Sub(started).Seconds())
				slog.InfoContext(call, "runtime observation", "node_role", target.Role, "source", source, "reason", reason, "http_status", status, "duration_ms", r.now().Sub(started).Milliseconds(), "remaining_budget_ms", remainingMS(call))
				r.mu.Lock()
				if err != nil {
					s := r.values[cache]
					s.valid = false
					r.values[cache] = s
					failures <- fmt.Errorf("%s %s runtime: %w", target.Role, source, err)
				} else {
					r.values[cache] = runtimeSample{at: started, valid: true, values: values}
				}
				r.mu.Unlock()
			}(target, source)
		}
	}
	workers.Wait()
	close(failures)
	var all []error
	for err := range failures {
		all = append(all, err)
	}
	return errors.Join(all...)
}
func (r *Runtime) read(ctx context.Context, target RuntimeTarget, source string, started time.Time) (map[resourceKey]float64, error) {
	url := target.URL
	if source == "cri" {
		url = strings.TrimSuffix(url, "/metrics/cadvisor") + "/metrics/resource"
	}
	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	response, err := target.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		reason, _ := readReason(&apiStatusError{code: response.StatusCode})
		return nil, &observationError{reason: reason, status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return nil, errors.New("kubelet metrics response invalid")
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(data)))
	if err != nil {
		return nil, errors.New("kubelet metrics parse failed")
	}
	if failed := families["resource_scrape_error"]; failed != nil {
		for _, metric := range failed.Metric {
			if metric.Gauge.GetValue() != 0 {
				return nil, errors.New("kubelet resource provider unavailable")
			}
		}
	}
	values := map[resourceKey]float64{}
	unknown := map[resourceKey]bool{}
	current := map[string]counter{}
	r.mu.RLock()
	previousValues := r.previous[target.Role+"/"+source]
	r.mu.RUnlock()
	// CRI container identity includes its start time so a same-named replacement
	// cannot turn a counter increase into fictitious usage. cAdvisor's optional
	// leaf stats are accepted only when its own CPU series exists for that leaf.
	starts := map[string]string{}
	capable := map[string]bool{}
	labelsOf := func(metricLabels map[string]string) string {
		return metricLabels["namespace"] + "\x00" + metricLabels["pod"] + "\x00" + metricLabels["container"]
	}
	for _, name := range []string{"container_start_time_seconds", "container_cpu_usage_seconds_total"} {
		family := families[name]
		if family == nil {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if name == "container_start_time_seconds" && metric.Gauge != nil && metric.Gauge.GetValue() > 0 {
				starts[labelsOf(labels)] = fmt.Sprint(metric.Gauge.GetValue())
			} else if name == "container_cpu_usage_seconds_total" && labels["container"] != "" && metric.Counter != nil {
				capable[labels["id"]] = true
			}
		}
	}
	definitions := []struct {
		name, resource string
		rate           bool
	}{
		{"container_cpu_usage_seconds_total", "cpu_cores", true},
		{"container_cpu_cfs_throttled_seconds_total", "throttled_seconds_per_second", true},
		{"container_memory_working_set_bytes", "working_set_bytes", false},
		{"container_memory_rss", "rss_bytes", false},
		{"container_fs_reads_bytes_total", "read_bytes_per_second", true},
		{"container_fs_writes_bytes_total", "write_bytes_per_second", true},
	}
	for _, definition := range definitions {
		if (source == "cri") != (definition.resource == "cpu_cores" || definition.resource == "working_set_bytes") {
			continue
		}
		family := families[definition.name]
		if family == nil {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			domain, ok := r.namespaces[labels["namespace"]]
			container := labels["container"]
			if !ok || container == "" || container == "POD" {
				continue
			}
			if source == "cadvisor" && (labels["id"] == "" || !capable[labels["id"]]) {
				continue
			}
			if definition.resource == "cpu_cores" && labels["cpu"] != "" && labels["cpu"] != "total" {
				continue
			}
			role := "other"
			switch container {
			case "api":
				role = "api"
			case "sandboxd", "compute", "image-worker", "prepare":
				role = "compute"
			}
			key := resourceKey{domain, role, definition.resource, source}
			at := started
			if metric.TimestampMs != nil {
				at = time.UnixMilli(metric.GetTimestampMs())
				if age := r.now().Sub(at); age < 0 || age > r.maxAge {
					unknown[key] = true
					continue
				}
			}
			value := 0.
			if metric.Gauge != nil {
				value = metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				value = metric.Counter.GetValue()
			} else {
				continue
			}
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, errors.New("negative runtime counter/gauge")
			}
			if definition.rate {
				identity := definition.name + "\x00" + labels["id"] + "\x00" + labelsOf(labels) + "\x00" + labels["device"]
				if source == "cri" {
					birth := starts[labelsOf(labels)]
					if birth == "" || labels["pod"] == "" {
						unknown[key] = true
						continue
					}
					identity += "\x00" + birth
				}
				previous, known := previousValues[identity]
				current[identity] = counter{value, at}
				elapsed := at.Sub(previous.at).Seconds()
				if !known || elapsed <= 0 || value < previous.value || at.Sub(previous.at) > r.maxAge {
					unknown[key] = true
					continue
				}
				value = (value - previous.value) / elapsed
			}
			values[key] += value
			if len(current) > 8192 {
				return nil, errors.New("runtime counter inventory exceeds bounded scope")
			}
		}
	}
	for key := range unknown {
		delete(values, key)
	}
	r.mu.Lock()
	r.previous[target.Role+"/"+source] = current
	r.mu.Unlock()
	return values, nil
}
