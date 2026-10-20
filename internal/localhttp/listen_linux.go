// Package localhttp provides explicitly local HTTP transports for AgentBox.
package localhttp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func ValidateLoopback(address string) error {
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 0 || number > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("listen must use an explicit loopback IP address and port")
	}
	return nil
}

func ValidateSocket(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 100 || strings.ContainsRune(path, 0) {
		return errors.New("Unix socket must be a clean absolute path of at most 100 bytes")
	}
	return nil
}

func ValidateListen(address string) error {
	if path, ok := strings.CutPrefix(address, "unix:"); ok {
		return ValidateSocket(path)
	}
	return ValidateLoopback(address)
}

func Listen(address string) (net.Listener, error) {
	if err := ValidateListen(address); err != nil {
		return nil, err
	}
	path, unix := strings.CutPrefix(address, "unix:")
	if !unix {
		return net.Listen("tcp", address)
	}
	return listenUnix(path)
}

type unixListener struct {
	*net.UnixListener
	path  string
	owned os.FileInfo
	lock  *os.File
	once  sync.Once
	err   error
}

func (l *unixListener) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if current, err := os.Lstat(l.path); err == nil && os.SameFile(l.owned, current) {
			l.err = errors.Join(l.err, os.Remove(l.path))
		}
		l.err = errors.Join(l.err, l.lock.Close())
	})
	return l.err
}

func listenUnix(path string) (_ net.Listener, retErr error) {
	parent := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if resolved != parent || !info.IsDir() || !ok || stat.Uid != uint32(os.Geteuid()) || (info.Mode().Perm() != 0700 && info.Mode().Perm() != 0750) {
		return nil, errors.New("socket parent must be owned by the daemon user, not symlinked, and mode 0700 or 0750")
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	lockInfo, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	lockStat, ok := lockInfo.Sys().(*syscall.Stat_t)
	if !lockInfo.Mode().IsRegular() || !ok || lockStat.Uid != uint32(os.Geteuid()) || lockInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("socket lock must be a private regular file owned by the daemon user")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("socket is locked: %w", err)
	}
	if current, err := os.Lstat(path); err == nil {
		owner, ok := current.Sys().(*syscall.Stat_t)
		if current.Mode()&os.ModeSocket == 0 || !ok || owner.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("refusing to replace an unowned socket or non-socket path")
		}
		connection, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, errors.New("Unix socket already has an active listener")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot confirm stale Unix socket: %w", dialErr)
		}
		latest, err := os.Lstat(path)
		if err != nil || !os.SameFile(current, latest) {
			return nil, errors.New("Unix socket changed during stale check")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	owned, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	l := &unixListener{UnixListener: listener, path: path, owned: owned, lock: lock}
	if err := os.Chmod(path, 0660); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}
