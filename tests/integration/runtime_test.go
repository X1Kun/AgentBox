//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

func TestRealRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires explicit AGENTBOX_INTEGRATION=1 and an isolated delegated runtime environment")
	}
	state := filepath.Join(t.TempDir(), "state")
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: state, CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Concurrency: 1, OutputLimit: 16384}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	execute := func(t *testing.T, code string, timeout int) api.Result {
		t.Helper()
		r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: code, TimeoutMS: timeout, Stdin: "input-data\n", Env: map[string]string{"AGENT_VALUE": "allowed"}})
		if err != nil {
			t.Fatalf("execute: %v; result=%+v", err, r)
		}
		if !r.CleanupComplete {
			t.Fatal("cleanup not confirmed")
		}
		entries, err := os.ReadDir(filepath.Join(state, "bundles"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("bundle residue: %v %v", entries, err)
		}
		entries, err = os.ReadDir(filepath.Join(c.CgroupRoot, "sandboxes"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				t.Fatalf("cgroup residue: %s", entry.Name())
			}
		}
		t.Logf("status=%s setup=%.1fms execute=%.1fms cleanup=%.1fms peak=%d", r.Status, r.Timings.SetupMS, r.Timings.ExecutionMS, r.Timings.CleanupMS, r.Usage.MemoryPeakBytes)
		return r
	}
	t.Run("http_execute", func(t *testing.T) {
		const token = "integration-test-token-with-32-characters"
		server := httptest.NewServer(httpapi.Handler(e, token, slog.New(slog.NewTextHandler(io.Discard, nil))))
		defer server.Close()
		request, err := http.NewRequest("POST", server.URL+"/v1/execute", strings.NewReader(`{"runtime":"python","code":"print('http-ok')"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result api.Result
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || result.Status != "succeeded" || result.Stdout != "http-ok\n" || !result.CleanupComplete {
			t.Fatalf("HTTP execution: %d %+v", response.StatusCode, result)
		}
	})
	t.Run("stdio_and_environment", func(t *testing.T) {
		r := execute(t, "import os, sys\nprint(input())\nprint(os.environ['AGENT_VALUE'])\nprint('AGENTBOX_API_TOKEN' in os.environ)\nprint('error-stream', file=sys.stderr)\n", 5000)
		if r.Status != "succeeded" || r.Stdout != "input-data\nallowed\nFalse\n" || r.Stderr != "error-stream\n" || r.ExitCode == nil || *r.ExitCode != 0 {
			t.Fatalf("unexpected result: %+v", r)
		}
	})
	t.Run("guest_input_permissions", func(t *testing.T) {
		code := `import os, stat
assert os.getuid() == 1000
assert stat.S_IMODE(os.stat('/agentbox').st_mode) == 0o755
assert stat.S_IMODE(os.stat('/agentbox/task.json').st_mode) == 0o444
assert stat.S_IMODE(os.stat('/agentbox/stdin').st_mode) == 0o600
assert not os.access('/agentbox/stdin', os.R_OK)
assert input() == 'input-data'
print('guest-input-ok')
`
		r := execute(t, code, 5000)
		if r.Status != "succeeded" || r.Stdout != "guest-input-ok\n" {
			t.Fatalf("guest input permissions: %+v", r)
		}
	})
	t.Run("main_module_namespace", func(t *testing.T) {
		code := `import __main__, pickle, sys
from dataclasses import dataclass
assert __main__.__dict__ is globals()
assert __file__ == '/workspace/main.py'
assert sys.argv == [__file__]
assert 'task' not in globals()
def top_level_function():
    return 42
assert __main__.top_level_function is top_level_function
assert pickle.loads(pickle.dumps(top_level_function)) is top_level_function
@dataclass
class Item:
    value: int
item = Item(42)
assert pickle.loads(pickle.dumps(item)) == item
print('main-module-ok')
`
		r := execute(t, code, 5000)
		if r.Status != "succeeded" || r.Stdout != "main-module-ok\n" {
			t.Fatalf("main module namespace: %+v", r)
		}
	})
	t.Run("user_failure", func(t *testing.T) {
		r := execute(t, "raise ValueError('expected')", 5000)
		if r.Status != "failed" || !strings.Contains(r.Stderr, "ValueError: expected") {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("user_exit_125_is_not_runtime_error", func(t *testing.T) {
		r := execute(t, "import os; os._exit(125)", 5000)
		if r.Status != "failed" || r.ExitCode == nil || *r.ExitCode != 125 {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("user_exit_128_is_not_runtime_error", func(t *testing.T) {
		r := execute(t, "import os; os._exit(128)", 5000)
		if r.Status != "failed" || r.ExitCode == nil || *r.ExitCode != 128 {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		r := execute(t, "while True: pass", 200)
		if r.Status != "timeout" {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("output_flood", func(t *testing.T) {
		r := execute(t, "import os\nwhile True: os.write(1, b'x'*8192)", 5000)
		if r.Status != "output_limit" || !r.OutputTruncated || len(r.Stdout)+len(r.Stderr) > 16384 {
			t.Fatalf("unexpected output result: status=%s size=%d", r.Status, len(r.Stdout)+len(r.Stderr))
		}
	})
	t.Run("filesystem_and_network", func(t *testing.T) {
		code := `import os, socket
assert os.getuid() == 1000
for path in ['/etc/agentbox-sentinel', '/usr/local/lib/agentbox-sentinel']:
    try:
        open(path, 'w').write('unexpected')
        raise AssertionError('rootfs writable')
    except OSError:
        pass
open('/workspace/marker', 'w').write('private')
assert not os.path.exists('/dev/kvm')
assert not os.path.exists('/var/run/docker.sock')
s = socket.socket()
s.settimeout(0.3)
try:
    s.connect(('169.254.169.254', 80))
    raise AssertionError('egress succeeded')
except OSError:
    pass
print('isolated')
`
		r := execute(t, code, 5000)
		if r.Status != "succeeded" || r.Stdout != "isolated\n" {
			t.Fatalf("unexpected: %+v", r)
		}
		r = execute(t, "import os; assert not os.path.exists('/workspace/marker'); print('fresh')", 5000)
		if r.Status != "succeeded" || r.Stdout != "fresh\n" {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("background_child_is_destroyed", func(t *testing.T) {
		r := execute(t, "import os, time\npid=os.fork()\nif pid==0:\n    while True: time.sleep(1)\nprint('parent-done')", 5000)
		if r.Status != "succeeded" || r.Stdout != "parent-done\n" {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("process_limit", func(t *testing.T) {
		code := `import os, resource, signal, time
assert resource.getrlimit(resource.RLIMIT_NPROC) == (64, 64)
# Exercise the same mechanism at 8 processes, avoiding KVM address-space
# overhead hitting memory.high before the configured 64-process boundary.
resource.setrlimit(resource.RLIMIT_NPROC, (8, 8))
children = []
try:
    for i in range(12):
        try:
            pid = os.fork()
        except OSError:
            print('fork-limited', len(children))
            break
        if pid == 0:
            time.sleep(10)
            os._exit(0)
        children.append(pid)
    else:
        raise AssertionError('process limit not enforced')
finally:
    for pid in children:
        os.kill(pid, signal.SIGKILL)
    for pid in children:
        os.waitpid(pid, 0)
`
		r := execute(t, code, 5000)
		if r.Status != "succeeded" || !strings.HasPrefix(r.Stdout, "fork-limited ") {
			t.Fatalf("unexpected: %+v", r)
		}
	})
	t.Run("memory_oom", func(t *testing.T) {
		// Disable only the soft watermark for this test. Otherwise direct
		// reclaim at memory.high can stall allocation until the wall timeout,
		// legitimately preventing the hard-limit OOM this case needs to test.
		adjusted := make(chan error, 1)
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				// Wait until task creation is confirmed: runtime initialization
				// may still write cgroup settings before the exec task exists.
				matches, err := filepath.Glob(filepath.Join(state, "bundles", "ab-*", "task.pid"))
				if err != nil {
					adjusted <- err
					return
				}
				for _, path := range matches {
					id := filepath.Base(filepath.Dir(path))
					high := filepath.Join(c.CgroupRoot, "sandboxes", id, "memory.high")
					if err := os.WriteFile(high, []byte("536870912"), 0644); err != nil {
						adjusted <- err
						return
					}
					adjusted <- nil
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			adjusted <- errors.New("OOM test did not find its cgroup")
		}()
		r := execute(t, "import time\ntime.sleep(0.1)\nchunks=[]\nwhile True: chunks.append(bytearray(8*1024*1024))", 10000)
		if err := <-adjusted; err != nil {
			t.Fatal(err)
		}
		if r.Status != "oom" || r.Usage.OOMKills == 0 {
			t.Fatalf("OOM not identified: %+v", r)
		}
	})
	t.Run("cancellation_and_capacity", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		type answer struct {
			result api.Result
			err    error
		}
		done := make(chan answer, 1)
		go func() {
			r, err := e.Execute(ctx, api.Request{Runtime: "python", Code: "import time; time.sleep(20)", TimeoutMS: 30000})
			done <- answer{r, err}
		}()
		defer cancel()
		time.Sleep(200 * time.Millisecond)
		_, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('must-not-run')"})
		if !errors.Is(err, sandbox.ErrBusy) {
			t.Fatalf("expected busy, got %v", err)
		}
		cancel()
		select {
		case a := <-done:
			if a.err != nil || a.result.Status != "cancelled" || !a.result.CleanupComplete {
				t.Fatalf("cancel: %+v %v", a.result, a.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancellation did not finish")
		}
		r := execute(t, "print('healthy')", 5000)
		if r.Status != "succeeded" {
			t.Fatalf("executor failed to recover: %+v", r)
		}
	})
	t.Run("restart_reconciles_owned_intents", func(t *testing.T) {
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		id := "ab-00000000000000000000000000000001"
		bundle := filepath.Join(state, "bundles", id)
		group := filepath.Join(c.CgroupRoot, "sandboxes", id)
		if err := os.Mkdir(bundle, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(group, 0755); err != nil {
			t.Fatal(err)
		}
		restarted, err := sandbox.New(c)
		if err != nil {
			t.Fatal(err)
		}
		e = restarted
		for _, path := range []string{bundle, group} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned residue survived restart: %s %v", path, err)
			}
		}
		r := execute(t, "print('restarted')", 5000)
		if r.Status != "succeeded" {
			t.Fatalf("post-restart execution: %+v", r)
		}
	})
	t.Run("cancel_during_setup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		r, err := e.Execute(ctx, api.Request{Runtime: "python", Code: "print('cancel-setup')"})
		if err != nil || r.Status != "cancelled" || !r.CleanupComplete {
			t.Fatalf("setup cancellation: %+v %v", r, err)
		}
	})
	t.Run("cleanup_failure_blocks_admission_and_restart_repairs", func(t *testing.T) {
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		marker := filepath.Join(dir, "fail-delete")
		wrapper := filepath.Join(dir, "runsc-wrapper")
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
		script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = delete ] && [ -f " + quote(marker) + " ]; then exit 77; fi\ndone\nexec " + quote(c.Runsc) + " \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		injected := c
		injected.Runsc = wrapper
		replacement, err := sandbox.New(injected)
		if err != nil {
			t.Fatal(err)
		}
		e = replacement
		if err := os.WriteFile(marker, []byte("fail"), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('cleanup-injected')"})
		if err == nil || r.Status != "infrastructure_error" || r.CleanupComplete || e.Ready() {
			t.Fatalf("cleanup failure was hidden: %+v %v", r, err)
		}
		if _, err := os.Stat(filepath.Join(state, "bundles", r.ExecutionID)); err != nil {
			t.Fatal("cleanup intent was lost", err)
		}
		_, err = e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('must-not-run')"})
		if !errors.Is(err, sandbox.ErrUnhealthy) {
			t.Fatalf("broken executor admitted work: %v", err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		replacement, err = sandbox.New(c)
		if err != nil {
			t.Fatal(err)
		}
		e = replacement
		r = execute(t, "print('repaired')", 5000)
		if r.Status != "succeeded" || r.Stdout != "repaired\n" {
			t.Fatalf("restart did not repair cleanup: %+v", r)
		}
	})
}
