//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/localhttp"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

const runtimeClientToken = "gateway-real-client-token-with-32-characters"
const runtimeDaemonToken = "gateway-real-daemon-token-with-32-characters"

type gatewayCompletion struct {
	Result api.Result
	Err    error
}
type observedGatewayEngine struct {
	*sandbox.Engine
	completed chan gatewayCompletion
}

func (e *observedGatewayEngine) Execute(ctx context.Context, request api.Request) (api.Result, error) {
	result, err := e.Engine.Execute(ctx, request)
	e.completed <- gatewayCompletion{result, err}
	return result, err
}

type gatewayRuntimeFixture struct {
	engine         *sandbox.Engine
	config         sandbox.Config
	client, direct *http.Client
	url, socket    string
	command        *exec.Cmd
	stop           func()
	start          func()
	completed      chan gatewayCompletion
}

func gatewayRuntime(t *testing.T, mode string, maxInflight int) *gatewayRuntimeFixture {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runsc, cgroups and a Gateway binary")
	}
	if os.Geteuid() != 0 {
		t.Fatal("integration controller must be root to start an ordinary-user Gateway")
	}
	dir, err := os.MkdirTemp("/tmp", "ab-gateway-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chown(dir, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.Getenv("AGENTBOX_GATEWAY_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(filepath.Join(dir, "gateway"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	_ = input.Close()
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy Gateway: %v %v", copyErr, closeErr)
	}
	if err := os.Chmod(filepath.Join(dir, "gateway"), 0755); err != nil {
		t.Fatal(err)
	}
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Concurrency: 1, Mode: mode, WarmPoolSize: 2, MaxSandboxes: 4, QueueTimeout: 3 * time.Second}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "daemon.sock")
	listener, err := localhttp.Listen("unix:" + socket)
	if err != nil {
		t.Fatal(err)
	}
	// Models the deployed daemon's dedicated primary group.
	if err := os.Chown(socket, 0, 1000); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	completed := make(chan gatewayCompletion, 128)
	backend := &http.Server{Handler: httpapi.DeadlineHandler(httpapi.Handler(&observedGatewayEngine{e, completed}, runtimeDaemonToken, log), 50*time.Second)}
	go func() { _ = backend.Serve(listener) }()
	t.Cleanup(func() { _ = backend.Close() })
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	f := &gatewayRuntimeFixture{engine: e, config: c, socket: socket, url: "http://" + address, client: &http.Client{Timeout: 50 * time.Second}, completed: completed}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	f.direct = &http.Client{Transport: transport, Timeout: 50 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	f.start = func() {
		command := exec.Command(filepath.Join(dir, "gateway"), "-listen", address, "-daemon-socket", socket, "-max-inflight", strconv.Itoa(maxInflight))
		command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "AGENTBOX_GATEWAY_TOKEN=" + runtimeClientToken, "AGENTBOX_DAEMON_TOKEN=" + runtimeDaemonToken}
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
		logs := &bytes.Buffer{}
		command.Stdout, command.Stderr = logs, logs
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		f.command = command
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		stopped := false
		f.stop = func() {
			if stopped {
				return
			}
			stopped = true
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Gateway stop: %v; %s", err, logs.String())
				}
			case <-time.After(7 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("Gateway did not stop")
			}
		}
		waitRuntime(t, func() bool {
			response, err := f.client.Get(f.url + "/readyz")
			if err != nil {
				return false
			}
			_ = response.Body.Close()
			return response.StatusCode == 200
		})
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", command.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(status), "Uid:\t1000\t1000\t1000\t1000") {
			t.Fatal("Gateway is not running as UID 1000")
		}
	}
	t.Cleanup(func() {
		if f.stop != nil {
			f.stop()
		}
	})
	f.start()
	return f
}

