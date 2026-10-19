package api

type CPUControlReport struct {
	State                string   `json:"state"`
	Reason               string   `json:"reason"`
	Action               string   `json:"action"`
	SampleSequence       uint64   `json:"sample_sequence"`
	ActualCPU            *float64 `json:"actual_cpu"`
	SuggestedCPU         float64  `json:"suggested_cpu"`
	Applied              bool     `json:"applied"`
	Decisions            uint64   `json:"decisions"`
	Changes              uint64   `json:"changes"`
	WriteFailures        uint64   `json:"write_failures"`
	Error                string   `json:"error,omitempty"`
	EvaluatedAt          string   `json:"evaluated_at"`
	MinCPU               float64  `json:"min_cpu"`
	MaxCPU               float64  `json:"max_cpu"`
	LowPressure          float64  `json:"low_pressure"`
	HighPressure         float64  `json:"high_pressure"`
	DecisionIntervalMS   float64  `json:"decision_interval_ms"`
	StableForMS          float64  `json:"stable_for_ms"`
	DecreaseFactor       float64  `json:"decrease_factor"`
	IncreaseCPUPerSecond float64  `json:"increase_cpu_per_second"`
}
