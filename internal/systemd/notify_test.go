package systemd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotifications(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run(fmt.Sprint(abstract), func(t *testing.T) {
			address := filepath.Join(t.TempDir(), "notify.sock")
			if abstract {
				address = fmt.Sprintf("@agentbox-notify-%d-%d", os.Getpid(), time.Now().UnixNano())
			}
			listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: address, Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("NOTIFY_SOCKET", address)
			for _, test := range []struct {
				send   func() error
				prefix string
			}{{Ready, "READY=1\n"}, {Stopping, "STOPPING=1\n"}} {
				if err := test.send(); err != nil {
					t.Fatal(err)
				}
				if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				buffer := make([]byte, 512)
				n, _, err := listener.ReadFromUnix(buffer)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(string(buffer[:n]), test.prefix) {
					t.Fatalf("unexpected notification: %q", buffer[:n])
				}
			}
		})
	}
}

func TestNoNotifySocketIsOptional(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Ready(); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredSocketFailureIsReported(t *testing.T) {
	for _, address := range []string{"relative.sock", filepath.Join(t.TempDir(), "missing.sock")} {
		t.Setenv("NOTIFY_SOCKET", address)
		if err := Ready(); err == nil {
			t.Fatalf("accepted unusable notification socket %q", address)
		}
	}
}
