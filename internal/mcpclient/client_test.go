package mcpclient

import (
	"strings"
	"testing"
)

func TestSubprocessGetsOnlyGatewayCredential(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "must-not-reach-MCP")
	t.Setenv("AGENTBOX_DAEMON_TOKEN", "must-not-reach-MCP-either")
	command, err := Command("/operator/path/agentbox-mcp", "http://127.0.0.1:8080", "public-gateway-token-with-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.Env, "\n")
	if strings.Contains(joined, "must-not-reach") || !strings.Contains(joined, "AGENTBOX_GATEWAY_TOKEN=") || len(command.Env) != 3 {
		t.Fatal("child credential boundary broken")
	}
}
