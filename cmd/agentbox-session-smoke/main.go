// agentbox-session-smoke is a trusted in-cluster upstream SDK acceptance client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/X1Kun/AgentBox/internal/sessionprobe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

var claims = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
var sandboxes = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
var pools = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}

const ownerLabel = "agentbox.dev/session"
const runLabel = "agentbox.dev/session-run"

type identity struct {
	Claim       string `json:"claim"`
	ClaimUID    string `json:"claim_uid"`
	Sandbox     string `json:"sandbox"`
	SandboxUID  string `json:"sandbox_uid"`
	Pod         string `json:"pod"`
	PodUID      string `json:"pod_uid"`
	PodIP       string `json:"pod_ip"`
	Service     string `json:"service"`
	ServiceUID  string `json:"service_uid"`
	ServiceFQDN string `json:"service_fqdn"`
}
type report struct {
	Stage     string               `json:"stage"`
	Run       string               `json:"run"`
	Owner     string               `json:"owner"`
	Namespace string               `json:"namespace"`
	SDK       string               `json:"sdk_version"`
	Go        string               `json:"go_version"`
	Sessions  []identity           `json:"sessions"`
	Expiring  *identity            `json:"expiring,omitempty"`
	TimedOut  *identity            `json:"timed_out,omitempty"`
	Checks    []sessionprobe.Check `json:"checks"`
	Timings   map[string]int64     `json:"timings_ms"`
	Error     string               `json:"error,omitempty"`
	Cleanup   bool                 `json:"cleanup_complete"`
	Profile   string               `json:"profile"`
	Warm      *warmReport          `json:"warm,omitempty"`
	Fault     *faultReport         `json:"fault,omitempty"`
}
type probe struct {
	namespace, run, owner, pool string
	core                        coreclient.CoreV1Interface
	dynamic                     dynamic.Interface
	result                      report
}

