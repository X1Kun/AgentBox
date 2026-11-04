package architecture_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestClientAndAgentDoNotImportBackendImplementations(t *testing.T) {
	for _, target := range []string{"session/client", "agent", "session/protocol", "batchprotocol", "toolcontract"} {
		t.Run(target, func(t *testing.T) {
			output, err := exec.Command("go", "list", "-mod=readonly", "-deps", "github.com/X1Kun/AgentBox/internal/"+target).CombinedOutput()
			if err != nil {
				t.Fatalf("dependency inspection: %v\n%s", err, output)
			}
			for _, dependency := range strings.Fields(string(output)) {
				if strings.HasPrefix(dependency, "k8s.io/") || strings.HasPrefix(dependency, "sigs.k8s.io/") {
					t.Fatalf("%s pulled in a Kubernetes implementation: %s", target, dependency)
				}
				for _, forbidden := range []string{"session", "session/httpapi", "session/sandboxd", "batch", "mcpserver"} {
					if dependency == "github.com/X1Kun/AgentBox/internal/"+forbidden {
						t.Fatalf("%s depends on server implementation %s", target, forbidden)
					}
				}
			}
		})
	}
}

func TestSessionBusinessDoesNotImportItsAdapters(t *testing.T) {
	output, err := exec.Command("go", "list", "-mod=readonly", "-deps", "github.com/X1Kun/AgentBox/internal/session").CombinedOutput()
	if err != nil {
		t.Fatalf("dependency inspection: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, adapter := range []string{"client", "httpapi", "sandboxd"} {
			if dependency == "github.com/X1Kun/AgentBox/internal/session/"+adapter {
				t.Fatalf("business package depends on adapter %s", adapter)
			}
		}
	}
}
