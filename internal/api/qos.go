package api

// QoSReport is observation-only. Missing/invalid rates remain null; they must
// not be interpreted as zero pressure. Observation failures do not stop tasks.
type QoSReport struct {
	Mode         string                `json:"mode"`
	State        string                `json:"state"`
	IntervalMS   float64               `json:"interval_ms"`
	CollectedAt  string                `json:"collected_at,omitempty"`
	AgeMS        float64               `json:"age_ms"`
	CollectionMS float64               `json:"collection_ms"`
	Stale        bool                  `json:"stale"`
	Sources      []ResourceObservation `json:"sources"`
}
type ResourceObservation struct {
	Name       string          `json:"name"`
	Scope      string          `json:"scope"`
	Path       string          `json:"path"`
	Available  bool            `json:"available"`
	Error      string          `json:"error,omitempty"`
	Baseline   string          `json:"baseline,omitempty"`
	SampledAt  string          `json:"sampled_at,omitempty"`
	ReadMS     float64         `json:"read_ms"`
	IntervalMS *float64        `json:"interval_ms"`
	Sample     *ResourceSample `json:"sample"`
}
type ResourceSample struct {
	CPU    PressureReport     `json:"cpu_pressure"`
	Memory PressureReport     `json:"memory_pressure"`
	Cgroup *CgroupObservation `json:"cgroup,omitempty"`
}
type PressureReport struct {
	Some PressureValue  `json:"some"`
	Full *PressureValue `json:"full"`
}
type PressureValue struct {
	Avg10        float64  `json:"avg10"`
	Avg60        float64  `json:"avg60"`
	Avg300       float64  `json:"avg300"`
	TotalUsec    uint64   `json:"total_usec"`
	StallPercent *float64 `json:"stall_percent"`
}
type ResourceLimit struct {
	Unlimited bool   `json:"unlimited"`
	Value     uint64 `json:"value"`
}
type CgroupObservation struct {
	CPUUsageUsec        uint64        `json:"cpu_usage_usec"`
	CPUPeriods          uint64        `json:"cpu_periods"`
	CPUThrottledPeriods uint64        `json:"cpu_throttled_periods"`
	CPUThrottledUsec    uint64        `json:"cpu_throttled_usec"`
	CPUQuota            ResourceLimit `json:"cpu_quota_usec"`
	CPUPeriodUsec       uint64        `json:"cpu_period_usec"`
	MemoryCurrentBytes  uint64        `json:"memory_current_bytes"`
	MemoryHigh          ResourceLimit `json:"memory_high_bytes"`
	MemoryMax           ResourceLimit `json:"memory_max_bytes"`
	MemoryHighEvents    uint64        `json:"memory_high_events"`
	MemoryMaxEvents     uint64        `json:"memory_max_events"`
	OOMEvents           uint64        `json:"oom_events"`
	OOMKills            uint64        `json:"oom_kills"`
	Delta               *CgroupDelta  `json:"delta"`
}
type CgroupDelta struct {
	CPUUsageCores       float64 `json:"cpu_usage_cores"`
	CPUPeriods          uint64  `json:"cpu_periods"`
	CPUThrottledPeriods uint64  `json:"cpu_throttled_periods"`
	CPUThrottledUsec    uint64  `json:"cpu_throttled_usec"`
	MemoryHighEvents    uint64  `json:"memory_high_events"`
	MemoryMaxEvents     uint64  `json:"memory_max_events"`
	OOMEvents           uint64  `json:"oom_events"`
	OOMKills            uint64  `json:"oom_kills"`
}
