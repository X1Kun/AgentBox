// Package observer samples project Kubernetes state using read-only credentials.
// Scrapes only expose fresh, complete in-memory observations.
package observer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var jobs = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
var pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var workloads = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
var claims = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
var pools = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
var namespacesResource = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

type key struct{ kind, state string }
type sample struct {
	at     time.Time
	valid  bool
	values map[key]float64
}
type Inventory struct {
	client                                         dynamic.Interface
	namespaces                                     map[string]string
	maxAge                                         time.Duration
	now                                            func() time.Time
	started                                        time.Time
	logger                                         *slog.Logger
	mu                                             sync.RWMutex
	pollMu                                         sync.Mutex
	stateMu                                        sync.Mutex
	sequence                                       atomic.Uint64
	budget                                         time.Duration
	namespaceUIDs                                  map[string]string
	values                                         map[string]sample
	seen                                           map[string]map[string]bool
	objects                                        map[string]map[string]string
	validDesc, timestampDesc, itemsDesc, scopeDesc *prometheus.Desc
	checks                                         *prometheus.CounterVec
	reads                                          *prometheus.CounterVec
	readDuration                                   *prometheus.HistogramVec
	events                                         *prometheus.CounterVec
	durations                                      *prometheus.HistogramVec
}

func New(client dynamic.Interface, namespaces map[string]string, maxAge time.Duration, logger *slog.Logger) (*Inventory, error) {
	if client == nil || len(namespaces) == 0 || len(namespaces) > 2 || maxAge < time.Second || maxAge > time.Minute {
		return nil, errors.New("invalid observer scope")
	}
	copyNS := map[string]string{}
	for domain, namespace := range namespaces {
		if (domain != "session" && domain != "batch") || namespace == "" || strings.ContainsAny(namespace, "/, ") {
			return nil, errors.New("invalid observer namespace")
		}
		copyNS[domain] = namespace
	}
	if logger == nil {
		logger = slog.Default()
	}
	i := &Inventory{client: client, namespaces: copyNS, maxAge: maxAge, now: time.Now, started: time.Now(), logger: logger, values: map[string]sample{}, seen: map[string]map[string]bool{}, objects: map[string]map[string]string{}, namespaceUIDs: map[string]string{}}
	i.budget = pollBudget
	i.reads = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_inventory_requests_total", Help: "Inventory reads by finite resource and failure category."}, []string{"domain", "verb", "resource", "reason"})
	i.readDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentbox_inventory_request_duration_seconds", Help: "Inventory backend read duration.", Buckets: prometheus.DefBuckets}, []string{"domain", "verb", "resource", "reason"})
	i.validDesc = prometheus.NewDesc("agentbox_inventory_valid", "One only for a fresh complete namespace observation; missing data is not idle.", []string{"domain"}, nil)
	i.scopeDesc = prometheus.NewDesc("agentbox_observer_scope", "Explicit configured application domains expected by monitoring.", []string{"domain"}, nil)
	i.timestampDesc = prometheus.NewDesc("agentbox_inventory_timestamp_seconds", "Start time of last successful complete namespace observation.", []string{"domain"}, nil)
	i.itemsDesc = prometheus.NewDesc("agentbox_inventory_items", "Current complete inventory, with bounded domain/kind/state labels.", []string{"domain", "kind", "state"}, nil)
	i.checks = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_inventory_polls_total", Help: "Completed namespace polls."}, []string{"domain", "outcome"})
	i.events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_lifecycle_events_total", Help: "Kubernetes lifecycle observations; initial recovery is separated from live events."}, []string{"domain", "kind", "stage", "outcome", "origin"})
	i.durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentbox_lifecycle_duration_seconds", Help: "Durations from Kubernetes timestamps; unavailable times are omitted, not synthesized.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}}, []string{"domain", "stage", "origin"})
	return i, nil
}
func (i *Inventory) Register(registry *prometheus.Registry) {
	registry.MustRegister(i, i.checks, i.events, i.durations, i.reads, i.readDuration)
}
func (i *Inventory) Describe(ch chan<- *prometheus.Desc) {
	ch <- i.validDesc
	ch <- i.timestampDesc
	ch <- i.itemsDesc
	ch <- i.scopeDesc
}
func (i *Inventory) Collect(ch chan<- prometheus.Metric) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	now := i.now()
	for domain := range i.namespaces {
		ch <- prometheus.MustNewConstMetric(i.scopeDesc, prometheus.GaugeValue, 1, domain)
		s := i.values[domain]
		age := now.Sub(s.at)
		valid := s.valid && !s.at.IsZero() && age >= 0 && age < i.maxAge
		flag := 0.
		if valid {
			flag = 1
		}
		ch <- prometheus.MustNewConstMetric(i.validDesc, prometheus.GaugeValue, flag, domain)
		stamp := 0.
		if !s.at.IsZero() {
			stamp = float64(s.at.UnixNano()) / 1e9
		}
		ch <- prometheus.MustNewConstMetric(i.timestampDesc, prometheus.GaugeValue, stamp, domain)
		if valid {
			for k, value := range s.values {
				ch <- prometheus.MustNewConstMetric(i.itemsDesc, prometheus.GaugeValue, value, domain, k.kind, k.state)
			}
		}
	}
}
func (i *Inventory) beginRead(ctx context.Context, verb, resource, namespace string) func(error) {
	started := time.Now()
	p, _ := ctx.Value(pollKey{}).(pollContext)
	i.logger.InfoContext(ctx, "inventory read", "phase", "start", "poll_id", p.id, "domain", p.domain, "verb", verb, "resource", resource, "namespace", namespace, "remaining_budget_ms", remainingMS(ctx))
	return func(err error) {
		reason, status := readReason(err)
		elapsed := time.Since(started)
		i.reads.WithLabelValues(p.domain, verb, resource, reason).Inc()
		i.readDuration.WithLabelValues(p.domain, verb, resource, reason).Observe(elapsed.Seconds())
		i.logger.InfoContext(ctx, "inventory read", "phase", "end", "poll_id", p.id, "domain", p.domain, "verb", verb, "resource", resource, "namespace", namespace, "reason", reason, "http_status", status, "duration_ms", elapsed.Milliseconds(), "remaining_budget_ms", remainingMS(ctx))
	}
}
func (i *Inventory) list(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (items []unstructured.Unstructured, err error) {
	done := i.beginRead(ctx, "list", gvr.Resource, namespace)
	defer func() { done(err) }()
	list, err := i.client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 512})
	if err != nil {
		return nil, err
	}
	if list.GetContinue() != "" || len(list.Items) > 512 {
		return nil, &observationError{reason: "limit_exceeded"}
	}
	return list.Items, nil
}
func str(obj map[string]interface{}, path ...string) string {
	v, _, _ := unstructured.NestedString(obj, path...)
	return v
}
func timestamp(value string) time.Time                    { v, _ := time.Parse(time.RFC3339Nano, value); return v }
func objectTime(obj *unstructured.Unstructured) time.Time { return obj.GetCreationTimestamp().Time }
func conditionTime(obj *unstructured.Unstructured, kind, status string) time.Time {
	values, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, v := range values {
		c, ok := v.(map[string]interface{})
		if ok && c["type"] == kind && c["status"] == status {
			value, _ := c["lastTransitionTime"].(string)
			return timestamp(value)
		}
	}
	return time.Time{}
}
func reference(obj *unstructured.Unstructured, kind string) string {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == kind && ref.Controller != nil && *ref.Controller {
			return string(ref.UID)
		}
	}
	return ""
}
func (i *Inventory) stage(domain, kind string, obj *unstructured.Unstructured, stage, outcome string, at, from time.Time, durationStage string) {
	if at.IsZero() || obj.GetUID() == "" {
		return
	}
	uid := domain + "/" + string(obj.GetUID())
	if i.seen[uid] == nil {
		i.seen[uid] = map[string]bool{}
	}
	if i.seen[uid][stage] {
		return
	}
	i.seen[uid][stage] = true
	origin := "live"
	created := objectTime(obj)
	if created.IsZero() || created.Before(i.started) {
		origin = "initial"
	}
	i.events.WithLabelValues(domain, kind, stage, outcome, origin).Inc()
	fields := []interface{}{"domain", domain, "kind", kind, "object_id", obj.GetName(), "uid", string(obj.GetUID()), "stage", stage, "outcome", outcome, "event_at", at.UTC().Format(time.RFC3339Nano), "observed_at", i.now().UTC().Format(time.RFC3339Nano), "time_source", "kubernetes", "origin", origin}
	if !from.IsZero() && !at.Before(from) && durationStage != "" {
		duration := at.Sub(from).Seconds()
		i.durations.WithLabelValues(domain, durationStage, origin).Observe(duration)
		fields = append(fields, "duration_seconds", duration)
	}
	i.logger.Info("kubernetes lifecycle observation", fields...)
}
func emptyCounts(domain string) map[key]float64 {
	values := map[key]float64{}
	for _, phase := range []string{"pending", "running", "succeeded", "failed", "unknown", "terminating"} {
		values[key{"pod", phase}] = 0
	}
	if domain == "batch" {
		for _, phase := range []string{"queued", "running", "succeeded", "failed", "terminating"} {
			values[key{"job", phase}] = 0
		}
	} else {
		for _, phase := range []string{"creating", "ready", "closing", "closed", "invalidated", "unknown"} {
			values[key{"claim", phase}] = 0
		}
		values[key{"pool", "objects"}] = 0
	}
	return values
}
func (i *Inventory) Poll(ctx context.Context) error {
	// Independent domains share shutdown cancellation, never each other's budget.
	i.pollMu.Lock()
	defer i.pollMu.Unlock()
	failures := make(chan error, len(i.namespaces))
	var workers sync.WaitGroup
	id := fmt.Sprintf("%s-%d", i.started.UTC().Format(time.RFC3339Nano), i.sequence.Add(1))
	domains := []string{}
	for domain := range i.namespaces {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		workers.Add(1)
		go func(domain string) {
			defer workers.Done()
			call, cancel := context.WithTimeout(context.WithValue(ctx, pollKey{}, pollContext{id, domain}), i.budget)
			defer cancel()
			started := i.now()
			err := i.pollDomain(call, domain, started)
			outcome := "complete"
			if err != nil {
				outcome = "error"
				i.mu.Lock()
				previous := i.values[domain]
				previous.valid = false
				i.values[domain] = previous
				i.mu.Unlock()
				failures <- fmt.Errorf("%s inventory: %w", domain, err)
				reason, status := readReason(err)
				i.logger.Warn("inventory unavailable", "poll_id", id, "domain", domain, "reason", reason, "http_status", status, "duration_ms", i.now().Sub(started).Milliseconds(), "remaining_budget_ms", remainingMS(call))
			}
			i.checks.WithLabelValues(domain, outcome).Inc()
		}(domain)
	}
	workers.Wait()
	close(failures)
	var all []error
	for err := range failures {
		all = append(all, err)
	}
	return errors.Join(all...)
}
func (i *Inventory) pollDomain(ctx context.Context, domain string, started time.Time) error {
	namespace := i.namespaces[domain]
	done := i.beginRead(ctx, "get", "namespaces", namespace)
	ns, err := i.client.Resource(namespacesResource).Get(ctx, namespace, metav1.GetOptions{})
	done(err)
	if err != nil {
		return err
	}
	if ns.GetUID() == "" || ns.GetDeletionTimestamp() != nil {
		return &observationError{reason: "identity_unavailable"}
	}
	i.stateMu.Lock()
	if previous := i.namespaceUIDs[domain]; previous != "" && previous != string(ns.GetUID()) {
		i.stateMu.Unlock()
		return &observationError{reason: "identity_changed"}
	}
	i.namespaceUIDs[domain] = string(ns.GetUID())
	i.stateMu.Unlock()
	p, err := i.list(ctx, pods, namespace)
	if err != nil {
		return err
	}
	values := emptyCounts(domain)
	current := map[string]string{}
	admitted := map[string]time.Time{}
	var primary, extra []unstructured.Unstructured
	if domain == "batch" {
		primary, err = i.list(ctx, jobs, namespace)
		if err == nil {
			extra, err = i.list(ctx, workloads, namespace)
		}
	} else {
		primary, err = i.list(ctx, claims, namespace)
		if err == nil {
			extra, err = i.list(ctx, pools, namespace)
		}
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Bookkeeping is shared, but no network read holds this lock.
	i.stateMu.Lock()
	defer i.stateMu.Unlock()
	if domain == "batch" {
		for n := range extra {
			w := &extra[n]
			parent := reference(w, "Job")
			at := conditionTime(w, "Admitted", "True")
			if parent != "" && !at.IsZero() {
				if _, duplicate := admitted[parent]; duplicate {
					return errors.New("ambiguous Job Workloads")
				}
				admitted[parent] = at
			}
		}
		for n := range primary {
			job := &primary[n]
			uid := string(job.GetUID())
			current[uid] = "job"
			phase := "running"
			suspended, _, _ := unstructured.NestedBool(job.Object, "spec", "suspend")
			if suspended {
				phase = "queued"
			}
			finished := conditionTime(job, "Complete", "True")
			outcome := "complete"
			if finished.IsZero() {
				finished = conditionTime(job, "Failed", "True")
				if !finished.IsZero() {
					phase = "failed"
					outcome = "failed"
				}
			} else {
				phase = "succeeded"
			}
			if job.GetDeletionTimestamp() != nil {
				phase = "terminating"
			}
			values[key{"job", phase}]++
			i.stage(domain, "job", job, "submitted", "complete", objectTime(job), time.Time{}, "")
			i.stage(domain, "job", job, "admitted", "complete", admitted[uid], objectTime(job), "queue_wait")
			i.stage(domain, "job", job, "finished", outcome, finished, objectTime(job), "job_finish")
		}
	} else {
		for n := range primary {
			claim := &primary[n]
			current[string(claim.GetUID())] = "claim"
			phase := "creating"
			ready := conditionTime(claim, "Ready", "True")
			closed := timestamp(claim.GetAnnotations()["agentbox.dev/api-closed-at"])
			if !ready.IsZero() {
				phase = "ready"
			}
			if claim.GetAnnotations()["agentbox.dev/api-invalidated"] != "" {
				phase = "invalidated"
			}
			if claim.GetAnnotations()["agentbox.dev/api-close-requested"] == "true" || claim.GetDeletionTimestamp() != nil {
				phase = "closing"
			}
			// Natural expiration need not have been observed by the product API.
			// Expired/terminating is not proof that backing resources are gone.
			deadline := timestamp(str(claim.Object, "spec", "lifecycle", "shutdownTime"))
			if deadline.IsZero() && (phase == "creating" || phase == "ready") {
				phase = "unknown"
			} else if !deadline.IsZero() && !started.Before(deadline) || claimExpired(claim) {
				phase = "closing"
			}
			if !closed.IsZero() {
				phase = "closed"
			}
			values[key{"claim", phase}]++
			i.stage(domain, "claim", claim, "submitted", "complete", objectTime(claim), time.Time{}, "")
			i.stage(domain, "claim", claim, "ready", "complete", ready, objectTime(claim), "claim_ready")
			i.stage(domain, "claim", claim, "closed", "complete", closed, objectTime(claim), "claim_lifetime")
		}
		values[key{"pool", "objects"}] = float64(len(extra))
		poolKnown := true
		desired, total, ready := int64(0), int64(0), int64(0)
		for n := range extra {
			pool := &extra[n]
			current[string(pool.GetUID())] = "pool"
			generation, found, _ := unstructured.NestedInt64(pool.Object, "status", "observedGeneration")
			if !found || generation < pool.GetGeneration() {
				poolKnown = false
			}
			v, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
			desired += v
			v, _, _ = unstructured.NestedInt64(pool.Object, "status", "replicas")
			total += v
			v, _, _ = unstructured.NestedInt64(pool.Object, "status", "readyReplicas")
			ready += v
		}
		values[key{"pool", "status_known"}] = 0
		if poolKnown {
			values[key{"pool", "status_known"}] = 1
			values[key{"pool", "desired"}] = float64(desired)
			values[key{"pool", "members"}] = float64(total)
			values[key{"pool", "ready"}] = float64(ready)
		}
	}
	for n := range p {
		pod := &p[n]
		uid := string(pod.GetUID())
		current[uid] = "pod"
		phase := strings.ToLower(str(pod.Object, "status", "phase"))
		switch phase {
		case "pending", "running", "succeeded", "failed":
		default:
			phase = "unknown"
		}
		if pod.GetDeletionTimestamp() != nil {
			phase = "terminating"
		}
		values[key{"pod", phase}]++
		statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		for _, raw := range statuses {
			status, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			start := timestamp(str(status, "state", "running", "startedAt"))
			end := timestamp(str(status, "state", "terminated", "finishedAt"))
			if start.IsZero() {
				start = timestamp(str(status, "state", "terminated", "startedAt"))
			}
			i.stage(domain, "pod", pod, "started", "complete", start, admitted[reference(pod, "Job")], "pod_startup")
			outcome := "complete"
			code, _, _ := unstructured.NestedInt64(status, "state", "terminated", "exitCode")
			if code != 0 {
				outcome = "failed"
			}
			i.stage(domain, "pod", pod, "finished", outcome, end, start, "execution")
		}
	}
	for uid, kind := range i.objects[domain] {
		if _, exists := current[uid]; !exists {
			i.logger.Info("kubernetes object disappearance", "domain", domain, "kind", kind, "uid", uid, "observed_at", i.now().UTC().Format(time.RFC3339Nano), "time_source", "observer_disappearance")
			delete(i.seen, domain+"/"+uid)
		}
	}
	i.objects[domain] = current
	i.mu.Lock()
	i.values[domain] = sample{at: started, valid: true, values: values}
	i.mu.Unlock()
	return nil
}

func claimExpired(claim *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(claim.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		generation, _ := c["observedGeneration"].(int64)
		if c["type"] == "Ready" && c["status"] == "False" && c["reason"] == "ClaimExpired" && generation == claim.GetGeneration() {
			return true
		}
	}
	return false
}
