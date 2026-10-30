// Package session adapts the upstream Claim lifecycle. Kubernetes remains the source of truth.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

var Claims = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
var Pools = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
var Sandboxes = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
var Pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var Services = schema.GroupVersionResource{Version: "v1", Resource: "services"}

const ownerKey = "agentbox.dev/api-owner"
const instanceKey = "agentbox.dev/api-instance"
const managedKey = "app.kubernetes.io/managed-by"
const digestKey = "agentbox.dev/api-request-digest"
const ttlKey = "agentbox.dev/api-ttl-seconds"
const poolKey = "agentbox.dev/api-pool-uid"
const bindingKey = "agentbox.dev/api-binding"
const invalidKey = "agentbox.dev/api-invalidated"
const closeKey = "agentbox.dev/api-close-requested"

var dns = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var principalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
var requestPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var idPattern = regexp.MustCompile(`^(ses-[a-f0-9]{40})\.([a-z0-9-]{1,64})$`)

type Error struct {
	Code      string
	SessionID string
}

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{Code: code} }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "backend_unavailable"
}

type Config struct {
	Namespace, Pool, Instance, RuntimeClass string
	DefaultTTL, MaxTTL                      int
	CloseWait                               time.Duration
}
type CreateRequest struct {
	RequestID  string `json:"request_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}
type View struct {
	ID               string `json:"session_id"`
	State            string `json:"state"`
	Reason           string `json:"reason,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	IdentityRevision string `json:"identity_revision,omitempty"`
	CleanupComplete  bool   `json:"cleanup_complete"`
	OperationActive  bool   `json:"operation_active"`
}
type Ref struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}
type Binding struct {
	Version int `json:"version"`
	Sandbox Ref `json:"sandbox"`
	Pod     Ref `json:"pod"`
	Service Ref `json:"service"`
}
type Service struct {
	client  dynamic.Interface
	cfg     Config
	now     func() time.Time
	runtime Runtime
	logger  *slog.Logger
}

func ValidPrincipal(value string) bool { return principalPattern.MatchString(value) }
func New(client dynamic.Interface, cfg Config) (*Service, error) {
	if client == nil || !dns.MatchString(cfg.Namespace) || !dns.MatchString(cfg.Pool) || !dns.MatchString(cfg.Instance) {
		return nil, errors.New("invalid session backend configuration")
	}
	if cfg.RuntimeClass == "" {
		cfg.RuntimeClass = "agentbox-gvisor"
	}
	if !dns.MatchString(cfg.RuntimeClass) {
		return nil, errors.New("invalid runtime class")
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 600
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 3600
	}
	if cfg.DefaultTTL < 60 || cfg.MaxTTL < cfg.DefaultTTL || cfg.MaxTTL > 86400 || cfg.CloseWait < 0 || cfg.CloseWait > 10*time.Second {
		return nil, errors.New("invalid lifecycle budgets")
	}
	return &Service{client: client, cfg: cfg, now: time.Now}, nil
}
func hash(value string) string { v := sha256.Sum256([]byte(value)); return hex.EncodeToString(v[:]) }
func (s *Service) resource(g schema.GroupVersionResource) dynamic.ResourceInterface {
	return s.client.Resource(g).Namespace(s.cfg.Namespace)
}
func (s *Service) Ready(ctx context.Context) error {
	_, err := s.resource(Pools).Get(ctx, s.cfg.Pool, metav1.GetOptions{})
	return err
}
func (s *Service) request(principal string, r CreateRequest) (string, int, string, error) {
	if !ValidPrincipal(principal) || !requestPattern.MatchString(r.RequestID) {
		return "", 0, "", fail("invalid_request")
	}
	ttl := r.TTLSeconds
	if ttl == 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl < 60 || ttl > s.cfg.MaxTTL {
		return "", 0, "", fail("invalid_request")
	}
	name := "ses-" + hash(s.cfg.Instance + "\x00" + principal + "\x00" + r.RequestID)[:40]
	encoded, _ := json.Marshal(struct {
		Pool string
		TTL  int
	}{s.cfg.Pool, ttl})
	return name, ttl, hash(string(encoded)), nil
}
func (s *Service) owned(c *unstructured.Unstructured, principal string) error {
	labels := c.GetLabels()
	a := c.GetAnnotations()
	if labels[ownerKey] != hash(principal)[:32] || labels[instanceKey] != s.cfg.Instance || labels[managedKey] != "agentbox-api" {
		return fail("not_found")
	}
	pool, _, _ := unstructured.NestedString(c.Object, "spec", "warmPoolRef", "name")
	policy, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownPolicy")
	if pool != s.cfg.Pool || policy != "Retain" || len(a[digestKey]) != 64 || c.GetUID() == "" || c.GetResourceVersion() == "" {
		return fail("integrity_error")
	}
	return nil
}
func sessionID(c *unstructured.Unstructured) string { return c.GetName() + "." + string(c.GetUID()) }
func (s *Service) load(ctx context.Context, principal, id string) (*unstructured.Unstructured, error) {
	parts := idPattern.FindStringSubmatch(id)
	if !ValidPrincipal(principal) || parts == nil {
		return nil, fail("not_found")
	}
	c, err := s.resource(Claims).Get(ctx, parts[1], metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fail("not_found")
	}
	if err != nil {
		return nil, err
	}
	if err := s.owned(c, principal); err != nil {
		return nil, err
	}
	if string(c.GetUID()) != parts[2] {
		return nil, fail("identity_changed")
	}
	return c, nil
}

