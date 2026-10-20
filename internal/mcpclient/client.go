// Package mcpclient starts the trusted local stdio adapter with minimal credentials.
package mcpclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Command(path, gatewayURL, token string) (*exec.Cmd, error) {
	check, err := gatewayclient.New(gatewayURL, token)
	if err != nil {
		return nil, err
	}
	check.Close()
	if path == "" {
		return nil, errors.New("MCP command must be configured")
	}
	command := exec.Command(path, "-gateway-url", gatewayURL)
	// Deliberately do not inherit DEEPSEEK_API_KEY, daemon credentials, or the
	// user's environment. The child needs only the public Gateway credential.
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "AGENTBOX_GATEWAY_TOKEN=" + token}
	command.Stderr = os.Stderr
	return command, nil
}

func Connect(ctx context.Context, path, gatewayURL, token string) (*mcp.ClientSession, error) {
	command, err := Command(path, gatewayURL, token)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentbox-agent", Version: "0.1.0"}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
}
