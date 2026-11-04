package session

// Administrative cleanup codes and server observation details are not client contracts.
const (
	codeAuditFailed                    = "audit_failed"
	codeCleanupListFailed              = "cleanup_list_failed"
	codeCleanupListInvalid             = "cleanup_list_invalid"
	codeCleanupOutcomeUnknown          = "cleanup_outcome_unknown"
	codeCleanupReadFailed              = "cleanup_read_failed"
	codeCleanupRejected                = "cleanup_rejected"
	codeCleanupTimeout                 = "cleanup_timeout"
	codeInvalidCleanupConfiguration    = "invalid_cleanup_configuration"
	codeQuotaUnavailable               = "quota_unavailable"
	reasonAwaitingUpstreamCleanup      = "awaiting_upstream_cleanup"
	reasonBackingEnvironmentTerminated = "backing_environment_terminated"
	reasonBackingIdentityChanged       = "backing_identity_changed"
)