// Create invokes one named create action. A lost reply is resolved by the same stable name.
func (s *Service) Create(ctx context.Context, principal string, r CreateRequest) (View, bool, error) {
	name, ttl, digest, err := s.request(principal, r)
	if err != nil {
		return View{}, false, err
	}
	c, err := s.resource(Claims).Get(ctx, name, metav1.GetOptions{})
	replay := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return View{}, false, err
	}
	if apierrors.IsNotFound(err) {
		pool, e := s.resource(Pools).Get(ctx, s.cfg.Pool, metav1.GetOptions{})
		if e != nil {
			return View{}, false, e
		}
		if pool.GetUID() == "" {
			return View{}, false, fail("integrity_error")
		}
		deadline := metav1.NewTime(s.now().UTC().Add(time.Duration(ttl) * time.Second).Truncate(time.Second))
		typed := &ext.SandboxClaim{TypeMeta: metav1.TypeMeta{APIVersion: "extensions.agents.x-k8s.io/v1beta1", Kind: "SandboxClaim"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.cfg.Namespace, Labels: map[string]string{ownerKey: hash(principal)[:32], instanceKey: s.cfg.Instance, managedKey: "agentbox-api"},
				Annotations: map[string]string{digestKey: digest, ttlKey: strconv.Itoa(ttl), poolKey: string(pool.GetUID())}},
			Spec: ext.SandboxClaimSpec{WarmPoolRef: ext.SandboxWarmPoolRef{Name: s.cfg.Pool}, Lifecycle: &ext.Lifecycle{ShutdownTime: &deadline, ShutdownPolicy: ext.ShutdownPolicyRetain}}}
		object, e := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
		if e != nil {
			return View{}, false, e
		}
		c, err = s.resource(Claims).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsTooManyRequests(err) {
				return View{}, false, fail("admission_rejected")
			}
			// No second application-level create, even on an ambiguous transport failure.
			c, e = s.resource(Claims).Get(ctx, name, metav1.GetOptions{})
			if e != nil {
				return View{}, false, &Error{Code: "create_outcome_unknown"}
			}
			replay = true
		}
	}
	if err := s.owned(c, principal); err != nil {
		return View{}, false, err
	}
	if c.GetAnnotations()[digestKey] != digest {
		return View{}, false, fail("request_conflict")
	}
	// Effective TTL/expiry is preserved on replay, including closed records.
	v, e := s.Get(ctx, principal, sessionID(c))
	return v, replay, e
}

