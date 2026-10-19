package api

type PressureStateReport struct {
	State                  string `json:"state"`
	EffectiveState         string `json:"effective_state"`
	Reason                 string `json:"reason"`
	SuggestedAdmissionOpen bool   `json:"suggested_admission_open"`
	SuggestedRefillPaused  bool   `json:"suggested_refill_paused"`
	SampleSequence         uint64 `json:"sample_sequence"`
	EvaluatedAt            string `json:"evaluated_at"`
}

type QoSSchedulingReport struct {
	Enabled            bool   `json:"enabled"`
	Enforced           bool   `json:"enforced"`
	AdmissionOpen      bool   `json:"admission_open"`
	RefillPaused       bool   `json:"refill_paused"`
	PressureRejections uint64 `json:"pressure_rejections"`
}
