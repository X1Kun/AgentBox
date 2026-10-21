package qos

import (
	"encoding/json"
	"io"
	"os"
	"syscall"
	"time"
)

// A trusted local collector atomically publishes a completed observation window.
// No request, model or sandbox is allowed to supply this operator-configured path.
type BusinessSample struct {
	SampledAt     time.Time `json:"sampled_at"`
	WindowMS      int       `json:"window_ms"`
	Requests      uint64    `json:"requests"`
	Rejected      uint64    `json:"rejected"`
	SLOViolations uint64    `json:"slo_violations"`
}

type BusinessReport struct {
	BusinessSample
	Available bool   `json:"available"`
	Healthy   bool   `json:"healthy"`
	Error     string `json:"error,omitempty"`
}

func readBusiness(path string, now time.Time) BusinessReport {
	bad := func(message string) BusinessReport { return BusinessReport{Error: message} }
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return bad("business sample unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4096 || stat.Mode().Perm()&0002 != 0 {
		return bad("business sample must be a bounded non-world-writable regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return bad("business sample read failed")
	}
	var wire struct {
		SampledAt     time.Time `json:"sampled_at"`
		WindowMS      int       `json:"window_ms"`
		Requests      *uint64   `json:"requests"`
		Rejected      *uint64   `json:"rejected"`
		SLOViolations *uint64   `json:"slo_violations"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Requests == nil || wire.Rejected == nil || wire.SLOViolations == nil {
		return bad("invalid business sample JSON")
	}
	sample := BusinessSample{SampledAt: wire.SampledAt, WindowMS: wire.WindowMS, Requests: *wire.Requests, Rejected: *wire.Rejected, SLOViolations: *wire.SLOViolations}
	report := BusinessReport{BusinessSample: sample}
	if !businessFresh(report, now) || sample.Rejected > sample.Requests || sample.SLOViolations > sample.Requests {
		report.Error = "invalid or stale business window"
		return report
	}
	report.Available = true
	report.Healthy = sample.Rejected == 0 && sample.SLOViolations == 0
	return report
}

func businessFresh(report BusinessReport, now time.Time) bool {
	return report.WindowMS >= 500 && report.WindowMS <= 5000 && !report.SampledAt.IsZero() &&
		!now.Before(report.SampledAt) && now.Sub(report.SampledAt) <= 2*time.Duration(report.WindowMS)*time.Millisecond
}
