package protocol

// State is the persisted and wire lifecycle vocabulary; its JSON spelling is stable.
type State string

const (
	StateCreating    = "creating"
	StateReady       = "ready"
	StateClosing     = "closing"
	StateClosed      = "closed"
	StateInvalidated = "invalidated"
	StateUnavailable = "unavailable"
	StateUnknown     = "unknown"
)

const (
	CodeAdmissionRejected       = "admission_rejected"
	CodeBackendUnavailable      = "backend_unavailable"
	CodeExecutionOutputLimit    = "execution_output_limit"
	CodeExecutionTimeout        = "execution_timeout"
	CodeExecutionUnavailable    = "execution_unavailable"
	CodeIdentityChanged         = "identity_changed"
	CodeIntegrityError          = "integrity_error"
	CodeInvalidRequest          = "invalid_request"
	CodeNotFound                = "not_found"
	CodeOperationOutcomeUnknown = "operation_outcome_unknown"
	CodeRequestConflict         = "request_conflict"
	CodeSessionBusy             = "session_busy"
	CodeSessionNotReady         = "session_not_ready"
	CodeStateConflict           = "state_conflict"
)
