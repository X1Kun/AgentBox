package api

// StatusReport is the public executor status response shared by cold and warm modes.
type StatusReport struct {
	QoS              *QoSReport `json:"qos,omitempty"`
	Mode             string     `json:"mode"`
	Ready            bool       `json:"ready"`
	Target           int        `json:"target"`
	MaxSandboxes     int        `json:"max_sandboxes"`
	Creating         int        `json:"creating"`
	Idle             int        `json:"idle"`
	Leased           int        `json:"leased"`
	Running          int        `json:"running"`
	Destroying       int        `json:"destroying"`
	Total            int        `json:"total"`
	Queued           int        `json:"queued"`
	QueuedBytes      int        `json:"queued_bytes"`
	CreationCount    uint64     `json:"creation_count"`
	CreationFailures uint64     `json:"creation_failures"`
	CleanupFailures  uint64     `json:"cleanup_failures"`
	CreationTotalMS  float64    `json:"creation_total_ms"`
}
