// Package systemd implements the small notification protocol used by the unit.
package systemd

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func Ready() error {
	return notify("READY=1\nSTATUS=Accepting sandbox executions")
}

func Stopping() error {
	return notify("STOPPING=1\nSTATUS=Waiting for sandbox cleanup")
}

func notify(message string) error {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return nil // Direct CLI execution does not require systemd.
	}
	if !strings.HasPrefix(address, "/") && !strings.HasPrefix(address, "@") {
		return fmt.Errorf("NOTIFY_SOCKET must be an absolute or abstract Unix socket")
	}
	connection, err := net.DialTimeout("unixgram", address, time.Second)
	if err != nil {
		return fmt.Errorf("connect systemd notification socket: %w", err)
	}
	defer connection.Close()
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := connection.Write([]byte(message)); err != nil {
		return fmt.Errorf("send systemd notification: %w", err)
	}
	return nil
}
