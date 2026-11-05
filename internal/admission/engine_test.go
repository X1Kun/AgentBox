package admission

import (
	"errors"
	"github.com/X1Kun/AgentBox/internal/admission/health"
	"testing"
	"time"
)

func policy() Config {
	return Config{Version: 1, Namespace: "work", LocalQueue: "batch", ClusterQueue: "batch-worker", Check: "health", Owner: "test-owner", Health: health.Contract{Domain: "worker", Revision: "v1", Profile: "short-followup", WindowSeconds: 2, DeadlineSeconds: 1}, PrometheusURL: "http://127.0.0.1:19090", MinimumSamples: 2, HealthyRatio: .95, ProtectRatio: .8, MaxAgeSeconds: 10, StableSeconds: 4, ProtectSeconds: 2, PermitIntervalSeconds: 3, MaxWaitSeconds: 30, RetrySeconds: 5, PollSeconds: 1, RequestTimeoutSeconds: 2}
}
func sample(at time.Time, sequence int64, ratio int64) health.Sample {
	return health.Sample{Contract: policy().Health, End: at, SourceStarted: time.Unix(1000, 0), Sequence: sequence, Planned: 100, Timely: ratio, Errors: 100 - ratio, Valid: true}
}
func TestHealthDwellNeedsNewTimeOrderedEvidence(t *testing.T) {
	cfg := policy()
	e := NewEngine(cfg)
	start := time.Unix(2000, 0)
	s := sample(start, 1, 100)
	if d := e.Evaluate(start, s, nil); d.State != HealthyPending || d.Permit {
		t.Fatal(d)
	}
	if d := e.Evaluate(start.Add(5*time.Second), s, nil); d.Permit {
		t.Fatal("repeated sample completed dwell", d)
	}
	for i, offset := range []int{1, 4} {
		s = sample(start.Add(seconds(offset)), int64(i+2), 100)
		d := e.Evaluate(s.End, s, nil)
		if d.Permit != (offset == 4) {
			t.Fatal(d)
		}
	}
	bad := sample(start.Add(5*time.Second), 4, 50)
	if d := e.Evaluate(bad.End, bad, nil); d.Permit {
		t.Fatal(d)
	}
	if d := e.Evaluate(bad.End.Add(time.Second), bad, nil); d.Permit {
		t.Fatal("repeated low sample released permission", d)
	}
	bad = sample(start.Add(7*time.Second), 5, 50)
	if d := e.Evaluate(bad.End, bad, nil); d.State != Protect || d.Permit {
		t.Fatal(d)
	}
	for i, offset := range []int{8, 12} {
		s = sample(start.Add(seconds(offset)), int64(i+6), 100)
		d := e.Evaluate(s.End, s, nil)
		if d.Permit != (offset == 12) {
			t.Fatal(d)
		}
	}
}
func TestInvalidSamplesAndSourceFailuresNeverPermit(t *testing.T) {
	at := time.Unix(2000, 0)
	cases := map[string]func(*health.Sample){
		"empty":    func(s *health.Sample) { s.Planned = 0; s.Timely = 0; s.Errors = 0 },
		"low":      func(s *health.Sample) { s.Planned = 1; s.Timely = 1; s.Errors = 0 },
		"future":   func(s *health.Sample) { s.End = at.Add(time.Second) },
		"stale":    func(s *health.Sample) { s.End = at.Add(-20 * time.Second) },
		"invalid":  func(s *health.Sample) { s.Valid = false },
		"counts":   func(s *health.Sample) { s.Timeouts = 1 },
		"contract": func(s *health.Sample) { s.Revision = "v2" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := NewEngine(policy())
			s := sample(at, 1, 100)
			mutate(&s)
			if d := e.Evaluate(at, s, nil); d.State != Unknown || d.Permit {
				t.Fatal(d)
			}
		})
	}
	if d := NewEngine(policy()).Evaluate(at, health.Sample{}, errors.New("private URL")); d.State != Degraded || d.Reason != "source_unavailable" {
		t.Fatal(d)
	}
}
func TestSourceResetRegressionAndMutableSequenceRestartDwell(t *testing.T) {
	at := time.Unix(2000, 0)
	e := NewEngine(policy())
	s := sample(at, 1, 100)
	e.Evaluate(at, s, nil)
	s = sample(at.Add(5*time.Second), 2, 100)
	if !e.Evaluate(s.End, s, nil).Permit {
		t.Fatal("healthy expected")
	}
	s.Timely = 99
	s.Errors = 1
	if d := e.Evaluate(s.End, s, nil); d.Permit || d.Reason != "sample_changed" {
		t.Fatal(d)
	}
	s = sample(at.Add(6*time.Second), 1, 100)
	s.SourceStarted = at.Add(time.Second)
	if d := e.Evaluate(s.End, s, nil); d.Permit || d.State != HealthyPending {
		t.Fatal(d)
	}
	s = sample(at.Add(7*time.Second), 3, 100)
	if d := e.Evaluate(s.End, s, nil); d.Permit || d.Reason != "source_regressed" {
		t.Fatal(d)
	}
}
func TestHysteresisCannotEstablishHealth(t *testing.T) {
	at := time.Unix(2000, 0)
	e := NewEngine(policy())
	if d := e.Evaluate(at, sample(at, 1, 90), nil); d.Permit {
		t.Fatal(d)
	}
	e.Evaluate(at.Add(time.Second), sample(at.Add(time.Second), 2, 100), nil)
	if !e.Evaluate(at.Add(6*time.Second), sample(at.Add(6*time.Second), 3, 100), nil).Permit {
		t.Fatal("healthy expected")
	}
	if d := e.Evaluate(at.Add(7*time.Second), sample(at.Add(7*time.Second), 4, 90), nil); !d.Permit {
		t.Fatal(d)
	}
}

func TestRestartDoesNotAccumulateHealthFromPreLeadershipWindows(t *testing.T) {
	cfg := policy()
	cfg.MaxAgeSeconds = 60
	cfg.StableSeconds = 20
	e := NewEngine(cfg)
	now := time.Unix(2000, 0)
	for i, age := range []int{30, 5, 1} {
		s := sample(now.Add(-seconds(age)), int64(i+1), 100)
		if d := e.Evaluate(now.Add(seconds(i)), s, nil); d.Permit {
			t.Fatal("old history established new leader health", d)
		}
	}
}
