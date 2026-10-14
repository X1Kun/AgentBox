package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/X1Kun/AgentBox/internal/api"
)

func TestPrepareInputPermissions(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []string{"0022", "0077", "0777"} {
		t.Run(mask, func(t *testing.T) {
			bundle := t.TempDir()
			if err := os.Chmod(bundle, 0700); err != nil {
				t.Fatal(err)
			}
			// umask is process-wide: exercise it in a subprocess so concurrent
			// tests and the race detector never observe a changed parent umask.
			cmd := exec.Command(executable, "-test.run=^TestPrepareInputUmaskHelper$")
			cmd.Env = append(os.Environ(), "AGENTBOX_INPUT_TEST_DIR="+bundle, "AGENTBOX_INPUT_TEST_UMASK="+mask)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare input under umask %s: %v\n%s", mask, err, output)
			}
			for path, mode := range map[string]os.FileMode{
				bundle:                         0700,
				filepath.Join(bundle, "input"): 0755,
				filepath.Join(bundle, "input", "task.json"): 0444,
				filepath.Join(bundle, "input", "stdin"):     0600,
			} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s: mode %04o, want %04o", path, info.Mode().Perm(), mode)
				}
			}
			data, err := os.ReadFile(filepath.Join(bundle, "input", "task.json"))
			if err != nil {
				t.Fatal(err)
			}
			var request api.Request
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			if request.Code != "print('permission-test')" {
				t.Fatalf("unexpected task: %+v", request)
			}
			data, err = os.ReadFile(filepath.Join(bundle, "input", "stdin"))
			if err != nil || string(data) != "input-data\n" {
				t.Fatalf("stdin: %q %v", data, err)
			}
		})
	}
}

func TestPrepareInputUmaskHelper(t *testing.T) {
	bundle := os.Getenv("AGENTBOX_INPUT_TEST_DIR")
	if bundle == "" {
		return
	}
	mask, err := strconv.ParseUint(os.Getenv("AGENTBOX_INPUT_TEST_UMASK"), 8, 9)
	if err != nil {
		t.Fatal(err)
	}
	previous := syscall.Umask(int(mask))
	defer syscall.Umask(previous)
	if err := prepareInput(bundle, api.Request{Runtime: "python", Code: "print('permission-test')", Stdin: "input-data\n"}); err != nil {
		t.Fatal(err)
	}
}
