package api

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	MaxCodeBytes     = 256 << 10
	MaxInputBytes    = 256 << 10
	MaxBodyBytes     = 2 << 20
	DefaultTimeoutMS = 5000
	MaxTimeoutMS     = 30000
)

var envKey = regexp.MustCompile(`^AGENT_[A-Z0-9_]{1,58}$`)

type Request struct {
	RequestID string            `json:"request_id,omitempty"`
	Runtime   string            `json:"runtime"`
	Code      string            `json:"code"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Validate returns a normalized copy; resource settings are never client supplied.
func (r Request) Validate() (Request, error) {
	if r.Runtime != "python" {
		return r, fmt.Errorf("runtime must be python")
	}
	if strings.TrimSpace(r.Code) == "" || len(r.Code) > MaxCodeBytes {
		return r, fmt.Errorf("code must contain 1 to %d bytes", MaxCodeBytes)
	}
	if len(r.Stdin) > MaxInputBytes {
		return r, fmt.Errorf("stdin exceeds %d bytes", MaxInputBytes)
	}
	if len(r.RequestID) > 128 {
		return r, fmt.Errorf("request_id exceeds 128 bytes")
	}
	if r.TimeoutMS == 0 {
		r.TimeoutMS = DefaultTimeoutMS
	}
	if r.TimeoutMS < 1 || r.TimeoutMS > MaxTimeoutMS {
		return r, fmt.Errorf("timeout_ms must be between 1 and %d", MaxTimeoutMS)
	}
	if len(r.Env) > 16 {
		return r, fmt.Errorf("at most 16 environment variables are allowed")
	}
	for k, v := range r.Env {
		if !envKey.MatchString(k) || strings.ContainsRune(v, 0) || len(v) > 4096 {
			return r, fmt.Errorf("environment keys must start with AGENT_ and values must contain at most 4096 bytes without NUL")
		}
	}
	return r, nil
}

type Timings struct {
	QueueMS     float64 `json:"queue_ms"`
	SetupMS     float64 `json:"setup_ms"`
	ExecutionMS float64 `json:"execution_ms"`
	CleanupMS   float64 `json:"cleanup_ms"`
	TotalMS     float64 `json:"total_ms"`
}

type Usage struct {
	CPUUsec         uint64 `json:"cpu_usec"`
	MemoryPeakBytes uint64 `json:"memory_peak_bytes"`
	OOMKills        uint64 `json:"oom_kills"`
}

type Result struct {
	ExecutionID     string  `json:"execution_id"`
	RequestID       string  `json:"request_id,omitempty"`
	Status          string  `json:"status"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	Stdout          string  `json:"stdout"`
	Stderr          string  `json:"stderr"`
	OutputTruncated bool    `json:"output_truncated"`
	Timings         Timings `json:"timings"`
	Usage           Usage   `json:"usage"`
	CleanupComplete bool    `json:"cleanup_complete"`
}
