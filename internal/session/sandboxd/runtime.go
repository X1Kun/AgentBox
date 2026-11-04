// Package sandboxd adapts the pinned upstream sandboxd protocol. It does
// not create workloads or run code in the API process.
package sandboxd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/X1Kun/AgentBox/internal/requestid"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	process "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type Runtime struct {
	metrics *telemetry.Metrics
	logger  *slog.Logger
}

func New() *Runtime             { return &Runtime{} }
func failure(code string) error { return &session.Error{Code: code} }

// No Service DNS, proxies, redirects, connection reuse or reconnection. After
// TCP establishment, verify the original Pod UID/IP again before returning the
// connection to the protocol client. A reconnect must fail, never retarget.
func pinnedDial(target session.Target, port int) func(context.Context, string, string) (net.Conn, error) {
	return observedDial(context.Background(), target, port, nil)
}
func observedDial(origin context.Context, target session.Target, port int, runtime *Runtime) func(context.Context, string, string) (net.Conn, error) {
	var used atomic.Bool
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		// gRPC's connection context is distinct from its RPC context. Preserve
		// the dial cancellation/deadline while restoring request correlation.
		if id := requestid.From(origin); id != "" {
			ctx = requestid.With(ctx, id)
		}
		if !used.CompareAndSwap(false, true) {
			return nil, errors.New("runtime reconnection refused")
		}
		done := runtime.begin(ctx, target, "runtime_validate_before")
		err := target.Validate(ctx)
		done(err)
		if err != nil {
			return nil, err
		}
		d := net.Dialer{Timeout: 3 * time.Second}
		done = runtime.begin(ctx, target, "runtime_connect")
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(target.IP, strconv.Itoa(port)))
		done(err)
		if err != nil {
			return nil, err
		}
		done = runtime.begin(ctx, target, "runtime_validate_after")
		err = target.Validate(ctx)
		done(err)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
func (r *Runtime) Execute(ctx context.Context, target session.Target, code string) (session.Execution, error) {
	dial := observedDial(ctx, target, 9090, r)
	conn, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(target.IP, "9090"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) { return dial(ctx, "tcp", address) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<10), grpc.MaxRetryRPCBufferSize(0)))
	if err != nil {
		return session.Execution{}, err
	}
	defer conn.Close()
	return executeObserved(ctx, process.NewProcessServiceClient(conn), code, r, target)
}
func execute(ctx context.Context, client process.ProcessServiceClient, code string) (session.Execution, error) {
	return executeObserved(ctx, client, code, nil, session.Target{})
}
func executeObserved(ctx context.Context, client process.ProcessServiceClient, code string, runtime *Runtime, target session.Target) (result session.Execution, resultErr error) {
	cwd := "/workspace"
	done := runtime.begin(ctx, target, "runtime_rpc_start")
	stream, err := client.Start(ctx, &process.StartRequest{Config: &process.ProcessConfig{
		Command: []string{"python3", "-I", "-B", "-u", "-c", code}, Cwd: &cwd}}, grpc.MaxRetryRPCBufferSize(0))
	done(err)
	if err != nil {
		return session.Execution{}, err
	}
	initialized, exited := false, false
	waitInit := runtime.begin(ctx, target, "runtime_wait_init")
	var processDone, streamDone func(error)
	var initializedAt time.Time
	var processID int64
	firstOutput := false
	defer func() {
		if !initialized {
			waitInit(resultErr)
		} else if !exited {
			processDone(resultErr)
		} else {
			streamDone(resultErr)
		}
	}()
	for {
		event, e := stream.Recv()
		if e == io.EOF {
			if !initialized || !exited {
				return session.Execution{}, &protocolError{}
			}
			return result, nil
		}
		if e != nil {
			return session.Execution{}, e
		}
		if exited {
			return session.Execution{}, &protocolError{}
		}
		if !initialized {
			if event.GetInit() == nil || event.GetInit().GetProcessId() == 0 {
				return session.Execution{}, &protocolError{}
			}
			processID = int64(event.GetInit().GetProcessId())
			waitInit(nil)
			initialized = true
			initializedAt = time.Now()
			processDone = runtime.begin(ctx, target, "runtime_process", "process_id", processID)
			continue
		}
		if !firstOutput && (event.GetStdout() != nil || event.GetStderr() != nil) {
			runtime.end(ctx, target, "runtime_first_output", initializedAt, nil, "process_id", processID)
			firstOutput = true
		}
		switch value := event.GetEvent().(type) {
		case *process.StartResponse_Stdout:
			if len(result.Stdout)+len(result.Stderr)+len(value.Stdout) > session.MaxOutputBytes {
				return session.Execution{}, failure("execution_output_limit")
			}
			result.Stdout += string(value.Stdout)
		case *process.StartResponse_Stderr:
			if len(result.Stdout)+len(result.Stderr)+len(value.Stderr) > session.MaxOutputBytes {
				return session.Execution{}, failure("execution_output_limit")
			}
			result.Stderr += string(value.Stderr)
		case *process.StartResponse_Exit:
			if value.Exit == nil {
				return session.Execution{}, &protocolError{}
			}
			result.ExitCode = int(value.Exit.GetExitCode())
			processDone(nil)
			exited = true
			streamDone = runtime.begin(ctx, target, "runtime_stream_finish", "process_id", processID, "exit_code", result.ExitCode)
		default:
			return session.Execution{}, &protocolError{}
		}
	}
}
func fileURL(target session.Target, path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "http://" + net.JoinHostPort(target.IP, "8080") + "/v1/files/" + strings.Join(parts, "/")
}
func (r *Runtime) fileCall(ctx context.Context, target session.Target, method, path string, content []byte) (*http.Response, error) {
	transport := &http.Transport{DialContext: observedDial(ctx, target, 8080, r), DisableKeepAlives: true, ResponseHeaderTimeout: 3 * time.Second, MaxResponseHeaderBytes: 16 << 10}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, method, fileURL(target, path), bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil // a mutation must never become replayable by the transport
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case 404:
			return nil, failure("file_not_found")
		case 403:
			return nil, failure("file_access_denied")
		case 400:
			return nil, failure("invalid_path")
		default:
			return nil, failure("runtime_unavailable")
		}
	}
	return resp, nil
}
func (r *Runtime) Write(ctx context.Context, target session.Target, path string, content []byte) error {
	response, err := r.fileCall(ctx, target, http.MethodPut, path, content)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}
func (r *Runtime) Read(ctx context.Context, target session.Target, path string) ([]byte, error) {
	response, err := r.fileCall(ctx, target, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "application/octet-stream" {
		return nil, failure("invalid_path")
	}
	value, err := io.ReadAll(io.LimitReader(response.Body, session.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > session.MaxFileBytes {
		return nil, failure("file_too_large")
	}
	return value, nil
}
