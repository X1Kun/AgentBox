package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/X1Kun/AgentBox/internal/requestid"
	sessionprotocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"log/slog"
	"regexp"
	ext "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"strings"
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

const closedAtKey = "agentbox.dev/api-closed-at"

var dns = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var principalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

var requestPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

var idPattern = regexp.MustCompile(`^(ses-[a-f0-9]{40})\.([a-z0-9-]{1,64})$`)

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

func hash(value string) string { v := sha256.Sum256([]byte(value)); return hex.EncodeToString(v[:]) }

func (s *Service) resource(g schema.GroupVersionResource) dynamic.ResourceInterface {
	return s.client.Resource(g).Namespace(s.cfg.Namespace)
}

func (s *Service) owned(c *unstructured.Unstructured, principal string) error {
	labels := c.GetLabels()
	a := c.GetAnnotations()
	if labels[ownerKey] != hash(principal)[:32] || labels[instanceKey] != s.cfg.Instance || labels[managedKey] != "agentbox-api" {
		return fail(sessionprotocol.CodeNotFound)
	}
	pool, _, _ := unstructured.NestedString(c.Object, "spec", "warmPoolRef", "name")
	policy, _, _ := unstructured.NestedString(c.Object, "spec", "lifecycle", "shutdownPolicy")
	if pool != s.cfg.Pool || policy != "Retain" || len(a[digestKey]) != 64 || c.GetUID() == "" || c.GetResourceVersion() == "" {
		return fail(sessionprotocol.CodeIntegrityError)
	}
	return nil
}

func sessionID(c *unstructured.Unstructured) string { return c.GetName() + "." + string(c.GetUID()) }

func (s *Service) load(ctx context.Context, principal, id string) (*unstructured.Unstructured, error) {
	parts := idPattern.FindStringSubmatch(id)
	if !ValidPrincipal(principal) || parts == nil {
		return nil, fail(sessionprotocol.CodeNotFound)
	}
	c, err := s.getResource(ctx, Claims, parts[1])
	if apierrors.IsNotFound(err) {
		return nil, fail(sessionprotocol.CodeNotFound)
	}
	if err != nil {
		return nil, err
	}
	if err := s.owned(c, principal); err != nil {
		return nil, err
	}
	if string(c.GetUID()) != parts[2] {
		return nil, fail(sessionprotocol.CodeIdentityChanged)
	}
	return c, nil
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
		return nil, fail(sessionprotocol.CodeIntegrityError)
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

func ref(obj metav1.Object) Ref { return Ref{Name: obj.GetName(), UID: string(obj.GetUID())} }

func (s *Service) list(ctx context.Context, g schema.GroupVersionResource) (result []unstructured.Unstructured, err error) {
	stage := "pods_list"
	if g == Services {
		stage = "services_list"
	}
	done := s.beginBackend(ctx, stage, "")
	defer func() { done(err) }()
	v, e := s.resource(g).List(ctx, metav1.ListOptions{})
	if e != nil {
		return nil, e
	}
	return v.Items, nil
}

// patch tests both UID and resourceVersion; conflicts cause a bounded re-read, never adoption.
func (s *Service) patch(ctx context.Context, c *unstructured.Unstructured, fields map[string]string, deadline string) (err error) {
	done := s.beginBackend(ctx, "claim_patch", c.GetName())
	defer func() { done(err) }()
	ops := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(c.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": c.GetResourceVersion()}}
	for k, v := range fields {
		ops = append(ops, map[string]any{"op": "add", "path": "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"), "value": v})
	}
	if deadline != "" {
		ops = append(ops, map[string]any{"op": "replace", "path": "/spec/lifecycle/shutdownTime", "value": deadline})
	}
	raw, _ := json.Marshal(ops)
	_, err = s.resource(Claims).Patch(ctx, c.GetName(), "application/json-patch+json", raw, metav1.PatchOptions{})
	return err
}

func retryPatch(err error) bool { return apierrors.IsConflict(err) || apierrors.IsInvalid(err) }

type backingSnapshot struct {
	binding *Binding
	pod     *unstructured.Unstructured
}

func (s *Service) backing(ctx context.Context, c *unstructured.Unstructured) (*backingSnapshot, bool, string, error) {
	name, _, _ := unstructured.NestedString(c.Object, "status", "sandbox", "name")
	if name == "" {
		return nil, false, "awaiting_assignment", nil
	}
	box, err := s.getResource(ctx, Sandboxes, name)
	if apierrors.IsNotFound(err) {
		return nil, false, "awaiting_sandbox", nil
	}
	if err != nil {
		return nil, false, "", err
	}
	if !controllerRef(box, string(c.GetUID())) {
		return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
	}
	pods, err := s.list(ctx, Pods)
	if err != nil {
		return nil, false, "", err
	}
	var pod *unstructured.Unstructured
	for i := range pods {
		if controllerRef(&pods[i], string(box.GetUID())) {
			if pod != nil {
				return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
			}
			pod = &pods[i]
		}
	}
	if pod == nil {
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.InfoContext(ctx, "session backing wait", "request_id", requestid.From(ctx), "session_id", sessionID(c), "claim_uid", string(c.GetUID()), "sandbox_name", box.GetName(), "sandbox_uid", string(box.GetUID()), "reason", "awaiting_pod")
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
				return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
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
		return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
	}
	runtimeClass, _, _ := unstructured.NestedString(pod.Object, "spec", "runtimeClassName")
	token, found, _ := unstructured.NestedBool(pod.Object, "spec", "automountServiceAccountToken")
	hostNet, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostNetwork")
	hostPID, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostPID")
	hostIPC, _, _ := unstructured.NestedBool(pod.Object, "spec", "hostIPC")
	if runtimeClass != s.cfg.RuntimeClass || !found || token || hostNet || hostPID || hostIPC {
		return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
	}
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok || volume["hostPath"] != nil {
			return nil, false, "", fail(sessionprotocol.CodeIntegrityError)
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
