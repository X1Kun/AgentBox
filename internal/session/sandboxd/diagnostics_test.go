package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/requestid"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	process "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func TestConnectionCallbackPreservesRequestIdentityAndDialDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		conn, e := listener.Accept()
		if e == nil {
			io.Copy(io.Discard, conn)
			conn.Close()
		}
	}()
	origin := requestid.With(context.Background(), "request-dial")
	call, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := call.Deadline()
	checks := 0
	target := session.Target{IP: "127.0.0.1", Validate: func(ctx context.Context) error {
		if requestid.From(ctx) != "request-dial" {
			t.Error("lost callback correlation")
		}
		at, _ := ctx.Deadline()
		if !at.Equal(deadline) {
			t.Error("changed cancellation")
		}
		checks++
		return nil
	}}
	conn, err := observedDial(origin, target, listener.Addr().(*net.TCPAddr).Port, nil)(call, "tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	<-finished
	if checks != 2 {
		t.Fatal("lost UID revalidation", checks)
	}
}

func TestProcessTimelineHasCorrelationAndNeverLogsPayloads(t *testing.T) {
	listener := bufconn.Listen(256 << 10)
	server := grpc.NewServer()
	stub := &processServer{config: make(chan *process.ProcessConfig, 1), events: []*process.StartResponse{
		{Event: &process.StartResponse_Init{Init: &process.InitEvent{ProcessId: 42}}},
		{Event: &process.StartResponse_Stdout{Stdout: []byte("private stdout")}},
		{Event: &process.StartResponse_Stdout{Stdout: []byte("private second chunk")}},
		{Event: &process.StartResponse_Exit{Exit: &process.ExitEvent{ExitCode: 0}}},
	}}
	process.RegisterProcessServiceServer(server, stub)
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///buffer", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var logs bytes.Buffer
	r := &Runtime{metrics: telemetry.New("session"), logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	ctx, cancel := context.WithTimeout(requestid.With(context.Background(), "request-stream"), time.Second)
	defer cancel()
	_, err = executeObserved(ctx, process.NewProcessServiceClient(conn), "private user code", r, session.Target{SessionID: "session-id", PodUID: "pod-uid"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "private") {
		t.Fatal("payload in diagnostics")
	}
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			t.Fatal(line)
		}
		if e["request_id"] != "request-stream" || e["pod_uid"] != "pod-uid" || e["session_id"] != "session-id" {
			t.Fatal("unattributed event", e)
		}
		if e["phase"] == "end" {
			counts[e["stage"].(string)]++
		}
	}
	for _, stage := range []string{"runtime_rpc_start", "runtime_wait_init", "runtime_first_output", "runtime_process", "runtime_stream_finish"} {
		if counts[stage] != 1 {
			t.Fatal("missing/duplicate stage", stage, counts)
		}
	}
	if !strings.Contains(logs.String(), `"process_id":42`) || stub.calls.Load() != 1 {
		t.Fatal("lost process link or replayed command")
	}
}
