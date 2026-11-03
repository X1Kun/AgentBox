package batch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Store struct {
	root    *os.Root
	metrics *telemetry.Metrics
}

var stagingRecord = regexp.MustCompile(`^(bat-[a-f0-9]{40})\.json\.[a-f0-9]{32}\.tmp$`)

func NewStore(path string) (*Store, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{"records", "locks", "attempts", "published"} {
		if err = r.MkdirAll(p, 0770); err != nil {
			r.Close()
			return nil, err
		}
	}
	return &Store{root: r}, nil
}
func (s *Store) Close() error { return s.root.Close() }
func (s *Store) lock(ctx context.Context, name string) (func(), error) {
	if !dns.MatchString(name) {
		return nil, fail("invalid_request")
	}
	f, err := s.root.OpenFile("locks/"+name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func boundedFile(root *os.Root, name string, max int64) ([]byte, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, fail("artifact_invalid")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fail("artifact_invalid")
	}
	return data, nil
}
func (s *Store) read(name string) (*Record, error) {
	data, err := boundedFile(s.root, "records/"+name+".json", 64<<10)
	if err != nil {
		return nil, err
	}
	var r Record
	if json.Unmarshal(data, &r) != nil || r.Version != 1 || r.Name != name || !strings.HasPrefix(name, "bat-") {
		return nil, fail("record_invalid")
	}
	return &r, nil
}
func atomicWrite(root *os.Root, name string, data []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := name + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = root.Rename(temp, name); err != nil {
		return err
	}
	d, err := root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *Store) write(r *Record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	old, readErr := boundedFile(s.root, "records/"+r.Name+".json", 64<<10)
	if readErr == nil && bytes.Equal(old, data) {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err = atomicWrite(s.root, "records/"+r.Name+".json", data); err != nil {
		return err
	}
	if s.metrics != nil {
		from := "none"
		if readErr == nil {
			var previous Record
			if json.Unmarshal(old, &previous) == nil {
				from = previous.State
			}
		}
		s.metrics.Transition(from, r.State, r.Name+"."+r.JobUID, time.Now())
	}
	return nil
}
func (s *Store) names() ([]string, error) {
	d, err := s.root.Open("records")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var names []string
	for {
		entries, readErr := d.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, e := range entries {
			if staged := stagingRecord.FindStringSubmatch(e.Name()); staged != nil {
				// An atomic writer may be between fsync and rename. Never treat its
				// temporary as a task or remove it while that record is locked.
				lock, lockErr := s.root.OpenFile("locks/"+staged[1], os.O_CREATE|os.O_RDWR, 0600)
				if lockErr != nil {
					return nil, lockErr
				}
				if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
					_ = s.root.Remove("records/" + e.Name())
					_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				}
				lock.Close()
				continue
			}
			n := strings.TrimSuffix(e.Name(), ".json")
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") && dns.MatchString(n) && strings.HasPrefix(n, "bat-") {
				names = append(names, n)
				if len(names) > 1024 {
					return nil, fail("record_capacity")
				}
			} else {
				return nil, fail("record_invalid")
			}
		}
		if errors.Is(readErr, io.EOF) {
			return names, nil
		}
	}
}