func TestGatewayRuntime(t *testing.T) {
	f := gatewayRuntime(t, "cold", 8)
	client, url := f.client, f.url
	request, err := http.NewRequest("POST", url+"/v1/execute", strings.NewReader(`{"runtime":"python","code":"print('must-not-run')"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("Gateway accepted an unauthenticated execution")
	}
	code := "import os\nassert 'AGENTBOX_API_TOKEN' not in os.environ\nassert 'AGENTBOX_DAEMON_TOKEN' not in os.environ\nassert 'AGENTBOX_GATEWAY_TOKEN' not in os.environ\nprint('gateway-ok')"
	data, _ := json.Marshal(api.Request{Runtime: "python", Code: code, RequestID: "gateway-runtime", TimeoutMS: 5000})
	request, err = http.NewRequest("POST", url+"/v1/execute", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+runtimeClientToken)
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result api.Result
	decodeErr := json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	if decodeErr != nil || response.StatusCode != 200 || result.Status != "succeeded" || result.Stdout != "gateway-ok\n" || result.RequestID != "gateway-runtime" || !result.CleanupComplete {
		t.Fatalf("Gateway execution: HTTP %d %+v %v", response.StatusCode, result, decodeErr)
	}
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, f.config)
	t.Logf("ordinary Gateway UID=1000 -> Unix HTTP -> runsc: status=%s cleanup=%v", result.Status, result.CleanupComplete)
}

type gatewayHTTPResult struct {
	Code   int
	Data   []byte
	Err    error
	WallMS float64
}

func (f *gatewayRuntimeFixture) call(ctx context.Context, direct bool, path string, body *api.Request) gatewayHTTPResult {
	method, client, base, token := "GET", f.client, f.url, runtimeClientToken
	if direct {
		client, base, token = f.direct, "http://agentbox", runtimeDaemonToken
	}
	var raw io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		raw = bytes.NewReader(data)
		method = "POST"
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, raw)
	if err != nil {
		return gatewayHTTPResult{Err: err}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	begin := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return gatewayHTTPResult{Err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	return gatewayHTTPResult{response.StatusCode, data, err, float64(time.Since(begin)) / float64(time.Millisecond)}
}

func (f *gatewayRuntimeFixture) task(t *testing.T) string {
	t.Helper()
	var id string
	waitRuntime(t, func() bool {
		files, _ := filepath.Glob(filepath.Join(f.config.StateDir, "bundles", "*", "task.pid"))
		if len(files) != 1 {
			return false
		}
		id = filepath.Base(filepath.Dir(files[0]))
		return true
	})
	// task.pid confirms runsc exec has created the task; allow its sleep to begin.
	time.Sleep(50 * time.Millisecond)
	return id
}

func (f *gatewayRuntimeFixture) completion(t *testing.T, requestID string) api.Result {
	t.Helper()
	timer := time.NewTimer(7 * time.Second)
	defer timer.Stop()
	for {
		select {
		case done := <-f.completed:
			if done.Result.RequestID != requestID {
				continue
			}
			if done.Err != nil || !done.Result.CleanupComplete {
				t.Fatalf("real completion: %+v %v", done.Result, done.Err)
			}
			return done.Result
		case <-timer.C:
			t.Fatal("real daemon execution did not finish cleanup")
			return api.Result{}
		}
	}
}

func (f *gatewayRuntimeFixture) assertRemoved(t *testing.T, id string) {
	t.Helper()
	for _, path := range []string{filepath.Join(f.config.StateDir, "bundles", id), filepath.Join(f.config.CgroupRoot, "sandboxes", id)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("execution residue %s: %v", path, err)
		}
	}
	waitRuntime(t, func() bool {
		s := f.engine.Status()
		return s.Ready && s.Running == 0 && s.Leased == 0 && s.Destroying == 0 && s.Idle == 2 && s.Creating == 0
	})
}

func (f *gatewayRuntimeFixture) memory(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", f.command.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && (fields[0] == "VmRSS:" || fields[0] == "VmHWM:") {
			result[strings.TrimSuffix(fields[0], ":")+"_KiB"], _ = strconv.Atoi(fields[1])
		}
	}
	return result
}

func TestGatewayValidationRuntime(t *testing.T) {
	f := gatewayRuntime(t, "warm", 1)
	for _, stopGateway := range []bool{false, true} {
		name := "client-cancel"
		if stopGateway {
			name = "gateway-stop"
		}
		ctx, cancel := context.WithCancel(context.Background())
		request := api.Request{Runtime: "python", Code: "import time; time.sleep(20)", TimeoutMS: 30000, RequestID: name}
		pending := make(chan gatewayHTTPResult, 1)
		go func() { pending <- f.call(ctx, false, "/v1/execute", &request) }()
		id := f.task(t)
		if !stopGateway {
			rejected := f.call(context.Background(), false, "/v1/execute", &api.Request{Runtime: "python", Code: "print('must-not-run')"})
			if rejected.Err != nil || rejected.Code != 503 || !bytes.Contains(rejected.Data, []byte("gateway_capacity")) {
				t.Fatalf("overload: %+v", rejected)
			}
			t.Logf("GATEWAY_OVERLOAD rejection_ms=%.3f", rejected.WallMS)
			for _, path := range []string{"/readyz", "/v1/status"} {
				response := f.call(context.Background(), false, path, nil)
				if response.Err != nil || response.Code != 200 {
					t.Fatalf("control under load: %s %+v", path, response)
				}
				t.Logf("GATEWAY_CONTROL path=%s wall_ms=%.3f", path, response.WallMS)
			}
		}
		begin := time.Now()
		if stopGateway {
			f.stop()
		} else {
			cancel()
		}
		result := f.completion(t, name)
		if result.Status != "cancelled" || result.ExecutionID != id {
			t.Fatalf("cancelled real task: %+v", result)
		}
		f.assertRemoved(t, id)
		t.Logf("GATEWAY_RECOVERY case=%s cleanup_and_refill_ms=%.3f cleanup=true", name, float64(time.Since(begin))/float64(time.Millisecond))
		cancel()
		select {
		case <-pending:
		case <-time.After(time.Second):
			t.Fatal("frontend request did not terminate")
		}
		if stopGateway {
			f.start()
		}
		response := f.call(context.Background(), false, "/v1/execute", &api.Request{Runtime: "python", Code: "print('recovered')", RequestID: name + "-recovered"})
		var recovered api.Result
		if err := json.Unmarshal(response.Data, &recovered); err != nil || response.Code != 200 || !recovered.CleanupComplete || recovered.Stdout != "recovered\n" {
			t.Fatalf("capacity not restored: %+v %v", response, err)
		}
		f.completion(t, name+"-recovered")
	}
	before := f.memory(t)
	large := f.call(context.Background(), false, "/v1/execute", &api.Request{Runtime: "python", Code: "import os\ndata = memoryview(b'\\x00' * (1 << 20))\nwhile data:\n    n = os.write(1, data[:8192])\n    assert n > 0\n    data = data[n:]", RequestID: "large-output"})
	var output api.Result
	if err := json.Unmarshal(large.Data, &output); err != nil || large.Code != 200 || output.Status != "succeeded" || len(output.Stdout) != 1<<20 || output.OutputTruncated || !output.CleanupComplete {
		t.Fatalf("large output: HTTP %d bytes=%d decode=%v status=%s", large.Code, len(large.Data), err, output.Status)
	}
	f.completion(t, "large-output")
	t.Logf("GATEWAY_LARGE_OUTPUT json_bytes=%d before=%v after=%v", len(large.Data), before, f.memory(t))
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, f.config)
}

func TestGatewayLatencyRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_GATEWAY_MEASURE") != "1" {
		t.Skip("explicit small-sample measurement only")
	}
	f := gatewayRuntime(t, "warm", 8)
	for pair := -3; pair < 30; pair++ {
		routes := []bool{true, false}
		if pair%2 != 0 {
			routes = []bool{false, true}
		}
		for _, direct := range routes {
			waitRuntime(t, func() bool {
				s := f.engine.Status()
				return s.Idle == 2 && s.Creating == 0 && s.Running == 0 && s.Destroying == 0
			})
			id := fmt.Sprintf("pair-%d-%v", pair, direct)
			response := f.call(context.Background(), direct, "/v1/execute", &api.Request{Runtime: "python", Code: "print(1)", RequestID: id})
			var result api.Result
			if err := json.Unmarshal(response.Data, &result); err != nil || response.Err != nil || response.Code != 200 || result.Status != "succeeded" || !result.CleanupComplete || result.Stdout != "1\n" {
				t.Fatalf("measurement failure: HTTP %d err=%v decode=%v result=%+v", response.Code, response.Err, err, result)
			}
			f.completion(t, id)
			if pair < 0 {
				continue
			}
			route := "gateway"
			if direct {
				route = "unix-direct"
			}
			row := map[string]any{"pair": pair, "route": route, "wall_ms": response.WallMS, "timings": result.Timings, "outside_engine_ms": response.WallMS - result.Timings.TotalMS, "http": response.Code, "status": result.Status, "cleanup_complete": result.CleanupComplete}
			encoded, _ := json.Marshal(row)
			t.Logf("GATEWAY_SAMPLE %s", encoded)
		}
	}
	t.Logf("GATEWAY_MEMORY %v", f.memory(t))
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, f.config)
}