func controllerRef(obj metav1.Object, uid string) bool {
	for _, v := range obj.GetOwnerReferences() {
		if string(v.UID) == uid && v.Controller != nil && *v.Controller {
			return true
		}
	}
	return false
}
func condition(obj *unstructured.Unstructured, typ, status, reason string) bool {
	values, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, v := range values {
		m, ok := v.(map[string]any)
		if ok && m["type"] == typ && m["status"] == status && m["observedGeneration"] == obj.GetGeneration() && (reason == "" || m["reason"] == reason) {
			return true
		}
	}
	return false
}
func readBinding(c *unstructured.Unstructured) (*Binding, error) {
	text := c.GetAnnotations()[bindingKey]
	if text == "" {
		return nil, nil
	}
	var b Binding
	if json.Unmarshal([]byte(text), &b) != nil || b.Version != 1 || b.Sandbox.Name == "" || b.Sandbox.UID == "" || b.Pod.Name == "" || b.Pod.UID == "" || b.Service.Name == "" || b.Service.UID == "" {
		return nil, fail("integrity_error")
	}
	return &b, nil
}
func revision(b *Binding) string {
	if b == nil {
		return ""
	}
	raw, _ := json.Marshal(b)
	return hash(string(raw))[:32]
}
func ref(obj metav1.Object) Ref { return Ref{obj.GetName(), string(obj.GetUID())} }
func (s *Service) list(ctx context.Context, g schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	v, e := s.resource(g).List(ctx, metav1.ListOptions{})
	if e != nil {
		return nil, e
	}
	return v.Items, nil
}

// patch tests both UID and resourceVersion; conflicts cause a bounded re-read, never adoption.
func (s *Service) patch(ctx context.Context, c *unstructured.Unstructured, fields map[string]string, deadline string) error {
	ops := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(c.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": c.GetResourceVersion()}}
	for k, v := range fields {
		ops = append(ops, map[string]any{"op": "add", "path": "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"), "value": v})
	}
	if deadline != "" {
		ops = append(ops, map[string]any{"op": "replace", "path": "/spec/lifecycle/shutdownTime", "value": deadline})
	}
	raw, _ := json.Marshal(ops)
	_, err := s.resource(Claims).Patch(ctx, c.GetName(), "application/json-patch+json", raw, metav1.PatchOptions{})
	return err
}
func retryPatch(err error) bool { return apierrors.IsConflict(err) || apierrors.IsInvalid(err) }

// observation is local to one fresh API observation; it is never cached across
// connection checks or requests. The Pod has passed the full backing checks.
type observation struct {
	View
	pod *unstructured.Unstructured
}

func (s *Service) Get(ctx context.Context, principal, id string) (View, error) {
	snapshot, err := s.snapshot(ctx, principal, id)
	return snapshot.View, err
}

