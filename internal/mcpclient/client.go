// Package mcpclient starts the trusted local stdio adapter with minimal credentials.
package mcpclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/X1Kun/AgentBox/internal/sessionclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Command(path, apiURL, token, caFile string) (*exec.Cmd, error) {
	check, err := sessionclient.New(apiURL, token, caFile)
	if err != nil {
		return nil, err
	}
	check.Close()
	if path == "" {
		return nil, errors.New("MCP command must be configured")
	}
	command := exec.Command(path, "-api-url", apiURL, "-api-ca-file", caFile)
	// Deliberately do not inherit DEEPSEEK_API_KEY, cluster credentials, or the
	// user's environment. The child needs only the session API credential.
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "AGENTBOX_API_TOKEN=" + token}
	command.Stderr = os.Stderr
	return command, nil
}

func Connect(ctx context.Context, path, apiURL, token, caFile string) (*mcp.ClientSession, error) {
	command, err := Command(path, apiURL, token, caFile)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentbox-agent", Version: "2.0.0"}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
}
