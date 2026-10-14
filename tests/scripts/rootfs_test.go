package scripts_test

import (
	"archive/tar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareRootfsPreservesPermissions(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "prepare-rootfs.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mask := range []string{"0022", "0077"} {
		t.Run(mask, func(t *testing.T) {
			dir := t.TempDir()
			projectDir := filepath.Join(dir, "project")
			scriptPath := filepath.Join(projectDir, "scripts", "prepare-rootfs.sh")
			if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(scriptPath, script, 0600); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(dir, "image.tar")
			writeRootfsArchive(t, archive)
			toolDir := filepath.Join(dir, "tools")
			if err := os.Mkdir(toolDir, 0755); err != nil {
				t.Fatal(err)
			}
			// Mock only image transport; use the real preparation script and tar
			// so this regression runs without Docker, root, or a network.
			docker := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$AGENTBOX_ROOTFS_TEST_LOG"
case "$1" in
    pull) ;;
    image) printf '%s\n' '["python@sha256:0000000000000000000000000000000000000000000000000000000000000000"]' ;;
    create) printf '%s\n' 'rootfs-fixture' ;;
    export) cp "$AGENTBOX_ROOTFS_TEST_ARCHIVE" "$4" ;;
    container) test "$2" = rm; test "$3" = rootfs-fixture ;;
    *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(toolDir, "docker"), []byte(docker), 0755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "docker.log")
			cmd := exec.Command("bash", "-c", `umask "$1"; exec bash "$2"`, "rootfs-permissions-test", mask, scriptPath)
			cmd.Env = append(os.Environ(), "PATH="+toolDir+":"+os.Getenv("PATH"), "AGENTBOX_ROOTFS_TEST_ARCHIVE="+archive, "AGENTBOX_ROOTFS_TEST_LOG="+logPath)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare rootfs under umask %s: %v\n%s", mask, err, output)
			}
			rootfs := filepath.Join(projectDir, ".local", "rootfs")
			for path, mode := range map[string]os.FileMode{
				".": 0755, "usr": 0755, "usr/local": 0755, "usr/local/bin": 0755,
				"usr/local/bin/python3.12": 0755, "usr/local/lib": 0755,
				"usr/local/lib/libfixture.so": 0644, "etc/private": 0600,
				"tmp": 01777, "agentbox": 0755, "workspace": 0755,
			} {
				info, err := os.Stat(filepath.Join(rootfs, path))
				if err != nil {
					t.Fatal(err)
				}
				got := uint32(info.Mode().Perm())
				if info.Mode()&os.ModeSticky != 0 {
					got |= 01000
				}
				if got != uint32(mode) {
					t.Errorf("%s: mode %04o, want %04o", path, got, mode)
				}
			}
			if link, err := os.Readlink(filepath.Join(rootfs, "usr/local/bin/python3")); err != nil || link != "python3.12" {
				t.Fatalf("Python symlink: %q %v", link, err)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(calls), "container rm rootfs-fixture\n") {
				t.Fatalf("export container was not cleaned up: %q %v", calls, err)
			}
		})
	}
}

func writeRootfsArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := tar.NewWriter(f)
	for _, entry := range []struct {
		name string
		mode int64
		kind byte
		data string
		link string
	}{
		{".", 0700, tar.TypeDir, "", ""},
		{"usr", 0755, tar.TypeDir, "", ""},
		{"usr/local", 0755, tar.TypeDir, "", ""},
		{"usr/local/bin", 0755, tar.TypeDir, "", ""},
		{"usr/local/bin/python3.12", 0755, tar.TypeReg, "#!/bin/sh\nexit 0\n", ""},
		{"usr/local/bin/python3", 0777, tar.TypeSymlink, "", "python3.12"},
		{"usr/local/lib", 0755, tar.TypeDir, "", ""},
		{"usr/local/lib/libfixture.so", 0644, tar.TypeReg, "fixture", ""},
		{"etc", 0755, tar.TypeDir, "", ""},
		{"etc/private", 0600, tar.TypeReg, "private", ""},
		{"tmp", 01777, tar.TypeDir, "", ""},
	} {
		h := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Size: int64(len(entry.data)), Linkname: entry.link, Uid: 2000, Gid: 2000}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
