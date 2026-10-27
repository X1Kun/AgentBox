package sessionprobe

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

type failingOperations struct {
	calls, writes int
	result        *sdk.ExecutionResult
	err           error
}

func (f *failingOperations) Run(context.Context, string, ...sdk.CallOption) (*sdk.ExecutionResult, error) {
	f.calls++
	return f.result, f.err
}
func (f *failingOperations) Write(context.Context, string, []byte, ...sdk.CallOption) error {
	f.writes++
	return nil
}
func (f *failingOperations) Read(context.Context, string, ...sdk.CallOption) ([]byte, error) {
	return nil, errors.New("unexpected read")
}
func (f *failingOperations) Exists(context.Context, string, ...sdk.CallOption) (bool, error) {
	return false, nil
}

func TestUnknownExecutionIsNotReplayed(t *testing.T) {
	a := &failingOperations{err: io.ErrUnexpectedEOF}
	b := &failingOperations{}
	_, err := Exercise(context.Background(), a, b, "10.244.1.2", "10.96.0.1")
	if err == nil || !strings.Contains(err.Error(), "no replay") || a.calls != 1 {
		t.Fatalf("uncertain execution was not stopped: calls=%d err=%v", a.calls, err)
	}
}
func TestUnexpectedExitCannotBecomeSuccessfulAcceptance(t *testing.T) {
	a := &failingOperations{result: &sdk.ExecutionResult{ExitCode: 23}}
	_, err := Exercise(context.Background(), a, &failingOperations{}, "10.244.1.2", "10.96.0.1")
	if err == nil || a.calls != 1 {
		t.Fatalf("unexpected exit passed or retried: calls=%d err=%v", a.calls, err)
	}
}
func TestWorkspaceBoundaryRequiresExplicitForbidden(t *testing.T) {
	for _, err := range []error{nil, io.EOF, &sdk.HTTPError{StatusCode: 404}, context.DeadlineExceeded} {
		if workspaceDenied(err) {
			t.Fatalf("non-denial counted as a boundary: %v", err)
		}
	}
	if !workspaceDenied(&sdk.HTTPError{StatusCode: 403}) {
		t.Fatal("explicit forbidden not recognized")
	}
}
func TestInvalidTargetRejectedBeforeOperations(t *testing.T) {
	a := &failingOperations{}
	if _, err := Exercise(context.Background(), a, a, "not-an-ip;echo bad", "10.96.0.1"); err == nil || a.writes != 0 || a.calls != 0 {
		t.Fatalf("invalid target used: %#v %v", a, err)
	}
}
