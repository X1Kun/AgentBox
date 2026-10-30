package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kt "k8s.io/client-go/testing"
)

type runtimeStub struct {
	calls   atomic.Int32
	start   chan struct{}
	unblock chan struct{}
	err     error
}

func captureOperationLogs(h *harness) *bytes.Buffer {
	var output bytes.Buffer
	h.service.logger = slog.New(slog.NewJSONHandler(&output, nil))
	return &output
}

func requireOperationEvent(t *testing.T, output *bytes.Buffer, phase, outcome string) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["phase"] == phase && event["outcome"] == outcome {
			if event["session_id"] == "" || event["operation_id"] == "" {
				t.Fatal("missing operation correlation")
			}
			return
		}
	}
	t.Fatalf("missing %s/%s event: %s", phase, outcome, output)
}

func TestUnknownExecutionCloseFailureIsDiagnosedWithoutReplay(t *testing.T) {
	h, v, r := operationSetup(t)
	logs := captureOperationLogs(h)
	r.err = errors.New("private-code-and-bearer-secret")
	h.client.PrependReactor("patch", "sandboxclaims", func(a kt.Action) (bool, runtime.Object, error) {
		if bytes.Contains(a.(kt.PatchAction).GetPatch(), []byte("api-close-requested")) {
			return true, nil, errors.New("private-cluster-address-and-secret")
		}
		return false, nil, nil
	})
	_, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "private-user-code"})
	if Code(err) != "operation_outcome_unknown" || r.calls.Load() != 1 {
		t.Fatalf("execution outcome: %v, calls=%d", err, r.calls.Load())
	}
	requireOperationEvent(t, logs, "execute", "unknown")
	requireOperationEvent(t, logs, "close", "request_failed")
	if strings.Contains(logs.String(), "private-") {
		t.Fatalf("native details leaked: %s", logs)
	}
	c := h.claim(t, v.ID)
	if c.GetAnnotations()[operationKey] == "" || c.GetAnnotations()[closeKey] != "" {
		t.Fatal("failed close released lock or falsely recorded success")
	}
	fresh := h.restart(t)
	fresh.SetRuntime(r)
	_, err = fresh.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "do_not_replay()"})
	if Code(err) != "session_busy" || r.calls.Load() != 1 {
		t.Fatal("restart bypassed persistent lock")
	}
}

func TestReleaseFailureReadbackAndConservativeClosure(t *testing.T) {
	for _, mode := range []string{"lost_reply", "not_applied", "unreadable", "different_holder", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			h, v, r := operationSetup(t)
			logs := captureOperationLogs(h)
			releasing := false
			h.client.PrependReactor("get", "sandboxclaims", func(kt.Action) (bool, runtime.Object, error) {
				if releasing && mode == "unreadable" {
					return true, nil, io.ErrUnexpectedEOF
				}
				return false, nil, nil
			})
			h.client.PrependReactor("patch", "sandboxclaims", func(a kt.Action) (bool, runtime.Object, error) {
				var ops []struct {
					Path  string
					Value any
				}
				if err := json.Unmarshal(a.(kt.PatchAction).GetPatch(), &ops); err != nil {
					t.Fatal(err)
				}
				for _, op := range ops {
					if !strings.HasSuffix(op.Path, "api-operation") || op.Value != "" {
						continue
					}
					releasing = true
					if mode == "lost_reply" {
						handled, _, err := kt.ObjectReaction(h.client.Tracker())(a)
						if !handled || err != nil {
							t.Fatalf("apply release: %v", err)
						}
					} else if mode == "different_holder" || mode == "replacement" {
						obj, err := h.client.Tracker().Get(Claims, "work", a.(kt.PatchAction).GetName())
						if err != nil {
							t.Fatal(err)
						}
						c := obj.(*unstructured.Unstructured).DeepCopy()
						annotations := c.GetAnnotations()
						annotations[operationKey] = "different-holder"
						c.SetAnnotations(annotations)
						if mode == "replacement" {
							c.SetUID("replacement-claim")
						}
						if err := h.client.Tracker().Update(Claims, c, "work"); err != nil {
							t.Fatal(err)
						}
					}
					return true, nil, io.ErrUnexpectedEOF
				}
				return false, nil, nil
			})
			_, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "print(1)"})
			if r.calls.Load() != 1 {
				t.Fatal("execution replayed")
			}
			if mode == "lost_reply" {
				if err != nil {
					t.Fatalf("confirmed release lost result: %v", err)
				}
				requireOperationEvent(t, logs, "release", "complete")
				return
			}
			if Code(err) != "operation_outcome_unknown" {
				t.Fatalf("unconfirmed release: %v", err)
			}
			requireOperationEvent(t, logs, "release", "unknown")
			if mode == "unreadable" || mode == "replacement" {
				requireOperationEvent(t, logs, "close", "request_failed")
			} else {
				requireOperationEvent(t, logs, "close", "pending")
			}
			obj, err := h.client.Tracker().Get(Claims, "work", strings.Split(v.ID, ".")[0])
			if err != nil {
				t.Fatal(err)
			}
			c := obj.(*unstructured.Unstructured)
			if c.GetAnnotations()[operationKey] == "" {
				t.Fatal("unconfirmed holder was cleared")
			}
			if mode == "replacement" && c.GetAnnotations()[closeKey] != "" {
				t.Fatal("closed replacement Claim")
			}
		})
	}
}

