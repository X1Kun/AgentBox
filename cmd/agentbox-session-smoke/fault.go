package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/X1Kun/AgentBox/internal/sessionprobe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

type faultReport struct {
	Original    identity  `json:"original"`
	Replacement *identity `json:"replacement,omitempty"`
	Expiring    *identity `json:"expiring,omitempty"`
	GuardError  string    `json:"guard_error,omitempty"`
}

// A stable Claim or Service name cannot fence a replacement Pod.
func replacementBoundary(before, after identity) error {
	if before.ClaimUID == "" || before.SandboxUID == "" || before.PodUID == "" || before.ServiceUID == "" || after.PodUID == "" {
		return errors.New("incomplete backing identity")
	}
	if before.Claim != after.Claim || before.ClaimUID != after.ClaimUID || before.Sandbox != after.Sandbox || before.SandboxUID != after.SandboxUID ||
		before.Service != after.Service || before.ServiceUID != after.ServiceUID || before.ServiceFQDN != after.ServiceFQDN {
		return errors.New("claim, Sandbox or Service changed during Pod-loss observation")
	}
	if before.PodUID == after.PodUID {
		return errors.New("Pod loss did not produce a new UID")
	}
	return nil
}
func (p *probe) faultCheck(name string) {
	p.result.Checks = append(p.result.Checks, sessionprobe.Check{Name: name, Passed: true})
}