func (s *Service) snapshot(ctx context.Context, principal, id string) (observation, error) {
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return observation{}, err
		}
		v, change, err := s.observe(ctx, c)
		if err != nil {
			return observation{}, err
		}
		if len(change) == 0 {
			return v, nil
		}
		if err = s.patch(ctx, c, change, ""); err != nil {
			if retryPatch(err) {
				continue
			}
			return observation{}, err
		}
		// The observation becomes publishable only after its UID/version CAS
		// succeeds. Re-listing cannot make that earlier snapshot atomic.
		return v, nil
	}
	return observation{}, fail("state_conflict")
}
func (s *Service) observe(ctx context.Context, c *unstructured.Unstructured) (observation, map[string]string, error) {
	expires, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownTime")
	deadline, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return observation{}, nil, fail("integrity_error")
	}
	b, err := readBinding(c)
	if err != nil {
		return observation{}, nil, err
	}
	v := observation{View: View{ID: sessionID(c), State: "creating", ExpiresAt: expires, IdentityRevision: revision(b), OperationActive: c.GetAnnotations()[operationKey] != ""}}
	if c.GetDeletionTimestamp() != nil {
		return observation{}, nil, fail("identity_changed")
	}
	closing := c.GetAnnotations()[closeKey] == "true" || !s.now().Before(deadline)
	if closing {
		v.State = "closing"
		v.Reason = "awaiting_upstream_cleanup"
		if !condition(c, "Ready", "False", ext.ClaimExpiredReason) {
			return v, nil, nil
		}
		remaining, e := s.remaining(ctx, c, b)
		if e != nil {
			return observation{}, nil, e
		}
		if !remaining {
			v.State = "closed"
			v.Reason = ""
			v.CleanupComplete = true
			if v.OperationActive {
				v.OperationActive = false
				return v, map[string]string{operationKey: ""}, nil
			}
		}
		return v, nil, nil
	}
	if reason := c.GetAnnotations()[invalidKey]; reason != "" {
		v.State = "invalidated"
		v.Reason = reason
		return v, nil, nil
	}
	current, ready, reason, e := s.backing(ctx, c)
	if e != nil {
		return observation{}, nil, e
	}
	if current != nil {
		v.pod = current.pod
	}
	if b != nil {
		if current == nil || *current.binding != *b {
			v.State = "invalidated"
			v.Reason = "backing_identity_changed"
			return v, map[string]string{invalidKey: v.Reason}, nil
		}
		if reason == "backing_terminating" || reason == "backing_failed" {
			v.State = "invalidated"
			v.Reason = "backing_environment_terminated"
			return v, map[string]string{invalidKey: v.Reason}, nil
		}
		if !ready {
			v.State = "unavailable"
			if reason == "backing_failed" {
				v.State = "failed"
			}
			v.Reason = reason
			return v, nil, nil
		}
		v.State = "ready"
		return v, nil, nil
	}
	if !ready {
		if reason == "backing_failed" {
			v.State = "failed"
		}
		v.Reason = reason
		return v, nil, nil
	}
	v.State = "ready"
	v.IdentityRevision = revision(current.binding)
	raw, _ := json.Marshal(current.binding)
	return v, map[string]string{bindingKey: string(raw)}, nil
}

type backingSnapshot struct {
	binding *Binding
	pod     *unstructured.Unstructured
}

