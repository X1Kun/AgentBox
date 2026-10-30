// Package sessionruntime adapts the pinned upstream sandboxd protocol. It does
// not create workloads or run code in the API process.
package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/X1Kun/AgentBox/internal/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	process "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

type Runtime struct{}

func New() *Runtime             { return &Runtime{} }
func failure(code string) error { return &session.Error{Code: code} }

// No Service DNS, proxies, redirects, connection reuse or reconnection. After
// TCP establishment, verify the original Pod UID/IP again before returning the
// connection to the protocol client. A reconnect must fail, never retarget.
func pinnedDial(target session.Target, port int) func(context.Context, string, string) (net.Conn, error) {
	var used atomic.Bool
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if !used.CompareAndSwap(false, true) {
			return nil, errors.New("runtime reconnection refused")
		}
		if err := target.Validate(ctx); err != nil {
			return nil, err
		}
		d := net.Dialer{Timeout: 3 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(target.IP, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		if err = target.Validate(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
func (r *Runtime) Execute(ctx context.Context, target session.Target, code string) (session.Execution, error) {
	dial := pinnedDial(target, 9090)
	conn, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(target.IP, "9090"),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) { return dial(ctx, "tcp", address) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<10), grpc.MaxRetryRPCBufferSize(0)))
	if err != nil {
		return session.Execution{}, err
	}
	defer conn.Close()
	return execute(ctx, process.NewProcessServiceClient(conn), code)
}
func execute(ctx context.Context, client process.ProcessServiceClient, code string) (session.Execution, error) {
	cwd := "/workspace"
	stream, err := client.Start(ctx, &process.StartRequest{Config: &process.ProcessConfig{
		Command: []string{"python3", "-I", "-B", "-u", "-c", code}, Cwd: &cwd}}, grpc.MaxRetryRPCBufferSize(0))
	if err != nil {
		return session.Execution{}, err
	}
	var result session.Execution
	initialized, exited := false, false
	for {
		event, e := stream.Recv()
		if e == io.EOF {
			if !initialized || !exited {
				return session.Execution{}, errors.New("incomplete execution stream")
			}
			return result, nil
		}
		if e != nil {
			return session.Execution{}, e
		}
		if exited {
			return session.Execution{}, errors.New("event after exit")
		}
		if !initialized {
			if event.GetInit() == nil || event.GetInit().GetProcessId() == 0 {
				return session.Execution{}, errors.New("missing process initialization")
			}
			initialized = true
			continue
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
				return session.Execution{}, errors.New("empty exit")
			}
			result.ExitCode = int(value.Exit.GetExitCode())
			exited = true
		default:
			return session.Execution{}, errors.New("unexpected process event")
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
func fileCall(ctx context.Context, target session.Target, method, path string, content []byte) (*http.Response, error) {
	transport := &http.Transport{DialContext: pinnedDial(target, 8080), DisableKeepAlives: true, ResponseHeaderTimeout: 3 * time.Second, MaxResponseHeaderBytes: 16 << 10}
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
	response, err := fileCall(ctx, target, http.MethodPut, path, content)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}
func (r *Runtime) Read(ctx context.Context, target session.Target, path string) ([]byte, error) {
	response, err := fileCall(ctx, target, http.MethodGet, path, nil)
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