func (p *probe) runFault(ctx context.Context, config *rest.Config) (result report, retErr error) {
	p.result.Fault = &faultReport{}
	defer func() { result = p.result }()
	opts := sdk.Options{Namespace: p.namespace, Runtime: sdk.RuntimeSandboxd, Connectivity: sdk.ConnectivityInClusterService,
		RestConfig: config, Labels: map[string]string{ownerLabel: p.owner, runLabel: p.run}, Quiet: true,
		ShutdownAfter: 10 * time.Minute, SandboxReadyTimeout: 180 * time.Second, CleanupTimeout: 60 * time.Second,
		RequestTimeout: 15 * time.Second, PerAttemptTimeout: 10 * time.Second, MaxUploadSize: 1 << 20, MaxDownloadSize: 1 << 20}
	client, err := sdk.NewClient(ctx, opts)
	if err != nil {
		return result, err
	}
	var handles []*sdk.Sandbox
	defer func() {
		for _, s := range handles {
			cleanup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			e := s.Close(cleanup)
			cancel()
			retErr = errors.Join(retErr, e)
		}
	}()
	a, err := client.CreateSandbox(ctx, p.pool, p.namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, a)
	original, err := p.identify(ctx, a)
	if err != nil {
		return result, err
	}
	p.result.Fault.Original = original
	p.result.Sessions = append(p.result.Sessions, original)
	if err := a.Write(ctx, "kept.txt", []byte("same-session")); err != nil {
		return result, err
	}
	if err := a.Write(ctx, "wait.py", []byte(controllerWait)); err != nil {
		return result, err
	}
	active, err := a.Commands().Start(ctx, "python3 -I -B -u wait.py", sdk.WithTimeout(180*time.Second))
	if err != nil {
		return result, err
	}
	defer active.Close()
	// Keep an actual command in flight while the host replaces only the upstream controller Pod.
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
			return result, errors.New("unexpected restart probe output")
		}
	}
	p.emit("fault-controller")
	if err := waitMarker(ctx, "fault-controller-done"); err != nil {
		return result, err
	}
	if err := a.Write(ctx, "continue", []byte("ok")); err != nil {
		return result, err
	}
	exitSeen := false
	for {
		event, e := active.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return result, e
		}
		if event.Type == sdk.ProcessEventStdout {
			output += string(event.Data)
		}
		if len(output) > 4096 {
			return result, errors.New("oversized restart probe output")
		}
		if event.Type == sdk.ProcessEventExit {
			if event.ExitCode != 0 {
				return result, errors.New("in-flight command failed across controller restart")
			}
			exitSeen = true
		}
	}
	if !exitSeen || output != "started\ndone\n" {
		return result, errors.New("in-flight command did not complete correctly")
	}
	if err := active.Close(); err != nil {
		return result, err
	}
	if err := p.stable(ctx, a, original); err != nil {
		return result, err
	}
	data, err := a.Read(ctx, "kept.txt")
	if err != nil {
		return result, err
	}
	if string(data) != "same-session" {
		return result, errors.New("controller restart changed workspace")
	}
	p.faultCheck("controller_restart_keeps_session_identity_and_workspace")
	p.faultCheck("inflight_command_survives_controller_restart_once")
	p.emit("fault-pod-loss")
	if err := waitMarkerBudget(ctx, "fault-pod-replaced", 180*time.Second); err != nil {
		return result, err
	}
	// A separate SDK client is deliberate: GetSandbox may otherwise return its cached old handle.
	observer, err := sdk.NewClient(ctx, opts)
	if err != nil {
		return result, err
	}
	replacement, err := observer.GetSandbox(ctx, original.Claim, p.namespace)
	if err != nil {
		return result, err
	}
	handles = append(handles, replacement)
	current, err := p.identify(ctx, replacement)
	if err != nil {
		return result, err
	}
	if err := replacementBoundary(original, current); err != nil {
		return result, err
	}
	p.result.Fault.Replacement = &current
	p.result.Sessions = append(p.result.Sessions, current)
	if err := p.stable(ctx, a, original); err == nil {
		return result, errors.New("old backing identity was accepted after Pod loss")
	} else {
		p.result.Fault.GuardError = sessionprobe.ShortError(err)
	}
	p.faultCheck("pod_replacement_changes_uid_and_invalidates_old_identity")
	// Inspect the new environment explicitly; never replay any old command against it.
	exists, err := replacement.Exists(ctx, "kept.txt")
	if err != nil {
		return result, err
	}
	if exists {
		return result, errors.New("replacement Pod unexpectedly retained emptyDir workspace")
	}
	p.faultCheck("replacement_environment_has_no_implicit_workspace_restore")
	p.emit("fault-replacement")
	if err := waitMarker(ctx, "fault-pod-inspected"); err != nil {
		return result, err
	}
	if err := a.Close(ctx); err != nil {
		return result, err
	}
	if err := replacement.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, current); err != nil {
		return result, err
	}
	if _, err := a.Run(ctx, "echo must-not-run"); !errors.Is(err, sdk.ErrNotReady) {
		return result, errors.New("closed old handle accepted execution")
	}
	p.faultCheck("changed_session_closed_without_command_replay")
	b, err := client.CreateSandbox(ctx, p.pool, p.namespace, sdk.WithShutdownAfter(90*time.Second))
	if err != nil {
		return result, err
	}
	handles = append(handles, b)
	expiring, err := p.identify(ctx, b)
	if err != nil {
		return result, err
	}
	p.result.Fault.Expiring = &expiring
	p.result.Sessions = append(p.result.Sessions, expiring)
	exists, err = b.Exists(ctx, "kept.txt")
	if err != nil {
		return result, err
	}
	if exists {
		return result, errors.New("new Claim inherited previous workspace")
	}
	execution, err := b.Run(ctx, "python3 -I -B -c 'print(42)'", sdk.WithTimeout(15*time.Second))
	if err != nil {
		return result, err
	}
	if execution == nil || execution.ExitCode != 0 || execution.Stdout != "42\n" {
		return result, errors.New("new Claim failed execution")
	}
	p.faultCheck("explicit_new_claim_gets_clean_executable_environment")
	p.emit("fault-expiry-close")
	if err := waitMarker(ctx, "fault-expiry-armed"); err != nil {
		return result, err
	}
	claim, err := p.dynamic.Resource(claims).Namespace(p.namespace).Get(ctx, expiring.Claim, metav1.GetOptions{})
	if err == nil {
		if string(claim.GetUID()) != expiring.ClaimUID {
			return result, errors.New("expiring claim identity changed")
		}
		value, _, _ := unstructured.NestedString(claim.Object, "spec", "lifecycle", "shutdownTime")
		deadline, e := time.Parse(time.RFC3339, value)
		if e != nil {
			return result, e
		}
		if delay := time.Until(deadline); delay > 15*time.Second {
			return result, errors.New("host did not arm the expiry boundary")
		} else if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-timer.C:
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return result, err
	}
	// Explicit close and the upstream expiry delete can win in either order.
	if err := b.Close(ctx); err != nil {
		return result, err
	}
	if err := p.gone(ctx, expiring); err != nil {
		return result, err
	}
	if err := b.Close(ctx); err != nil {
		return result, err
	}
	if _, err := b.Run(ctx, "echo must-not-run"); !errors.Is(err, sdk.ErrNotReady) {
		return result, errors.New("expired/closed handle accepted execution")
	}
	p.faultCheck("expiry_and_close_converge_with_idempotent_close")
	objects, err := p.dynamic.Resource(sandboxes).Namespace(p.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, err
	}
	if len(objects.Items) != 0 {
		return result, errors.New("fault fixture left unexpected sandboxes")
	}
	p.result.Cleanup = true
	p.emit("fault-finished")
	if err := waitMarker(ctx, "fault-finish"); err != nil {
		return result, err
	}
	return p.result, nil
}

const controllerWait = `import time
from pathlib import Path
print('started',flush=True)
for _ in range(1700):
 if Path('continue').exists():
  print('done',flush=True)
  break
 time.sleep(0.1)
else:
 raise SystemExit(2)
`
