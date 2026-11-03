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

func TestPlatformGVisor(t *testing.T) {
	cmd := exec.Command("python3", "platform_gvisor_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gVisor platform regression suite: %v\n%s", err, output)
	}
}

func TestPlatformNetwork(t *testing.T) {
	cmd := exec.Command("python3", "platform_network_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("network platform regression suite: %v\n%s", err, output)
	}
}

func TestPlatformSessions(t *testing.T) {
	cmd := exec.Command("python3", "platform_sessions_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("session platform regression suite: %v\n%s", err, output)
	}
}

func TestLegacyRetirementGuards(t *testing.T) {
	cmd := exec.Command("python3", "retirement_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("retirement guards: %v\n%s", err, output)
	}
}

func TestPlatformManifests(t *testing.T) {
	cmd := exec.Command("python3", "platform_manifests_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("manifest regression: %v\n%s", err, output)
	}
}

func TestPlatformBatch(t *testing.T) {
	cmd := exec.Command("python3", "platform_batch_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("batch platform regression: %v\n%s", err, output)
	}
}

func TestBatchImageProduct(t *testing.T) {
	cmd := exec.Command("python3", "platform_batch_product_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("image batch product regression: %v\n%s", err, output)
	}
}

func TestPlatformObservability(t *testing.T) {
	cmd := exec.Command("python3", "platform_observability_test.py", "-v")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("observability regression: %v\n%s", err, output)
	}
}
