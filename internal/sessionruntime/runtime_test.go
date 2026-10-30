package sessionruntime

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	process "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type processServer struct {
	process.UnimplementedProcessServiceServer
	events []*process.StartResponse
	calls  atomic.Int32
	config chan *process.ProcessConfig
}

func (s *processServer) Start(req *process.StartRequest, stream process.ProcessService_StartServer) error {
	s.calls.Add(1)
	s.config <- req.GetConfig()
	for _, e := range s.events {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	return nil
}
func TestUpstreamStreamContractAndBoundedOutput(t *testing.T) {
	init := &process.StartResponse{Event: &process.StartResponse_Init{Init: &process.InitEvent{ProcessId: 1}}}
	stdout := func(v string) *process.StartResponse {
		return &process.StartResponse{Event: &process.StartResponse_Stdout{Stdout: []byte(v)}}
	}
	exit := &process.StartResponse{Event: &process.StartResponse_Exit{Exit: &process.ExitEvent{ExitCode: 9}}}
	for _, tc := range []struct {
		name   string
		events []*process.StartResponse
		code   string
		ok     bool
	}{
		{"nonzero exit", []*process.StartResponse{init, stdout("hello"), exit}, "", true},
		{"missing exit", []*process.StartResponse{init, stdout("partial")}, "", false},
		{"missing init", []*process.StartResponse{exit}, "", false},
		{"event after exit", []*process.StartResponse{init, exit, stdout("bad")}, "", false},
		{"bounded output", []*process.StartResponse{init, stdout(strings.Repeat("a", session.MaxOutputBytes)), stdout("b"), exit}, "execution_output_limit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := bufconn.Listen(256 << 10)
			server := grpc.NewServer()
			stub := &processServer{events: tc.events, config: make(chan *process.ProcessConfig, 1)}
			process.RegisterProcessServiceServer(server, stub)
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpc.NewClient("passthrough:///buffer", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := execute(ctx, process.NewProcessServiceClient(conn), "print('hello')")
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result: %#v %v", result, err)
			}
			if tc.code != "" && session.Code(err) != tc.code {
				t.Fatal(err)
			}
			if tc.ok && (result.ExitCode != 9 || result.Stdout != "hello") {
				t.Fatal("bad result")
			}
			cfg := <-stub.config
			if strings.Join(cfg.Command, "|") != "python3|-I|-B|-u|-c|print('hello')" || cfg.GetCwd() != "/workspace" || stub.calls.Load() != 1 {
				t.Fatal("wrong Python command or replay")
			}
		})
	}
}
func TestPinnedConnectionRevalidatesAndNeverReconnects(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		conn, e := listener.Accept()
		if e == nil {
			_, _ = io.Copy(io.Discard, conn)
			conn.Close()
		}
		close(done)
	}()
	var checks atomic.Int32
	target := session.Target{IP: "127.0.0.1", Validate: func(context.Context) error {
		if checks.Add(1) == 2 {
			return &session.Error{Code: "identity_changed"}
		}
		return nil
	}}
	dial := pinnedDial(target, listener.Addr().(*net.TCPAddr).Port)
	_, err = dial(context.Background(), "tcp", "ignored-service")
	if session.Code(err) != "identity_changed" || checks.Load() != 2 {
		t.Fatal("connection escaped post-connect validation")
	}
	if _, err = dial(context.Background(), "tcp", "ignored-service"); err == nil {
		t.Fatal("reconnection permitted")
	}
	<-done
}
