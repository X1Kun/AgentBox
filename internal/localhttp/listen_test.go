package localhttp

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUnixOwnershipAndLifecycle(t *testing.T) {
	dir := privateDirectory(t)
	path := filepath.Join(dir, "daemon.sock")
	l, err := Listen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0660 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	if _, err := Listen("unix:" + path); err == nil {
		t.Fatal("active socket replaced")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket not removed", err)
	}
	// Emulate a crashed listener that left its socket inode behind.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	recovered, err := Listen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnixListenerPreservesOtherFiles(t *testing.T) {
	dir := privateDirectory(t)
	path := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen("unix:" + path); err == nil {
		t.Fatal("regular file replaced")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep" {
		t.Fatal("sentinel changed")
	}
	_ = os.Remove(path)
	if err := os.Symlink(filepath.Join(dir, "sentinel"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen("unix:" + path); err == nil {
		t.Fatal("symlink replaced")
	}
	_ = os.Remove(path)
	l, err := Listen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	// An endpoint replaced by an administrator is not owned by this listener.
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	data, _ = os.ReadFile(path)
	if string(data) != "replacement" {
		t.Fatal("close removed a replacement")
	}
}

func TestUnixParentAndAddressValidation(t *testing.T) {
	for _, address := range []string{"0.0.0.0:8080", "localhost:8080", "127.0.0.1:no", "unix:relative", "unix:/tmp/../socket"} {
		if err := ValidateListen(address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	_ = os.Symlink(private, alias)
	if _, err := Listen("unix:" + filepath.Join(alias, "daemon.sock")); err == nil {
		t.Fatal("symlinked directory accepted")
	}
	_ = os.Chmod(private, 0770)
	if _, err := Listen("unix:" + filepath.Join(private, "daemon.sock")); err == nil {
		t.Fatal("group-writable directory accepted")
	}
}