func (r *runtimeStub) Execute(ctx context.Context, target Target, _ string) (Execution, error) {
	r.calls.Add(1)
	if err := target.Validate(ctx); err != nil {
		return Execution{}, err
	}
	if r.start != nil {
		close(r.start)
		select {
		case <-r.unblock:
		case <-ctx.Done():
			return Execution{}, ctx.Err()
		}
	}
	return Execution{Stdout: "ok", ExitCode: 7}, r.err
}
func (r *runtimeStub) Write(context.Context, Target, string, []byte) error {
	r.calls.Add(1)
	return r.err
}
func (r *runtimeStub) Read(context.Context, Target, string) ([]byte, error) {
	r.calls.Add(1)
	return []byte{0, 255}, r.err
}
func operationSetup(t *testing.T) (*harness, View, *runtimeStub) {
	h := setup(t)
	v := h.create(t)
	h.ready(t, v.ID)
	pod, _ := h.client.Tracker().Get(Pods, "work", "pod")
	obj := pod.DeepCopyObject()
	obj.(interface{ UnstructuredContent() map[string]interface{} }).UnstructuredContent()["status"].(map[string]any)["podIP"] = "10.0.0.12"
	if err := h.client.Tracker().Update(Pods, obj, "work"); err != nil {
		t.Fatal(err)
	}
	var err error
	v, err = h.service.Get(testCtx, "alice", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := &runtimeStub{}
	h.service.SetRuntime(r)
	return h, v, r
}
func TestOperationLockPersistsAcrossReplicasAndDoesNotBlockClose(t *testing.T) {
	h, v, r := operationSetup(t)
	r.start = make(chan struct{})
	r.unblock = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "print(1)"})
		done <- e
	}()
	<-r.start
	fresh := h.restart(t)
	fresh.SetRuntime(r)
	_, err := fresh.Read(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "a"})
	if Code(err) != "session_busy" {
		t.Fatalf("concurrent file reached runtime: %v", err)
	}
	_, err = fresh.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "print(2)"})
	if Code(err) != "session_busy" {
		t.Fatalf("concurrent execution accepted: %v", err)
	}
	active, err := fresh.Get(testCtx, "alice", v.ID)
	if err != nil || !active.OperationActive {
		t.Fatal("missing durable active operation")
	}
	if _, err = fresh.Close(testCtx, "alice", v.ID); err != nil {
		t.Fatal(err)
	}
	close(r.unblock)
	if Code(<-done) != "operation_outcome_unknown" || r.calls.Load() != 1 {
		t.Fatal("close race accepted result or replayed")
	}
}
func TestExecutionFailureResultKeepsSessionAndBinaryFiles(t *testing.T) {
	h, v, r := operationSetup(t)
	result, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "raise Exception()"})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("nonzero exit is not a result: %v", err)
	}
	file, err := h.service.Read(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "output.bin"})
	if err != nil || len(file.Content) != 2 || file.Content[1] != 255 {
		t.Fatal("binary read failed")
	}
	if h.claim(t, v.ID).GetAnnotations()[operationKey] != "" || r.calls.Load() != 2 {
		t.Fatal("lock leaked after completion")
	}
}
func TestUnknownExecutionDoesNotReplayAndRequestsClosure(t *testing.T) {
	for _, injected := range []error{errors.New("lost stream"), context.DeadlineExceeded, fail("execution_output_limit")} {
		t.Run(injected.Error(), func(t *testing.T) {
			h, v, r := operationSetup(t)
			r.err = injected
			_, err := h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "side_effect()"})
			if err == nil || r.calls.Load() != 1 {
				t.Fatal("unknown result replayed")
			}
			c := h.claim(t, v.ID)
			if c.GetAnnotations()[closeKey] != "true" || c.GetAnnotations()[operationKey] == "" {
				t.Fatal("uncertain execution made reusable")
			}
			fresh := h.restart(t)
			fresh.SetRuntime(r)
			_, err = fresh.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "side_effect()"})
			if Code(err) != "session_not_ready" || r.calls.Load() != 1 {
				t.Fatal("closed execution reached runtime")
			}
		})
	}
}
func TestOperationsRejectOwnerIdentityAndUnsafePathsBeforeRuntime(t *testing.T) {
	h, v, r := operationSetup(t)
	_, err := h.service.Execute(testCtx, "bob", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "print(1)"})
	if Code(err) != "not_found" {
		t.Fatal(err)
	}
	for _, rev := range []string{"", "wrong"} {
		_, err = h.service.Execute(testCtx, "alice", v.ID, ExecuteRequest{IdentityRevision: rev, Code: "print(1)"})
		if Code(err) != "identity_changed" {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/etc/passwd", "../escape", "a/../b", "a//b", "a/./b", ".", "bad\\path", "a\x00"} {
		_, err = h.service.Read(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: path})
		if Code(err) != "invalid_request" {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	_, err = h.service.Write(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "large", Content: make([]byte, MaxFileBytes+1)})
	if Code(err) != "invalid_request" {
		t.Fatal("oversized file accepted")
	}
	for _, req := range []ExecuteRequest{{Code: strings.Repeat("a", MaxCodeBytes+1)}, {Code: "a", TimeoutSeconds: 61}} {
		_, err = h.service.Execute(testCtx, "alice", v.ID, req)
		if Code(err) != "invalid_request" {
			t.Fatal(err)
		}
	}
	if r.calls.Load() != 0 {
		t.Fatal("rejected operation reached runtime")
	}
}
func TestKnownFileRejectionReleasesLock(t *testing.T) {
	h, v, r := operationSetup(t)
	r.err = fail("file_access_denied")
	_, err := h.service.Write(testCtx, "alice", v.ID, FileRequest{IdentityRevision: v.IdentityRevision, Path: "symlink"})
	if Code(err) != "file_access_denied" {
		t.Fatal(err)
	}
	c := h.claim(t, v.ID)
	if c.GetAnnotations()[closeKey] != "" || c.GetAnnotations()[operationKey] != "" {
		t.Fatal("definite refusal closed or blocked session")
	}
}
func TestCanceledExecutionUsesDetachedCloseBudget(t *testing.T) {
	h, v, r := operationSetup(t)
	r.start = make(chan struct{})
	r.unblock = make(chan struct{})
	ctx, cancel := context.WithCancel(testCtx)
	done := make(chan error, 1)
	go func() {
		_, e := h.service.Execute(ctx, "alice", v.ID, ExecuteRequest{IdentityRevision: v.IdentityRevision, Code: "wait()"})
		done <- e
	}()
	<-r.start
	cancel()
	select {
	case err := <-done:
		if Code(err) != "operation_outcome_unknown" {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation hung")
	}
	if h.claim(t, v.ID).GetAnnotations()[closeKey] != "true" {
		t.Fatal("canceled request prevented cleanup intent")
	}
}
