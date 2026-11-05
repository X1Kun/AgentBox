package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/X1Kun/AgentBox/internal/admission/health"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var Workloads = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads"}
var Checks = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "admissionchecks"}
var LocalQueues = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "localqueues"}
var ClusterQueues = schema.GroupVersionResource{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "clusterqueues"}
var Namespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// Controller serializes one health domain. It never writes Job spec, admission,
// other check entries, retryCount, or any task/Pod lifecycle resource.
type Controller struct {
	mu             sync.Mutex
	client         dynamic.Interface
	cfg            Config
	load           func() (Config, error)
	source         health.Source
	newSource      func(Config) (health.Source, error)
	engine         *Engine
	now            func() time.Time
	logger         *slog.Logger
	metrics        *Metrics
	pins           map[string]types.UID
	started        time.Time
	notBefore      time.Time
	lastGrant      time.Time
	lastRead       time.Time
	sample         health.Sample
	sourceErr      error
	leaseNamespace string
	attached       bool
}

func New(client dynamic.Interface, cfg Config, source health.Source, logger *slog.Logger) (*Controller, error) {
	if client == nil || source == nil || cfg.Validate() != nil {
		return nil, errors.New("invalid admission controller")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{client: client, cfg: cfg, source: source, engine: NewEngine(cfg), now: time.Now, logger: logger, pins: map[string]types.UID{}}, nil
}
func (c *Controller) SetMetrics(m *Metrics)               { c.metrics = m }
func (c *Controller) SetLeaderNamespace(namespace string) { c.leaseNamespace = namespace }
func (c *Controller) SetReload(load func() (Config, error), factory func(Config) (health.Source, error)) {
	c.load, c.newSource = load, factory
}

func (c *Controller) pin(key string, obj *unstructured.Unstructured) error {
	if obj.GetUID() == "" || obj.GetDeletionTimestamp() != nil {
		return errors.New("installation_unavailable")
	}
	if old, exists := c.pins[key]; exists && old != obj.GetUID() {
		return errors.New("installation_changed")
	}
	c.pins[key] = obj.GetUID()
	return nil
}

func (c *Controller) reload() error {
	if c.load == nil {
		return nil
	}
	next, err := c.load()
	if err != nil || next.Validate() != nil {
		return errors.New("config_invalid")
	}
	if c.cfg.fingerprint() == next.fingerprint() {
		return nil
	}
	if !c.cfg.sameScope(next) || next.Health.Revision == c.cfg.Health.Revision {
		return errors.New("config_revision_required")
	}
	if c.newSource == nil {
		return errors.New("config_reload_unavailable")
	}
	source, err := c.newSource(next)
	if err != nil {
		return errors.New("config_invalid")
	}
	if closer, ok := c.source.(interface{ Close() }); ok {
		closer.Close()
	}
	c.cfg, c.source, c.engine = next, source, NewEngine(next)
	c.lastRead = time.Time{}
	c.notBefore = c.now().Add(seconds(next.PermitIntervalSeconds + next.RequestTimeoutSeconds))
	c.logger.Info("admission configuration changed", "revision", next.Health.Revision)
	return nil
}

func (c *Controller) installation(ctx context.Context) (*unstructured.Unstructured, error) {
	ns, err := c.client.Resource(Namespaces).Get(ctx, c.cfg.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if ns.GetLabels()["agentbox.dev/session"] != c.cfg.Owner {
		return nil, errors.New("installation_unowned")
	}
	if err = c.pin("namespace", ns); err != nil {
		return nil, err
	}
	lq, err := c.client.Resource(LocalQueues).Namespace(c.cfg.Namespace).Get(ctx, c.cfg.LocalQueue, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if text(lq, "spec", "clusterQueue") != c.cfg.ClusterQueue {
		return nil, errors.New("queue_changed")
	}
	if err = c.pin("local_queue", lq); err != nil {
		return nil, err
	}
	cq, err := c.client.Resource(ClusterQueues).Get(ctx, c.cfg.ClusterQueue, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cq.GetLabels()["agentbox.dev/session"] != c.cfg.Owner {
		return nil, errors.New("installation_unowned")
	}
	if err = c.pin("cluster_queue", cq); err != nil {
		return nil, err
	}
	checks, _, _ := unstructured.NestedSlice(cq.Object, "spec", "admissionChecksStrategy", "admissionChecks")
	c.attached = false
	for _, raw := range checks {
		rule, ok := raw.(map[string]any)
		if ok && rule["name"] == c.cfg.Check {
			flavors, _, _ := unstructured.NestedStringSlice(rule, "onFlavors")
			if len(flavors) > 0 {
				return nil, errors.New("unsupported_flavor_scope")
			}
			c.attached = true
		}
	}
	ac, err := c.client.Resource(Checks).Get(ctx, c.cfg.Check, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if ac.GetLabels()["agentbox.dev/session"] != c.cfg.Owner || text(ac, "spec", "controllerName") != ControllerName || ac.GetAnnotations()[RevisionAnnotation] != c.cfg.Health.Revision {
		return nil, errors.New("check_configuration_mismatch")
	}
	if c.leaseNamespace != "" && ac.GetAnnotations()["agentbox.dev/lease-namespace"] != c.leaseNamespace {
		return nil, errors.New("leader_scope_mismatch")
	}
	if parameters, found, _ := unstructured.NestedFieldNoCopy(ac.Object, "spec", "parameters"); found && parameters != nil {
		return nil, errors.New("unsupported_parameters")
	}
	if err = c.pin("check", ac); err != nil {
		return nil, err
	}
	return ac, nil
}

func text(o *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(o.Object, path...)
	return v
}
func truth(o *unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, v := range conditions {
		m, ok := v.(map[string]any)
		if ok && m["type"] == kind && m["status"] == "True" {
			return true
		}
	}
	return false
}
func checkEntry(o *unstructured.Unstructured, name string) (int, map[string]any, bool) {
	entries, _, _ := unstructured.NestedSlice(o.Object, "status", "admissionChecks")
	for i, v := range entries {
		entry, ok := v.(map[string]any)
		if ok && entry["name"] == name {
			return i, entry, true
		}
	}
	return -1, nil, false
}
func (c *Controller) active(ctx context.Context, ac *unstructured.Unstructured, valid bool, reason string) error {
	conditions, _, _ := unstructured.NestedSlice(ac.Object, "status", "conditions")
	status := "False"
	if valid {
		status = "True"
	}
	index := -1
	for i, v := range conditions {
		m, ok := v.(map[string]any)
		if !ok || m["type"] != "Active" {
			continue
		}
		index = i
		if m["status"] == status && m["reason"] == reason && m["observedGeneration"] == ac.GetGeneration() {
			return nil
		}
	}
	condition := map[string]any{"type": "Active", "status": status, "reason": reason, "message": reason, "observedGeneration": ac.GetGeneration(), "lastTransitionTime": c.now().UTC().Format(time.RFC3339Nano)}
	if index >= 0 {
		old := conditions[index].(map[string]any)
		if old["status"] == status {
			condition["lastTransitionTime"] = old["lastTransitionTime"]
		}
		conditions[index] = condition
	} else {
		conditions = append(conditions, condition)
	}
	ops := fences(ac)
	if _, exists := ac.Object["status"]; !exists {
		ops = append(ops, map[string]any{"op": "add", "path": "/status", "value": map[string]any{}})
	}
	ops = append(ops, map[string]any{"op": "add", "path": "/status/conditions", "value": conditions})
	data, _ := json.Marshal(ops)
	_, err := c.client.Resource(Checks).Patch(ctx, ac.GetName(), types.JSONPatchType, data, metav1.PatchOptions{}, "status")
	return err
}
func fences(o *unstructured.Unstructured) []map[string]any {
	return []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(o.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()}}
}

func (c *Controller) list(ctx context.Context) ([]unstructured.Unstructured, error) {
	var result []unstructured.Unstructured
	continuation := ""
	seen := map[string]bool{}
	for {
		page, err := c.client.Resource(Workloads).Namespace(c.cfg.Namespace).List(ctx, metav1.ListOptions{Limit: 128, Continue: continuation})
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if len(result) > 1024 {
			return nil, errors.New("workload_limit")
		}
		continuation = page.GetContinue()
		if continuation == "" {
			return result, nil
		}
		if seen[continuation] {
			return nil, errors.New("invalid_pagination")
		}
		seen[continuation] = true
	}
}
func (c *Controller) inScope(o *unstructured.Unstructured) bool {
	return o.GetNamespace() == c.cfg.Namespace && text(o, "spec", "queueName") == c.cfg.LocalQueue
}
func eligible(o *unstructured.Unstructured) bool {
	active, found, _ := unstructured.NestedBool(o.Object, "spec", "active")
	reserved := false
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		m, ok := raw.(map[string]any)
		if ok && m["type"] == "QuotaReserved" && m["status"] == "True" && m["observedGeneration"] == o.GetGeneration() {
			reserved = true
		}
	}
	return o.GetDeletionTimestamp() == nil && (!found || active) && !truth(o, "Admitted") && !truth(o, "Finished") && !truth(o, "Evicted") && reserved && text(o, "status", "admission", "clusterQueue") != ""
}

// Step is one domain reconciliation. Watches and a periodic requeue both enter
// here. Every write is based on a fresh direct read, never the informer cache.
func (c *Controller) Step(ctx context.Context) (resultErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	started := c.now()
	if c.started.IsZero() {
		c.started = started
		c.notBefore = started.Add(seconds(c.cfg.PermitIntervalSeconds + c.cfg.RequestTimeoutSeconds))
	}
	defer func() {
		if c.metrics != nil {
			if resultErr != nil {
				c.metrics.Observe(Decision{Degraded, "backend_unavailable", false}, c.sample, c.now())
			}
			c.metrics.Reconcile(started, resultErr)
		}
	}()
	if err := c.reload(); err != nil {
		c.engine.Reset()
		if c.metrics != nil {
			c.metrics.Observe(Decision{Degraded, "config_invalid", false}, health.Sample{}, c.now())
		}
		ac, e := c.client.Resource(Checks).Get(ctx, c.cfg.Check, metav1.GetOptions{})
		if e == nil && text(ac, "spec", "controllerName") == ControllerName && ac.GetUID() == c.pins["check"] {
			_ = c.active(ctx, ac, false, "ConfigurationInvalid")
		}
		return err
	}
	ac, err := c.installation(ctx)
	if err != nil {
		c.engine.Reset()
		return err
	}
	if err = c.active(ctx, ac, true, "ConfigurationValid"); err != nil {
		c.engine.Reset()
		return err
	}
	newObservation := c.lastRead.IsZero() || c.now().Sub(c.lastRead) >= seconds(c.cfg.PollSeconds)
	if newObservation {
		c.sample, c.sourceErr = c.source.Read(ctx)
		c.lastRead = c.now()
	}
	decision := c.engine.Evaluate(c.now(), c.sample, c.sourceErr)
	if newObservation {
		c.logger.Info("admission health", "domain", c.cfg.Health.Domain, "revision", c.cfg.Health.Revision, "state", decision.State, "reason", decision.Reason, "permit", decision.Permit,
			"source_started", c.sample.SourceStarted, "sample_end", c.sample.End, "sample_sequence", c.sample.Sequence, "planned", c.sample.Planned, "timely_correct", c.sample.Timely,
			"errors", c.sample.Errors, "timeouts", c.sample.Timeouts, "rejected", c.sample.Rejected)
	}
	if c.metrics != nil {
		c.metrics.Observe(decision, c.sample, c.now())
	}
	if !c.attached {
		return nil
	}
	objects, err := c.list(ctx)
	if err != nil {
		c.engine.Reset()
		return err
	}
	// Rebuild the latest durable grant on every pass. A restart also starts a
	// fresh health dwell and conservative cooldown, even if old Ready rows vanished.
	for i := range objects {
		o := &objects[i]
		if !c.inScope(o) {
			continue
		}
		_, entry, ok := checkEntry(o, c.cfg.Check)
		if !ok || entry["state"] != "Ready" {
			continue
		}
		stamp, _ := entry["lastTransitionTime"].(string)
		at, e := time.Parse(time.RFC3339Nano, stamp)
		if e != nil || at.After(c.now()) {
			c.engine.Reset()
			return errors.New("grant_time_invalid")
		}
		if at.After(c.lastGrant) {
			c.lastGrant = at
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		a, b := objects[i].GetCreationTimestamp(), objects[j].GetCreationTimestamp()
		if a.Equal(&b) {
			return objects[i].GetName() < objects[j].GetName()
		}
		return a.Before(&b)
	})
	for i := range objects {
		preview := &objects[i]
		if !c.inScope(preview) || !eligible(preview) {
			continue
		}
		o, e := c.client.Resource(Workloads).Namespace(c.cfg.Namespace).Get(ctx, preview.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return e
		}
		if o.GetUID() != preview.GetUID() || !c.inScope(o) || !eligible(o) || text(o, "status", "admission", "clusterQueue") != c.cfg.ClusterQueue {
			continue
		}
		_, entry, ok := checkEntry(o, c.cfg.Check)
		if !ok || entry["state"] != "Pending" {
			continue
		}
		now := c.now()
		stamp, _ := entry["lastTransitionTime"].(string)
		waiting, e := time.Parse(time.RFC3339Nano, stamp)
		if e != nil || waiting.After(now) {
			return errors.New("pending_time_invalid")
		}
		conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
		for _, raw := range conditions {
			m, ok := raw.(map[string]any)
			if ok && m["type"] == "QuotaReserved" && m["status"] == "True" {
				value, _ := m["lastTransitionTime"].(string)
				reserved, e := time.Parse(time.RFC3339Nano, value)
				if e != nil || reserved.After(now) {
					return errors.New("reservation_time_invalid")
				}
				if reserved.After(waiting) {
					waiting = reserved
				}
			}
		}
		action, reason := "", ""
		if !supported(o) {
			action, reason = "Rejected", "unsupported_template"
		} else if now.Sub(waiting) >= seconds(c.cfg.MaxWaitSeconds) {
			action, reason = "Retry", "check_wait_exceeded"
		} else if decision.Permit && c.sample.Reason(c.cfg.Health, now, seconds(c.cfg.MaxAgeSeconds), c.cfg.MinimumSamples) == "ok" && !now.Before(c.notBefore) && (c.lastGrant.IsZero() || now.Sub(c.lastGrant) >= seconds(c.cfg.PermitIntervalSeconds)) {
			action, reason = "Ready", "stable_health"
		}
		if action == "" {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if action == "Ready" {
			// Spend the interval before attempting a possibly ambiguous write.
			c.lastGrant = now
			c.notBefore = now.Add(seconds(c.cfg.PermitIntervalSeconds + c.cfg.RequestTimeoutSeconds))
		}
		e = c.writeCheck(ctx, o, action, reason, now)
		outcome := "committed"
		if e != nil {
			outcome = "unknown"
			if apierrors.IsConflict(e) || apierrors.IsInvalid(e) {
				outcome = "conflict"
			}
		}
		c.logger.Info("admission decision", "workload", o.GetName(), "uid", o.GetUID(), "revision", c.cfg.Health.Revision, "domain", c.cfg.Health.Domain, "sample_sequence", c.sample.Sequence, "sample_end", c.sample.End, "state", decision.State, "action", action, "reason", reason, "outcome", outcome)
		if c.metrics != nil {
			c.metrics.Action(action, outcome, now.Sub(waiting))
		}
		if e != nil {
			c.engine.Reset()
			return e
		}
		if action == "Ready" {
			// Start the successful-write cooldown at the receipt, not its
			// initiation: a slow API reply must not compress two actual grants.
			c.notBefore = c.now().Add(seconds(c.cfg.PermitIntervalSeconds))
			break
		}
	}
	return nil
}

func (c *Controller) writeCheck(ctx context.Context, o *unstructured.Unstructured, state, reason string, at time.Time) error {
	index, entry, ok := checkEntry(o, c.cfg.Check)
	if !ok || entry["state"] != "Pending" || !eligible(o) {
		return errors.New("state_changed")
	}
	entry["state"], entry["message"], entry["lastTransitionTime"] = state, "revision="+c.cfg.Health.Revision+" reason="+reason, at.UTC().Format(time.RFC3339Nano)
	delete(entry, "requeueAfterSeconds")
	if state == "Retry" {
		entry["requeueAfterSeconds"] = int64(c.cfg.RetrySeconds)
	}
	ops := fences(o)
	ops = append(ops, map[string]any{"op": "test", "path": fmt.Sprintf("/status/admissionChecks/%d/name", index), "value": c.cfg.Check}, map[string]any{"op": "replace", "path": fmt.Sprintf("/status/admissionChecks/%d", index), "value": entry})
	data, _ := json.Marshal(ops)
	_, err := c.client.Resource(Workloads).Namespace(c.cfg.Namespace).Patch(ctx, o.GetName(), types.JSONPatchType, data, metav1.PatchOptions{}, "status")
	return err
}

// The first release admits the existing fixed 500m/256Mi, one-Job image shape.
func supported(o *unstructured.Unstructured) bool {
	sets, _, _ := unstructured.NestedSlice(o.Object, "spec", "podSets")
	if len(sets) != 1 {
		return false
	}
	set, ok := sets[0].(map[string]any)
	if !ok {
		return false
	}
	count, _, _ := unstructured.NestedInt64(set, "count")
	if count != 1 {
		return false
	}
	return supportedPod(set)
}
