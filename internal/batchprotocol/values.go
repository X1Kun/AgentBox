package batchprotocol

// State is the persisted and wire lifecycle vocabulary; its JSON spelling is stable.
type State string

const (
	StateSubmitting = "submitting"
	StateQueued     = "queued"
	StateRunning    = "running"
	StateSucceeded  = "succeeded"
	StateFailed     = "failed"
	StateCancelling = "cancelling"
	StateCancelled  = "cancelled"
	StateUnknown    = "unknown"
	StateRejected   = "rejected"
)

const (
	CodeArtifactExpired           = "artifact_expired"
	CodeArtifactInvalid           = "artifact_invalid"
	CodeArtifactNotReady          = "artifact_not_ready"
	CodeBackendUnavailable        = "backend_unavailable"
	CodeImageIdentityChanged      = "image_identity_changed"
	CodeInstallationChanged       = "installation_changed"
	CodeInstallationUninitialized = "installation_uninitialized"
	CodeInvalidRequest            = "invalid_request"
	CodeJobIdentityChanged        = "job_identity_changed"
	CodeNotFound                  = "not_found"
	CodePodIdentityChanged        = "pod_identity_changed"
	CodeQueueUnavailable          = "queue_unavailable"
	CodeRecordCapacity            = "record_capacity"
	CodeRecordInvalid             = "record_invalid"
	CodeRequestConflict           = "request_conflict"
	CodeResultUnconfirmed         = "result_unconfirmed"
	CodeStorageUnavailable        = "storage_unavailable"
	CodeSubmissionOutcomeUnknown  = "submission_outcome_unknown"
	CodeSubmissionRejected        = "submission_rejected"
	CodeUnexpectedAttempts        = "unexpected_attempts"
)

const (
	ReasonArtifactInvalid    = "artifact_invalid"
	ReasonExecutionFailed    = "execution_failed"
	ReasonJobMissing         = "job_missing"
	ReasonSubmissionRejected = "submission_rejected"
)
