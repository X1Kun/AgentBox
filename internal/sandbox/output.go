package sandbox

import (
	"bytes"
	"io"
	"sync"
)

// outputCollector shares one byte budget across stdout and stderr. Writers always
// consume all input so a noisy child cannot wedge the output reader.
type outputCollector struct {
	mu             sync.Mutex
	remainingBytes int
	stdout, stderr bytes.Buffer
	limitExceeded  chan struct{}
	once           sync.Once
}

type outputWriter struct {
	collector *outputCollector
	isStderr  bool
}

func newOutputCollector(limit int) *outputCollector {
	return &outputCollector{remainingBytes: limit, limitExceeded: make(chan struct{})}
}

func (c *outputCollector) writer(isStderr bool) io.Writer {
	return outputWriter{collector: c, isStderr: isStderr}
}

func (w outputWriter) Write(p []byte) (int, error) {
	w.collector.mu.Lock()
	defer w.collector.mu.Unlock()
	n := min(len(p), w.collector.remainingBytes)
	if w.isStderr {
		_, _ = w.collector.stderr.Write(p[:n])
	} else {
		_, _ = w.collector.stdout.Write(p[:n])
	}
	w.collector.remainingBytes -= n
	if n < len(p) {
		w.collector.once.Do(func() { close(w.collector.limitExceeded) })
	}
	return len(p), nil
}

func (c *outputCollector) result() (string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.limitExceeded:
		return c.stdout.String(), c.stderr.String(), true
	default:
		return c.stdout.String(), c.stderr.String(), false
	}
}
