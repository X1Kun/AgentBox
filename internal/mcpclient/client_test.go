package mcpclient

import (
	"strings"
	"testing"
)

func TestSubprocessReceivesOnlyAPICredential(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "must-not-reach-MCP")
	t.Setenv("KUBECONFIG", "must-not-reach-MCP")
	command, err := Command("/trusted/agentbox-mcp", "https://127.0.0.1:8090", "api-token-with-at-least-32-visible-characters", "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Env, "\n")
	if strings.Contains(joined, "must-not-reach") || !strings.Contains(joined, "AGENTBOX_API_TOKEN=") || len(command.Env) != 3 {
		t.Fatal("credential boundary violated")
	}
	if strings.Contains(strings.Join(command.Args, " "), "gateway") {
		t.Fatal("legacy routing remains")
	}
}