func (s *Service) backing(ctx context.Context, c *unstructured.Unstructured) (*backingSnapshot, bool, string, error) {
	name, _, _ := unstructured.NestedString(c.Object, "status", "sandbox", "name")
	if name == "" {
		return nil, false, "awaiting_assignment", nil
	}
	box, err := s.resource(Sandboxes).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, "awaiting_sandbox", nil
	}
	if err != nil {
		return nil, false, "", err
	}
	if !controllerRef(box, string(c.GetUID())) {
		return nil, false, "", fail("integrity_error")
	}
	pods, err := s.list(ctx, Pods)
	if err != nil {
		return nil, false, "", err
	}
	var pod *unstructured.Unstructured
	for i := range pods {
		if controllerRef(&pods[i], string(box.GetUID())) {
			if pod != nil {
				return nil, false, "", fail("integrity_error")
			}
			pod = &pods[i]
		}
	}
	if pod == nil {
		return nil, false, "awaiting_pod", nil
	}
	services, err := s.list(ctx, Services)
	if err != nil {
		return nil, false, "", err
	}
	var service *unstructured.Unstructured
	for i := range services {
		if controllerRef(&services[i], string(box.GetUID())) {
			if service != nil {
				return nil, false, "", fail("integrity_error")
			}
			service = &services[i]
		}
	}
	if service == nil {
		return nil, false, "awaiting_service", nil
	}
	value := &backingSnapshot{binding: &Binding{Version: 1, Sandbox: ref(box), Pod: ref(pod), Service: ref(service)}, pod: pod}
	clusterIP, _, _ := unstructured.NestedString(service.Object, "spec", "clusterIP")
	selector, _, _ := unstructured.NestedStringMap(service.Object, "spec", "selector")
	selection := labels.SelectorFromSet(selector)
	selected := 0
	for i := range pods {
		if selection.Matches(labels.Set(pods[i].GetLabels())) {
			selected++
		}
	}
	if clusterIP != "None" || len(selector) == 0 || selected != 1 || !selection.Matches(labels.Set(pod.GetLabels())) {
		return nil, false, "", fail("integrity_error")
	}
	runtimeClass, _, _ := unstructured.NestedString(pod.Object, "spec", "runtimeClassName")
	token, found, _ := unstructured.NestedBool(pod.Object, "spec", "automountServiceAccountToken")
	hostNet, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostNetwork")
	hostPID, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostPID")
	hostIPC, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostIPC")
	if runtimeClass != s.cfg.RuntimeClass || !found || token || hostNet || hostPID || hostIPC {
		return nil, false, "", fail("integrity_error")
	}
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok || volume["hostPath"] != nil {
			return nil, false, "", fail("integrity_error")
		}
	}
	phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
	if phase == "Failed" || phase == "Succeeded" {
		return value, false, "backing_failed", nil
	}
	if box.GetDeletionTimestamp() != nil || pod.GetDeletionTimestamp() != nil || service.GetDeletionTimestamp() != nil {
		return value, false, "backing_terminating", nil
	}
	if !condition(c, "Ready", "True", "") || !condition(box, "Ready", "True", "") || !podReady(pod) {
		return value, false, "backing_not_ready", nil
	}
	return value, true, "", nil
}
func podReady(pod *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
	if phase != "Running" {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	for _, v := range conditions {
		m, ok := v.(map[string]any)
		if ok && m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// No-resource results need a current upstream expiry acknowledgement. Names
// conservatively cover services whose Sandbox owner was already collected.
func (s *Service) remaining(ctx context.Context, c *unstructured.Unstructured, b *Binding) (bool, error) {
	ids := map[string]bool{}
	names := map[string]bool{c.GetName(): true}
	if b != nil {
		ids[b.Sandbox.UID] = true
		names[b.Sandbox.Name] = true
	}
	if name, _, _ := unstructured.NestedString(c.Object, "status", "sandbox", "name"); name != "" {
		names[name] = true
	}
	if name := c.GetAnnotations()[ext.AssignedSandboxNameAnnotation]; name != "" {
		names[name] = true
	}
	boxes, err := s.list(ctx, Sandboxes)
	if err != nil {
		return false, err
	}
	remaining := false
	for i := range boxes {
		if controllerRef(&boxes[i], string(c.GetUID())) {
			remaining = true
			ids[string(boxes[i].GetUID())] = true
			names[boxes[i].GetName()] = true
		}
	}
	for _, kind := range []schema.GroupVersionResource{Pods, Services} {
		objects, e := s.list(ctx, kind)
		if e != nil {
			return false, e
		}
		for _, obj := range objects {
			if obj.GetLabels()[ext.SandboxIDLabel] == string(c.GetUID()) {
				remaining = true
			}
			for _, owner := range obj.GetOwnerReferences() {
				if owner.Kind == "Sandbox" && (ids[string(owner.UID)] || names[owner.Name]) {
					remaining = true
				}
			}
		}
	}
	return remaining, nil
}

func (s *Service) Close(ctx context.Context, principal, id string) (View, error) {
	patched := false
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return View{}, err
		}
		if c.GetAnnotations()[closeKey] == "true" {
			patched = true
			break
		}
		expires, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownTime")
		old, e := time.Parse(time.RFC3339, expires)
		if e != nil {
			return View{}, fail("integrity_error")
		}
		deadline := s.now().UTC().Truncate(time.Second)
		if old.Before(deadline) {
			deadline = old
		}
		err = s.patch(ctx, c, map[string]string{closeKey: "true"}, deadline.Format(time.RFC3339))
		if err == nil {
			patched = true
			break
		}
		if !retryPatch(err) {
			return View{}, err
		}
	}
	if !patched {
		return View{}, fail("state_conflict")
	}
	end := s.now().Add(s.cfg.CloseWait)
	for {
		v, err := s.Get(ctx, principal, id)
		if err != nil {
			return View{}, err
		}
		if v.State == "closed" || s.cfg.CloseWait == 0 || !s.now().Before(end) {
			return v, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return View{}, ctx.Err()
		case <-timer.C:
		}
	}
}
