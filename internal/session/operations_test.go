package session

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeStub struct {
	calls   atomic.Int32
	start   chan struct{}
	unblock chan struct{}
	err     error
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
