package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reproduce a runtime transaction paused between truncation and JSON write.
// Cancellation must wait for that transaction, and never start the next one.
func TestCreateCancellationDoesNotInterruptMetadataWrite(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"create", "start"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pause-phase"), []byte(phase), 0600); err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(dir, "runsc")
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestRunscMetadataHelper$ -- \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			session, err := newSession(runscCLI{binaryPath: wrapper, stateDir: dir, platform: "kvm"}, "fixture", dir, 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer session.finishStreams()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- session.create(ctx, dir) }()
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

func TestRunscMetadataHelper(t *testing.T) {
	var dir, operation string
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--root=") {
			dir = strings.TrimPrefix(arg, "--root=")
		}
		if arg == "create" || arg == "start" {
			operation = arg
		}
	}
	if dir == "" {
		return
	}
	phase, err := os.ReadFile(filepath.Join(dir, "pause-phase"))
	if err != nil {
		t.Fatal(err)
	}
	if operation == "start" {
		if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if string(phase) != operation {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"status":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "writing"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "finish")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test did not release metadata writer")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"status":"complete"}`), 0600); err != nil {
		t.Fatal(err)
	}
}
