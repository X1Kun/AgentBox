package scripts_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestPlatformLab(t *testing.T) {
	cmd := exec.Command("python3", "platform_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("platform lab regression suite: %v\n%s", err, output)
	}
}
