package api

type AuxiliaryReport struct {
	SampledAt  string           `json:"sampled_at"`
	AgeMS      float64          `json:"age_ms"`
	ReadMS     float64          `json:"read_ms"`
	IntervalMS float64          `json:"collection_interval_ms"`
	Stale      bool             `json:"stale"`
	IO         OptionalPressure `json:"io_pressure"`
	Devices    *OptionalDevices `json:"io_devices,omitempty"`
	PIDs       *OptionalPIDs    `json:"pids,omitempty"`
}

type OptionalPressure struct {
	Available  bool            `json:"available"`
	Error      string          `json:"error,omitempty"`
	Baseline   string          `json:"baseline,omitempty"`
	IntervalMS *float64        `json:"interval_ms"`
	Pressure   *PressureReport `json:"pressure"`
}

type OptionalDevices struct {
	Available bool       `json:"available"`
	Error     string     `json:"error,omitempty"`
	Devices   []DeviceIO `json:"devices"`
}

type DeviceIO struct {
	Device     string `json:"device"`
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	ReadIOs    uint64 `json:"read_ios"`
	WriteIOs   uint64 `json:"write_ios"`
}

type OptionalPIDs struct {
	Available      bool      `json:"available"`
	Error          string    `json:"error,omitempty"`
	Baseline       string    `json:"baseline,omitempty"`
	Sample         *PIDStats `json:"sample"`
	LimitHitsDelta *uint64   `json:"limit_hits_delta"`
}

type PIDStats struct {
	Current   uint64        `json:"current"`
	Max       ResourceLimit `json:"max"`
	LimitHits uint64        `json:"limit_hits"`
}
