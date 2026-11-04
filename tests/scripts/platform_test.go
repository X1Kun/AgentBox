package scripts_test

import (
	"os"
	"os/exec"
	"testing"
)

func TestStaticBaseline(t *testing.T) {
	cmd := exec.Command("python3", "static_baseline_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static baseline regression: %v\n%s", err, output)
	}
}

func TestExperimentDiagnostics(t *testing.T) {
	cmd := exec.Command("python3", "experiment_diagnostics_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("experiment diagnostics: %v\n%s", err, output)
	}
}

func TestProductDeployment(t *testing.T) {
	cmd := exec.Command("python3", "product_deployment_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("product deployment: %v\n%s", err, output)
	}
}

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

func TestBoundaryWorkloadContracts(t *testing.T) {
	cmd := exec.Command("python3", "boundary_workloads_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workload contracts: %v\n%s", err, output)
	}
}

func TestSessionBoundaryTeardown(t *testing.T) {
	cmd := exec.Command("python3", "session_boundary_cleanup_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("boundary cleanup regression: %v\n%s", err, output)
	}
}
