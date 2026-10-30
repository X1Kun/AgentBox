package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reproduce a runtime transaction paused between truncation and JSON write.
// Cancellation must wait for that transaction, and never start the next one.
func TestCreateCancellationDoesNotInterruptMetadataWrite(t *testing.T) {
	for _, phase := range []string{"create", "start"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pause-phase"), []byte(phase), 0600); err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(dir, "runsc")
			// A small shell fixture avoids cold-starting this entire race-enabled
			// Go test binary for every simulated runsc control operation.
			script := `#!/bin/sh
set -eu
root=
operation=
for arg in "$@"; do
  case "$arg" in
    --root=*) root=${arg#--root=} ;;
    create|start) operation=$arg ;;
  esac
done
if [ "$operation" = start ]; then : > "$root/started"; fi
if [ "$(cat "$root/pause-phase")" != "$operation" ]; then exit 0; fi
printf '%s' '{"status":' > "$root/state.json"
: > "$root/writing"
while [ ! -e "$root/finish" ]; do sleep 0.01; done
printf '%s' '{"status":"complete"}' > "$root/state.json"
`

			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			session, err := newSession(runscCLI{binaryPath: wrapper, stateDir: dir, platform: "kvm"}, "fixture", dir, 1024)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				done <- session.create(ctx, dir)
				close(finished)
			}()
			t.Cleanup(func() {
				cancel()
				// Release the fixture even when an assertion called Fatal. Join
				// create before closing files it may still pass to Cmd.Start.
				_ = os.WriteFile(filepath.Join(dir, "finish"), nil, 0600)
				<-finished // create retains its production 10s setup deadline.
				session.finishStreams()
			})
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "writing")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("helper did not begin metadata write")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				t.Fatalf("cancel interrupted metadata transaction: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if err := os.WriteFile(filepath.Join(dir, "finish"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was lost: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("transaction did not finish")
			}
			data, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil || !json.Valid(data) {
				t.Fatalf("incomplete metadata retained: %q %v", data, err)
			}
			if phase == "create" {
				if _, err := os.Stat(filepath.Join(dir, "started")); !os.IsNotExist(err) {
					t.Fatal("cancelled creation proceeded to start")
				}
			}
		})
	}
}
