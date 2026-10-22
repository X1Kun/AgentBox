package api

// StatusReport is the public executor status response shared by cold and warm modes.
type StatusReport struct {
	ParentCPUMilli   int64               `json:"parent_cpu_milli,omitempty"`
	SandboxCPUMilli  int64               `json:"sandbox_cpu_milli"`
	Memory           *MemoryBudgetReport `json:"memory,omitempty"`
	Inventory        *InventoryReport    `json:"inventory,omitempty"`
	Scheduling       *SchedulingReport   `json:"scheduling,omitempty"`
	Cleanup          *CleanupReport      `json:"cleanup,omitempty"`
	QoS              *QoSReport          `json:"qos,omitempty"`
	Mode             string              `json:"mode"`
	Ready            bool                `json:"ready"`
	Target           int                 `json:"target"`
	MaxSandboxes     int                 `json:"max_sandboxes"`
	Creating         int                 `json:"creating"`
	Idle             int                 `json:"idle"`
	Leased           int                 `json:"leased"`
	Running          int                 `json:"running"`
	Destroying       int                 `json:"destroying"`
	Total            int                 `json:"total"`
	Queued           int                 `json:"queued"`
	QueuedBytes      int                 `json:"queued_bytes"`
	CreationCount    uint64              `json:"creation_count"`
	CreationFailures uint64              `json:"creation_failures"`
	CleanupFailures  uint64              `json:"cleanup_failures"`
	CreationTotalMS  float64             `json:"creation_total_ms"`
}

// InventoryReport describes opt-in READY inventory control, not execution concurrency.
type InventoryReport struct {
	Enabled               bool    `json:"enabled"`
	Target                int     `json:"target"`
	RawTarget             int     `json:"raw_target"`
	StabilizedTarget      int     `json:"stabilized_target"`
	MinTarget             int     `json:"min_target"`
	MaxTarget             int     `json:"max_target"`
	AdjustIntervalMS      float64 `json:"adjust_interval_ms"`
	ObservationIntervalMS float64 `json:"observation_interval_ms"`
	DownscaleWindowMS     float64 `json:"downscale_window_ms"`
	Reason                string  `json:"reason"`
	LimitedBy             string  `json:"limited_by,omitempty"`
	HeadWaitReason        string  `json:"head_wait_reason,omitempty"`
	InventoryStalls       uint64  `json:"inventory_stalled_requests"`
	SlotWaitRequests      uint64  `json:"slot_wait_requests"`
	BothWaitRequests      uint64  `json:"both_wait_requests"`
	LastChangedAt         string  `json:"last_changed_at"`
	ScaleUps              uint64  `json:"scale_ups"`
	ScaleDowns            uint64  `json:"scale_downs"`
	ShrinkAttempts        uint64  `json:"shrink_attempts"`
	ShrinkSucceeded       uint64  `json:"shrink_succeeded"`
	ShrinkFailures        uint64  `json:"shrink_failures"`
}

// MemoryBudgetReport exposes the resolved deployment ceiling, not free memory.
type MemoryBudgetReport struct {
	ParentBytes   int64 `json:"parent_bytes"`
	SandboxBytes  int64 `json:"sandbox_bytes"`
	ReserveBytes  int64 `json:"reserve_bytes"`
	CapacityBytes int64 `json:"capacity_bytes"`
}
