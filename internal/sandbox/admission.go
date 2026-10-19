package sandbox

import "time"

// AdmissionError preserves errors.Is(ErrBusy/ErrUnhealthy) while explaining
// why admission failed. QueueWait is valid only after actual queue entry.
type AdmissionError struct {
	Reason    string
	Cause     error
	Queued    bool
	QueueWait time.Duration
}

func (e *AdmissionError) Error() string { return e.Reason + ": " + e.Cause.Error() }
func (e *AdmissionError) Unwrap() error { return e.Cause }

func admissionError(reason string, cause error) *AdmissionError {
	return &AdmissionError{Reason: reason, Cause: cause}
}
