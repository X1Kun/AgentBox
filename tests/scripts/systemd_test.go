package scripts_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestSystemdDeployment(t *testing.T) {
	cmd := exec.Command("python3", "systemd_deploy_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deployment regression suite: %v\n%s", err, output)
	}
}
