package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type runscCLI struct {
	storage       map[string]storageMount
	rootfs        string
	sandboxParent string
	binaryPath    string
	stateDir      string // runsc metadata directory, passed as --root.
	platform      string
	logger        *slog.Logger
}

// Only operator-controlled operation names and IDs are logged. Command args,
// environment, user input and guest stdout/stderr are never logged here.
func (r runscCLI) runControl(ctx context.Context, cmd *exec.Cmd, operation, id string) (retErr error) {
	start := time.Now()
	var before *storageCounters
	if operation == "create" || operation == "delete" {
		before = &storageCounters{At: start, Devices: r.deviceCounters()}
	}
	log := r.logger
	if log != nil {
		log.Info("runtime operation started", "operation", operation, "sandbox_id", id)
	}
	defer func() {
		if log == nil {
			return
		}
		fields := []any{"operation", operation, "sandbox_id", id, "elapsed_ms", ms(time.Since(start))}
		if cmd.Process != nil {
			fields = append(fields, "pid", cmd.Process.Pid)
		}
		if retErr != nil {
			fields = append(fields, "error", retErr.Error())
			if ctx.Err() != nil {
				fields = append(fields, "context_error", ctx.Err().Error())
			}
			log.Error("runtime operation failed", fields...)
		} else {
			log.Info("runtime operation completed", fields...)
		}
	}()
	// Capture the blocked CLI and sandbox before CommandContext kills the CLI.
	cancelCommand := cmd.Cancel
	if cancelCommand != nil {
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				r.captureFailure(operation, id, cmd.Process.Pid, before)
			}
			return cancelCommand()
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if log != nil {
		log.Info("runtime process started", "operation", operation, "sandbox_id", id, "pid", cmd.Process.Pid)
	}
	return cmd.Wait()
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
	err := r.runControl(ctx, cmd, args[0], args[len(args)-1])
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
	bundlePath      string
	taskCommand     *exec.Cmd
}

func newSession(runtime runscCLI, id, bundle string, outputLimit int) (*session, error) {
	s := &session{runtime: runtime, id: id, bundlePath: bundle, outputCollector: newOutputCollector(outputLimit)}
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
	return s, nil
}

func (s *session) readOutput(reader *os.File, destination io.Writer) {
	defer s.outputReadersWG.Done()
	_, _ = io.Copy(destination, reader)
}

// outputResult returns the final output after finishStreams has completed.
func (s *session) outputResult() (stdout, stderr string, truncated bool) {
	return s.outputCollector.result()
}

func (s *session) outputLimitExceeded() <-chan struct{} {
	return s.outputCollector.limitExceeded
}

func (s *session) checkRunning(ctx context.Context) error {
	data, err := s.runtime.control(ctx, "state", s.id)
	if err != nil {
		return err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Status != "running" {
		return fmt.Errorf("sandbox state is %q", state.Status)
	}
	return nil
}

func (s *session) create(ctx context.Context, bundle string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// runsc updates metadata with truncate/write, not atomic replacement. Let
	// each started create/start operation finish when admission is cancelled;
	// killing the CLI mid-write can make even `delete --force` fail to load it.
	// The independent deadline still bounds a stalled runtime operation.
	setupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cmd := s.runtime.command(setupCtx, "create", "--bundle="+bundle, s.id)
	// Files are passed directly, so create.Wait does not wait for sandbox-held
	// stdout/stderr to close. Stream draining belongs to this session.
	cmd.Stdout, cmd.Stderr = s.writers[0], s.writers[1]
	if err := s.runtime.runControl(setupCtx, cmd, "create", s.id); err != nil {
		return fmt.Errorf("runsc create: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.runtime.control(setupCtx, "start", s.id); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *session) start(ctx context.Context) error {
	var err error
	s.stdin, err = os.Open(filepath.Join(s.bundlePath, "input", "stdin"))
	if err != nil {
		return fmt.Errorf("open task stdin: %w", err)
	}
	s.taskCommand = s.runtime.command(ctx, "exec", "--process="+filepath.Join(s.bundlePath, "process.json"), "--internal-pid-file="+filepath.Join(s.bundlePath, "task.pid"), s.id)
	s.taskCommand.Stdin, s.taskCommand.Stdout, s.taskCommand.Stderr = s.stdin, s.writers[0], s.writers[1]
	if err := s.taskCommand.Start(); err != nil {
		return err
	}
	for _, file := range s.writers {
		_ = file.Close()
	}
	_ = s.stdin.Close()
	return nil
}

func (s *session) wait(ctx context.Context) (int, error) {
	err := s.taskCommand.Wait()
	pidData, pidErr := os.ReadFile(filepath.Join(s.bundlePath, "task.pid"))
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if parseErr == nil && pid <= 0 {
		parseErr = errors.New("task PID must be positive")
	}
	if pidErr != nil || parseErr != nil || pid <= 0 {
		return 0, fmt.Errorf("runsc exec did not confirm task creation: %w", errors.Join(err, pidErr, parseErr))
	}
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || !exitError.ProcessState.Exited() {
		return 0, fmt.Errorf("runsc exec: %w", err)
	}
	exitCode := exitError.ExitCode()
	// runsc also uses 128 for control failures. A created task returning 128
	// with its sandbox still running is a user exit, not a runtime failure.
	if exitCode == 128 {
		data, stateErr := s.runtime.control(ctx, "state", s.id)
		var state struct {
			Status string `json:"status"`
		}
		if stateErr != nil || json.Unmarshal(data, &state) != nil || state.Status != "running" {
			return 0, fmt.Errorf("runsc exec lost its sandbox: %w", err)
		}
	}
	return exitCode, nil
}

func (s *session) finishStreams() {
	for _, file := range s.writers {
		_ = file.Close()
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
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
