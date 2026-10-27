// Package sessionprobe implements trusted acceptance scenarios, not a public executor.
package sessionprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	sdk "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

type Operations interface {
	Run(context.Context, string, ...sdk.CallOption) (*sdk.ExecutionResult, error)
	Write(context.Context, string, []byte, ...sdk.CallOption) error
	Read(context.Context, string, ...sdk.CallOption) ([]byte, error)
	Exists(context.Context, string, ...sdk.CallOption) (bool, error)
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// Exercise deliberately issues every command once. On an uncertain transport
// result the caller must close the session, rather than replaying its command.
func Exercise(ctx context.Context, a, b Operations, peerIP, apiIP string) ([]Check, error) {
	if net.ParseIP(peerIP) == nil || net.ParseIP(apiIP) == nil {
		return nil, errors.New("invalid probe IP")
	}
	checks := []Check{}
	mark := func(name string) { checks = append(checks, Check{Name: name, Passed: true}) }
	run := func(command string, exit int, stdout string) error {
		result, err := a.Run(ctx, command, sdk.WithTimeout(15*time.Second), sdk.WithWorkingDir("."))
		if err != nil {
			return fmt.Errorf("execution outcome uncertain; no replay: %w", err)
		}
		if result == nil || result.ExitCode != exit || (stdout != "" && result.Stdout != stdout) || len(result.Stdout)+len(result.Stderr) > 4096 {
			return fmt.Errorf("unexpected execution result for acceptance command (expected exit %d)", exit)
		}
		return nil
	}
	if err := a.Write(ctx, "input.json", []byte(`{"values":[1,2,3]}`)); err != nil {
		return checks, err
	}
	if err := a.Write(ctx, "calc.py", []byte("import json\nfrom pathlib import Path\nv=sum(json.loads(Path('input.json').read_text())['values'])\nPath('result.json').write_text(json.dumps({'sum':v}))\nprint(v)\n")); err != nil {
		return checks, err
	}
	if err := run("python3 -I -B calc.py", 0, "6\n"); err != nil {
		return checks, err
	}
	data, err := a.Read(ctx, "result.json")
	if err != nil {
		return checks, err
	}
	var result struct {
		Sum int `json:"sum"`
	}
	if json.Unmarshal(data, &result) != nil || result.Sum != 6 {
		return checks, errors.New("result file validation failed")
	}
	if err := run("python3 -I -B -c 'import json; print(json.load(open(\"result.json\"))[\"sum\"])'", 0, "6\n"); err != nil {
		return checks, err
	}
	mark("multi_call_file_persistence")
	exists, err := b.Exists(ctx, "input.json")
	if err != nil {
		return checks, err
	}
	if exists {
		return checks, errors.New("session B can see session A input")
	}
	if err := b.Write(ctx, "b-only", []byte("isolated")); err != nil {
		return checks, err
	}
	exists, err = a.Exists(ctx, "b-only")
	if err != nil {
		return checks, err
	}
	if exists {
		return checks, errors.New("session A can see session B file")
	}
	mark("cross_session_files_isolated")
	if err := run("python3 -I -B -c 'raise SystemExit(23)'", 23, ""); err != nil {
		return checks, err
	}
	if err := run("python3 -I -B -c 'print(42)'", 0, "42\n"); err != nil {
		return checks, err
	}
	mark("program_failure_does_not_end_session")
	if err := run("python3 -I -B -c 'x=10'", 0, ""); err != nil {
		return checks, err
	}
	if err := run("python3 -I -B -c 'print(x)'", 1, ""); err != nil {
		return checks, err
	}
	mark("fresh_python_interpreter_per_command")
	boundary := "import os\nfrom pathlib import Path\nassert os.getuid()==1000\nassert os.statvfs('/').f_flag & os.ST_RDONLY\nassert not Path('/var/run/secrets/kubernetes.io/serviceaccount/token').exists()\nprint('bounded')\n"
	if err := a.Write(ctx, "boundary.py", []byte(boundary)); err != nil {
		return checks, err
	}
	if err := run("python3 -I -B boundary.py", 0, "bounded\n"); err != nil {
		return checks, err
	}
	mark("nonroot_readonly_no_cluster_token")
	if err := run("ln -s /etc/passwd escape", 0, ""); err != nil {
		return checks, err
	}
	if _, err := a.Read(ctx, "escape"); !workspaceDenied(err) {
		return checks, errors.New("file API did not explicitly deny an escaping symlink")
	}
	if _, err := a.Read(ctx, "../etc/passwd"); !workspaceDenied(err) {
		return checks, errors.New("file API did not explicitly deny traversal")
	}
	mark("file_api_workspace_boundary")
	targets, _ := json.Marshal([][]any{{peerIP, 8080}, {peerIP, 9090}, {apiIP, 443}})
	network := fmt.Sprintf("import json,socket\ntargets=json.loads(%q)\nfor ip,port in targets:\n for _ in range(2):\n  with socket.socket() as s:\n   s.settimeout(0.7)\n   try: s.connect((ip,port))\n   except socket.timeout: continue\n   raise AssertionError('unexpected sandbox egress')\nprint('blocked')\n", string(targets))
	if err := a.Write(ctx, "network.py", []byte(network)); err != nil {
		return checks, err
	}
	if err := run("python3 -I -B network.py", 0, "blocked\n"); err != nil {
		return checks, err
	}
	mark("peer_and_cluster_api_egress_blocked")
	return checks, nil
}

func ShortError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToValidUTF8(err.Error(), "?")
	if len(s) > 2048 {
		s = s[:2048] + "..."
	}
	return s
}

func workspaceDenied(err error) bool {
	var response *sdk.HTTPError
	return errors.As(err, &response) && response.StatusCode == 403
}