func (p *probe) emit(stage string) {
	p.result.Stage = stage
	_ = json.NewEncoder(os.Stdout).Encode(p.result)
}
func controlled(obj metav1.Object, owner, run string) bool {
	return obj.GetLabels()[ownerLabel] == owner && obj.GetLabels()[runLabel] == run
}
func ownedBy(refs []metav1.OwnerReference, uid string) bool {
	for _, r := range refs {
		if string(r.UID) == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func (p *probe) identify(ctx context.Context, s *sdk.Sandbox) (identity, error) {
	id := identity{Claim: s.ClaimName(), Sandbox: s.SandboxName(), Pod: s.PodName(), ServiceFQDN: s.ServiceFQDN()}
	claim, err := p.dynamic.Resource(claims).Namespace(p.namespace).Get(ctx, id.Claim, metav1.GetOptions{})
	if err != nil {
		return id, err
	}
	if !controlled(claim, p.owner, p.run) {
		return id, errors.New("claim ownership changed")
	}
	id.ClaimUID = string(claim.GetUID())
	linked, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if linked != id.Sandbox {
		return id, errors.New("claim sandbox link changed")
	}
	sandbox, err := p.dynamic.Resource(sandboxes).Namespace(p.namespace).Get(ctx, id.Sandbox, metav1.GetOptions{})
	if err != nil {
		return id, err
	}
	id.SandboxUID = string(sandbox.GetUID())
	if !ownedBy(sandbox.GetOwnerReferences(), id.ClaimUID) {
		return id, errors.New("Sandbox is not owned by this claim")
	}
	pod, err := p.core.Pods(p.namespace).Get(ctx, id.Pod, metav1.GetOptions{})
	if err != nil {
		return id, err
	}
	id.PodUID = string(pod.UID)
	id.PodIP = pod.Status.PodIP
	if !controlled(pod, p.owner, p.run) || !ownedBy(pod.OwnerReferences, id.SandboxUID) || pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "agentbox-gvisor" || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		return id, errors.New("unexpected Pod ownership, runtime or credentials")
	}
	parts := strings.Split(id.ServiceFQDN, ".")
	if len(parts) < 4 || parts[1] != p.namespace {
		return id, errors.New("unexpected session Service address")
	}
	id.Service = parts[0]
	service, err := p.core.Services(p.namespace).Get(ctx, id.Service, metav1.GetOptions{})
	if err != nil {
		return id, err
	}
	id.ServiceUID = string(service.UID)
	if !ownedBy(service.OwnerReferences, id.SandboxUID) || service.Spec.ClusterIP != "None" || len(service.Spec.Selector) == 0 {
		return id, errors.New("session Service ownership changed")
	}
	selected, err := p.core.Pods(p.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(service.Spec.Selector).String()})
	if err != nil {
		return id, err
	}
	if len(selected.Items) != 1 || selected.Items[0].UID != pod.UID {
		return id, errors.New("Service no longer uniquely selects its session")
	}
	return id, nil
}
func (p *probe) stable(ctx context.Context, s *sdk.Sandbox, expected identity) error {
	actual, err := p.identify(ctx, s)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("session identity changed across calls")
	}
	return nil
}
func waitMarker(ctx context.Context, name string) error {
	return waitMarkerBudget(ctx, name, 120*time.Second)
}
func waitMarkerBudget(ctx context.Context, name string, budget time.Duration) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	until := time.NewTimer(budget)
	defer until.Stop()
	for {
		if _, err := os.Stat(filepath.Join("/work", name)); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-until.C:
			return errors.New("host acceptance handshake timed out")
		case <-ticker.C:
		}
	}
}
func (p *probe) gone(ctx context.Context, id identity) error {
	until := time.NewTimer(90 * time.Second)
	defer until.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		remaining := false
		for _, v := range []struct {
			gvr       schema.GroupVersionResource
			name, uid string
		}{{claims, id.Claim, id.ClaimUID}, {sandboxes, id.Sandbox, id.SandboxUID}} {
			obj, err := p.dynamic.Resource(v.gvr).Namespace(p.namespace).Get(ctx, v.name, metav1.GetOptions{})
			if err == nil {
				if obj.GetUID() != types.UID(v.uid) {
					return errors.New("resource replaced while awaiting deletion")
				}
				remaining = true
			} else if !apierrors.IsNotFound(err) {
				return err
			}
		}
		pod, err := p.core.Pods(p.namespace).Get(ctx, id.Pod, metav1.GetOptions{})
		if err == nil {
			if string(pod.UID) != id.PodUID {
				return errors.New("Pod replaced while closing")
			}
			remaining = true
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		svc, err := p.core.Services(p.namespace).Get(ctx, id.Service, metav1.GetOptions{})
		if err == nil {
			if string(svc.UID) != id.ServiceUID {
				return errors.New("Service replaced while closing")
			}
			remaining = true
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		if !remaining {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-until.C:
			return errors.New("session resources did not converge after closure/expiry")
		case <-ticker.C:
		}
	}
}
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, d := range info.Deps {
			if d.Path == "sigs.k8s.io/agent-sandbox" {
				return d.Version
			}
		}
	}
	return "unknown"
}
func run(ctx context.Context) (result report, retErr error) {
	namespace, runID, owner := os.Getenv("SESSION_NAMESPACE"), os.Getenv("SESSION_RUN"), os.Getenv("SESSION_OWNER")
	result = report{Run: runID, Owner: owner, Namespace: namespace, SDK: moduleVersion(), Go: runtime.Version(), Timings: map[string]int64{}}
	if !regexp.MustCompile(`^[a-f0-9]{10}$`).MatchString(runID) || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(owner) || namespace != "agentbox-session-"+runID || result.SDK != "v1.0.8" {
		return result, errors.New("invalid acceptance namespace, ownership or SDK version")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return result, err
	}
	core, err := coreclient.NewForConfig(config)
	if err != nil {
		return result, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return result, err
	}
	p := probe{namespace: namespace, run: runID, owner: owner, pool: "python-cold", core: core, dynamic: dyn, result: result}
	authorization, err := authclient.NewForConfig(config)
	if err != nil {
		return result, err
	}
	for _, check := range []struct {
		group, resource, verb string
		allowed               bool
	}{
		{"", "secrets", "list", false}, {"", "pods", "create", false},
		{"extensions.agents.x-k8s.io", "sandboxtemplates", "patch", false},
		{"extensions.agents.x-k8s.io", "sandboxclaims", "create", true},
	} {
		review, err := authorization.SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{Namespace: namespace, Group: check.group, Resource: check.resource, Verb: check.verb}}}, metav1.CreateOptions{})
		if err != nil {
			return result, err
		}
		if review.Status.Allowed != check.allowed {
			return result, fmt.Errorf("unexpected client RBAC for %s %s", check.verb, check.resource)
		}
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "client_rbac_scope", Passed: true})

	defer func() { result = p.result }()
	profile := os.Getenv("SESSION_PROFILE")
	if profile == "" {
		profile = "cold"
	}
	p.result.Profile = profile
	if profile == "warm" {
		return p.runWarm(ctx, config)
	}
	if profile == "fault" {
		return p.runFault(ctx, config)
	}
	if profile != "cold" {
		return result, errors.New("unknown session acceptance profile")
	}
	pool, err := dyn.Resource(pools).Namespace(namespace).Get(ctx, p.pool, metav1.GetOptions{})
	if err != nil {
		return result, err
	}
	n, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	if n != 0 {
		return result, errors.New("acceptance requires zero prewarmed replicas")
	}
	before, err := dyn.Resource(sandboxes).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, err
	}
	if len(before.Items) != 0 {
		return result, errors.New("cold namespace already contains sandboxes")
	}
	client, err := sdk.NewClient(ctx, sdk.Options{Namespace: namespace, Runtime: sdk.RuntimeSandboxd, Connectivity: sdk.ConnectivityInClusterService,
		RestConfig: config, Labels: map[string]string{ownerLabel: owner, runLabel: runID}, Quiet: true,
		ShutdownAfter: 10 * time.Minute, SandboxReadyTimeout: 180 * time.Second, CleanupTimeout: 60 * time.Second,
		RequestTimeout: 15 * time.Second, PerAttemptTimeout: 10 * time.Second, MaxUploadSize: 1 << 20, MaxDownloadSize: 1 << 20})
	if err != nil {
		return result, err
	}
	var handles []*sdk.Sandbox
	defer func() {
		for _, s := range handles {
			cleanup, stop := context.WithTimeout(context.Background(), 90*time.Second)
			e := s.Close(cleanup)
			stop()
			if e != nil {
				retErr = errors.Join(retErr, e)
			}
		}
	}()
	start := time.Now()
	a, err := client.CreateSandbox(ctx, p.pool, namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, a)
	p.result.Timings["create_a"] = time.Since(start).Milliseconds()
	ia, err := p.identify(ctx, a)
	if err != nil {
		return result, err
	}
	start = time.Now()
	b, err := client.CreateSandbox(ctx, p.pool, namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, b)
	p.result.Timings["create_b"] = time.Since(start).Milliseconds()
	ib, err := p.identify(ctx, b)
	if err != nil {
		return result, err
	}
	if ia.PodUID == ib.PodUID {
		return result, errors.New("two claims share a Pod")
	}
	p.result.Sessions = []identity{ia, ib}
	p.emit("ready")
	start = time.Now()
	if err := waitMarker(ctx, "proceed"); err != nil {
		return result, err
	}
	p.result.Timings["host_validation_wait"] = time.Since(start).Milliseconds()
	parsedAPI, err := url.Parse(config.Host)
	if err != nil {
		return result, err
	}
	start = time.Now()
	checks, err := sessionprobe.Exercise(ctx, a, b, ib.PodIP, parsedAPI.Hostname())
	if err != nil {
		return result, err
	}
	p.result.Checks = append(p.result.Checks, checks...)
	p.result.Timings["operations"] = time.Since(start).Milliseconds()
	if err := p.stable(ctx, a, ia); err != nil {
		return result, err
	}
	if err := p.stable(ctx, b, ib); err != nil {
		return result, err
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "stable_claim_sandbox_pod_service_identity", Passed: true})
	if err := b.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, ib); err != nil {
		return result, err
	}
	if _, err := b.Run(ctx, "echo must-not-run"); !errors.Is(err, sdk.ErrNotReady) {
		return result, errors.New("closed handle accepted a command")
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "explicit_close_and_reject_new_calls", Passed: true})
	c, err := client.CreateSandbox(ctx, p.pool, namespace, sdk.WithShutdownAfter(30*time.Second))
	if err != nil {
		return result, err
	}
	handles = append(handles, c)
	ic, err := p.identify(ctx, c)
	if err != nil {
		return result, err
	}
	p.result.Expiring = &ic
	p.emit("expiry-ready")
	if err := p.gone(ctx, ic); err != nil {
		return result, err
	}
	if err := c.Close(ctx); err != nil {
		return result, err
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "controller_claim_expiry_and_gc", Passed: true})
	d, err := client.CreateSandbox(ctx, p.pool, namespace, sdk.WithShutdownAfter(2*time.Minute))
	if err != nil {
		return result, err
	}
	handles = append(handles, d)
	id, err := p.identify(ctx, d)
	if err != nil {
		return result, err
	}
	p.result.TimedOut = &id
	if err := d.Write(ctx, "long.py", []byte(longScript)); err != nil {
		return result, err
	}
	p.emit("timeout-ready")
	if err := waitMarker(ctx, "timeout"); err != nil {
		return result, err
	}
	timed, err := d.Commands().Start(ctx, "python3 -I -B -u long.py", sdk.WithTimeout(2*time.Second))
	if err != nil {
		return result, err
	}
	var timeoutErr error
	for {
		_, e := timed.Recv()
		if e != nil {
			timeoutErr = e
			break
		}
	}
	_ = timed.Close()
	if !errors.Is(timeoutErr, context.DeadlineExceeded) && status.Code(timeoutErr) != codes.DeadlineExceeded {
		return result, fmt.Errorf("expected stream deadline, got %v", timeoutErr)
	}
	// An expired RPC does not certify remote cleanup. Close the entire session.
	if err := d.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, id); err != nil {
		return result, err
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "deadline_no_replay_and_session_cleanup", Passed: true})
	if err := a.Write(ctx, "long.py", []byte(longScript)); err != nil {
		return result, err
	}
	active, err := a.Commands().Start(ctx, "python3 -I -B -u long.py", sdk.WithTimeout(120*time.Second))
	if err != nil {
		return result, err
	}
	defer active.Close()
	output := ""
	for !strings.Contains(output, "started") {
		event, e := active.Recv()
		if e != nil {
			return result, e
		}
		if event.Type == sdk.ProcessEventStdout {
			output += string(event.Data)
		}
		if len(output) > 4096 {
			return result, errors.New("unexpected long-command output")
		}
	}
	p.emit("closing-active")
	if err := waitMarker(ctx, "close"); err != nil {
		return result, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			_, e := active.Recv()
			if e != nil {
				return
			}
		}
	}()
	start = time.Now()
	if err := a.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, ia); err != nil {
		return result, err
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		return result, errors.New("stream did not close with session")
	}
	p.result.Timings["close_active"] = time.Since(start).Milliseconds()
	if _, err := a.Run(ctx, "echo must-not-run"); !errors.Is(err, sdk.ErrNotReady) {
		return result, errors.New("active session accepted calls after closure")
	}
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: "close_running_parent_and_child", Passed: true})
	remaining, err := dyn.Resource(sandboxes).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, err
	}
	if len(remaining.Items) != 0 {
		return result, errors.New("zero-prewarm namespace has residual sandboxes")
	}
	p.result.Cleanup = true
	return p.result, nil
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	result, err := run(ctx)
	if err != nil {
		result.Stage = "failed"
		result.Error = sessionprobe.ShortError(err)
		_ = json.NewEncoder(os.Stdout).Encode(result)
		os.Exit(1)
	}
	result.Stage = "complete"
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

const longScript = `import signal, subprocess, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
subprocess.Popen([sys.executable, '-I', '-B', '-c', 'import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(120)'])
print('started', flush=True)
time.sleep(120)
`
