package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runscCLI struct {
	binaryPath string
	stateDir   string // runsc metadata directory, passed as --root.
	platform   string
}

func (r runscCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"--root=" + r.stateDir, "--platform=" + r.platform, "--network=none", "--gofer-network-namespace=new", "--overlay2=none", "--log-format=json"}
	cmd := exec.CommandContext(ctx, r.binaryPath, append(base, args...)...)
	// Never pass API tokens, credentials, Python settings, or a caller's PATH.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.WaitDelay = time.Second
	return cmd
}

func (r runscCLI) control(ctx context.Context, args ...string) ([]byte, error) {
	collector := newOutputCollector(64 << 10)
	cmd := r.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = collector.writer(false), collector.writer(true)
	err := cmd.Run()
	out, stderr, limitExceeded := collector.result()
	if err != nil {
		return nil, fmt.Errorf("runsc %s: %w: %s", args[0], err, strings.TrimSpace(stderr))
	}
	if limitExceeded {
		return nil, errors.New("runsc control output exceeds limit")
	}
	return []byte(out), nil
}

type session struct {
	runtime         runscCLI
	id              string
	outputCollector *outputCollector
	readers         []*os.File
	writers         []*os.File
	stdin           *os.File
	outputReadersWG sync.WaitGroup
}

func newSession(runtime runscCLI, id, bundle string, outputLimit int) (*session, error) {
	s := &session{runtime: runtime, id: id, outputCollector: newOutputCollector(outputLimit)}
	for i := 0; i < 2; i++ {
		readPipe, writePipe, err := os.Pipe()
		if err != nil {
			s.close()
			s.outputReadersWG.Wait()
			return nil, err
		}
		s.readers = append(s.readers, readPipe)
		s.writers = append(s.writers, writePipe)
		isStderr := i == 1
		destination := s.outputCollector.writer(isStderr)
		s.outputReadersWG.Add(1)
		go s.readOutput(readPipe, destination)
	}
	var err error
	s.stdin, err = os.Open(filepath.Join(bundle, "input", "stdin"))
	if err != nil {
		s.close()
		s.outputReadersWG.Wait()
		return nil, err
	}
	return s, nil
}

func (s *session) readOutput(reader *os.File, destination io.Writer) {
	defer s.outputReadersWG.Done()
	_, _ = io.Copy(destination, reader)
}

func (s *session) create(ctx context.Context, bundle string) error {
	cmd := s.runtime.command(ctx, "create", "--bundle="+bundle, s.id)
	// Files are passed directly, so create.Wait does not wait for sandbox-held
	// stdout/stderr to close. Stream draining belongs to this session.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.stdin, s.writers[0], s.writers[1]
	err := cmd.Run()
	for _, f := range s.writers {
		_ = f.Close()
	}
	_ = s.stdin.Close()
	return err
}

func (s *session) start(ctx context.Context) error {
	_, err := s.runtime.control(ctx, "start", s.id)
	return err
}

func (s *session) wait(ctx context.Context) (int, error) {
	b, err := s.runtime.control(ctx, "wait", s.id)
	if err != nil {
		return 0, err
	}
	var result struct {
		ID         string `json:"id"`
		ExitStatus *int   `json:"exitStatus"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return 0, fmt.Errorf("decode runsc wait: %w", err)
	}
	if result.ID != s.id || result.ExitStatus == nil || *result.ExitStatus < 0 || *result.ExitStatus > 255 {
		return 0, errors.New("invalid runsc wait result")
	}
	return *result.ExitStatus, nil
}

func (s *session) finishStreams() {
	readersFinished := make(chan struct{})
	go func() {
		s.outputReadersWG.Wait()
		close(readersFinished)
	}()
	select {
	case <-readersFinished:
	case <-time.After(time.Second):
	}
	s.close()
	s.outputReadersWG.Wait()
}

func (s *session) close() {
	for _, f := range s.writers {
		_ = f.Close()
	}
	for _, f := range s.readers {
		_ = f.Close()
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
}
