package api

type DurationBucket struct {
	UpperMS *float64 `json:"upper_ms"` // null is the overflow bucket
	Count   uint64   `json:"count"`
}

type DurationReport struct {
	Count   uint64           `json:"count"`
	TotalMS float64          `json:"total_ms"`
	MaxMS   float64          `json:"max_ms"`
	Buckets []DurationBucket `json:"buckets"` // non-cumulative
}

type SchedulingReport struct {
	RefillPolicy           string         `json:"refill_policy,omitempty"`
	RefillCreationAttempts uint64         `json:"refill_creation_attempts"`
	RefillWithForeground   uint64         `json:"refill_started_with_foreground"`
	RefillBlockedBy        string         `json:"refill_blocked_by,omitempty"`
	ExecutionInFlight      int            `json:"execution_in_flight"`
	ExecutionLimit         int            `json:"execution_limit"`
	CreationLimit          int            `json:"creation_limit"`
	QueueCountLimit        int            `json:"queue_count_limit"`
	QueueBytesLimit        int            `json:"queue_bytes_limit"`
	QueueTimeoutMS         float64        `json:"queue_timeout_ms"`
	OldestQueueMS          float64        `json:"oldest_queue_ms"`
	CreationAttempts       uint64         `json:"creation_attempts"`
	CreationSucceeded      DurationReport `json:"creation_succeeded"`
	CreationFailed         DurationReport `json:"creation_failed"`
	CreationAborted        DurationReport `json:"creation_aborted"`
	QueueEntered           uint64         `json:"queue_entered"`
	QueueAssigned          DurationReport `json:"queue_assigned"`
	QueueCanceled          DurationReport `json:"queue_canceled"`
	QueueTimedOut          DurationReport `json:"queue_timed_out"`
	QueueUnavailable       DurationReport `json:"queue_unavailable"`
	QueuePressureRejected  DurationReport `json:"queue_pressure_rejected"`
	QueueCountFull         uint64         `json:"queue_count_full"`
	QueueBytesFull         uint64         `json:"queue_bytes_full"`
}

type CleanupPhase struct {
	SandboxID  string  `json:"sandbox_id"`
	Phase      string  `json:"phase"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at,omitempty"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

type CleanupReport struct {
	Active  []CleanupPhase `json:"active"`
	Recent  []CleanupPhase `json:"recent"`
	Dropped uint64         `json:"dropped"`
}

type UnavailableResponse struct {
	Error     string   `json:"error"`
	Reason    string   `json:"reason"`
	QueueMS   *float64 `json:"queue_ms,omitempty"`
	RequestID string   `json:"request_id,omitempty"`
}
