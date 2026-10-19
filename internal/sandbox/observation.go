package sandbox

import (
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/qos"
)

func (e *Engine) observationReport() *api.QoSReport {
	if e.observer == nil {
		return nil
	}
	snapshot := e.observer.Snapshot()
	var pressure *api.PressureStateReport
	if p := snapshot.Pressure; p != nil {
		pressure = &api.PressureStateReport{State: p.State, EffectiveState: p.EffectiveState, Reason: p.Reason, SuggestedAdmissionOpen: p.AdmissionOpen, SuggestedRefillPaused: p.RefillPaused, SampleSequence: p.Sequence, EvaluatedAt: p.At.UTC().Format(time.RFC3339Nano)}
	}
	report := &api.QoSReport{Mode: e.config.QoS.Mode, State: snapshot.State, IntervalMS: ms(snapshot.Interval), AgeMS: ms(snapshot.Age), CollectionMS: ms(snapshot.Duration), Stale: snapshot.Stale, Sources: make([]api.ResourceObservation, 0, len(snapshot.Sources))}
	if c := snapshot.Control; c != nil {
		report.CPUControl = &api.CPUControlReport{State: c.State, Reason: c.Reason, Action: c.Action, SampleSequence: c.Sequence, ActualCPU: c.ActualCPU, SuggestedCPU: c.SuggestedCPU, Applied: c.Applied, Decisions: c.Decisions, Changes: c.Changes, WriteFailures: c.WriteFailures, Error: c.Error, EvaluatedAt: c.At.UTC().Format(time.RFC3339Nano), MinCPU: c.Config.MinCPU, MaxCPU: c.Config.MaxCPU, LowPressure: c.Config.LowPressure, HighPressure: c.Config.HighPressure, DecisionIntervalMS: ms(c.Config.DecisionInterval)}
		report.CPUControl.StableForMS, report.CPUControl.DecreaseFactor, report.CPUControl.IncreaseCPUPerSecond = ms(c.Config.StableFor), c.Config.DecreaseFactor, c.Config.IncreaseCPUPerSecond
	}
	report.Sequence, report.HierarchyBoundary, report.HierarchyBoundaryReason = snapshot.Sequence, snapshot.Boundary.Path, snapshot.Boundary.Reason
	report.Pressure, report.Scheduling = pressure, e.qosSchedulingReport()
	if !snapshot.CollectedAt.IsZero() {
		report.CollectedAt = snapshot.CollectedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, observation := range snapshot.Sources {
		source := api.ResourceObservation{Name: observation.Source.Name, Scope: "system", Path: observation.Source.Path, Available: observation.Available, Error: observation.Error, Baseline: observation.Baseline}
		if observation.Source.Cgroup {
			source.Scope = "cgroup"
		}
		if observation.Available {
			reading, delta := observation.Reading, observation.Delta
			source.SampledAt = reading.At.UTC().Format(time.RFC3339Nano)
			source.ReadMS = ms(reading.Duration)
			source.Sample = &api.ResourceSample{CPU: pressureReport(reading.CPU, delta.CPUSomePercent, delta.CPUFullPercent, delta.Valid), Memory: pressureReport(reading.Memory, delta.MemorySomePercent, delta.MemoryFullPercent, delta.Valid)}
			source.Sample.Auxiliary = auxiliaryReport(reading.Auxiliary, observation.Source.Cgroup, snapshot.Stale)
			if delta.Valid {
				interval := ms(delta.Interval)
				source.IntervalMS = &interval
			}
			if observation.Source.Cgroup {
				source.Sample.Cgroup = cgroupObservation(reading.Resources, delta)
			}
		}
		report.Sources = append(report.Sources, source)
	}
	return report
}
func pressureReport(pressure qos.Pressure, some, full float64, valid bool) api.PressureReport {
	value := func(raw qos.PressureValue, percent float64) api.PressureValue {
		v := api.PressureValue{Avg10: raw.Avg10, Avg60: raw.Avg60, Avg300: raw.Avg300, TotalUsec: raw.TotalUsec}
		if valid {
			v.StallPercent = &percent
		}
		return v
	}
	result := api.PressureReport{Some: value(pressure.Some, some)}
	if pressure.HasFull {
		fullValue := value(pressure.Full, full)
		result.Full = &fullValue
	}
	return result
}
func cgroupObservation(s cgroup.Stats, d qos.Delta) *api.CgroupObservation {
	report := &api.CgroupObservation{
		CPUUserUsec: s.CPUUserUsec, CPUSystemUsec: s.CPUSystemUsec,
		CPUUsageUsec: s.CPUUsageUsec, CPUPeriods: s.CPUPeriods, CPUThrottledPeriods: s.CPUThrottledPeriods, CPUThrottledUsec: s.CPUThrottledUsec,
		CPUQuota: api.ResourceLimit{Unlimited: s.CPUQuota.Unlimited, Value: s.CPUQuota.Value}, CPUPeriodUsec: s.CPUPeriodUsec,
		MemoryCurrentBytes: s.MemoryCurrentBytes, MemoryHigh: api.ResourceLimit{Unlimited: s.MemoryHigh.Unlimited, Value: s.MemoryHigh.Value}, MemoryMax: api.ResourceLimit{Unlimited: s.MemoryMax.Unlimited, Value: s.MemoryMax.Value},
		MemoryHighEvents: s.MemoryHighEvents, MemoryMaxEvents: s.MemoryMaxEvents, OOMEvents: s.OOMEvents, OOMKills: s.OOMKills,
	}
	if d.Valid {
		report.Delta = &api.CgroupDelta{CPUUsageCores: d.CPUUsageCores, CPUPeriods: d.CPUPeriods, CPUThrottledPeriods: d.CPUThrottledPeriods, CPUThrottledUsec: d.CPUThrottledUsec, MemoryHighEvents: d.MemoryHighEvents, MemoryMaxEvents: d.MemoryMaxEvents, OOMEvents: d.OOMEvents, OOMKills: d.OOMKills}
		report.Delta.CPUUserCores, report.Delta.CPUSystemCores = d.CPUUserCores, d.CPUSystemCores
	}
	return report
}

func auxiliaryReport(a qos.Auxiliary, group, parentStale bool) *api.AuxiliaryReport {
	if a.At.IsZero() {
		return nil
	}
	age := time.Since(a.At)
	r := &api.AuxiliaryReport{SampledAt: a.At.UTC().Format(time.RFC3339Nano), AgeMS: ms(age), ReadMS: ms(a.Duration), IntervalMS: 1000, Stale: parentStale || age > 3*time.Second}
	r.IO = api.OptionalPressure{Available: a.IOAvailable, Error: a.IOError, Baseline: a.IOBaseline}
	if a.IOAvailable {
		pressure := pressureReport(a.IO, a.IOSomePercent, a.IOFullPercent, a.IOValid && !r.Stale)
		r.IO.Pressure = &pressure
		if a.IOValid && !r.Stale {
			elapsed := ms(a.IOInterval)
			r.IO.IntervalMS = &elapsed
		}
		if r.Stale {
			r.IO.Baseline = "stale"
		}
	}
	if group {
		r.Devices = &api.OptionalDevices{Available: a.DevicesAvailable, Error: a.DevicesError}
		if a.DevicesAvailable {
			r.Devices.Devices = []api.DeviceIO{}
			for _, d := range a.Devices {
				r.Devices.Devices = append(r.Devices.Devices, api.DeviceIO{Device: d.Device, ReadBytes: d.ReadBytes, WriteBytes: d.WriteBytes, ReadIOs: d.ReadIOs, WriteIOs: d.WriteIOs})
			}
		}
		r.PIDs = &api.OptionalPIDs{Available: a.PIDsAvailable, Error: a.PIDsError, Baseline: a.PIDBaseline}
		if a.PIDsAvailable {
			r.PIDs.Sample = &api.PIDStats{Current: a.PIDs.Current, Max: api.ResourceLimit{Unlimited: a.PIDs.Max.Unlimited, Value: a.PIDs.Max.Value}, LimitHits: a.PIDs.LimitHits}
			if a.PIDDeltaValid && !r.Stale {
				hits := a.PIDLimitHits
				r.PIDs.LimitHitsDelta = &hits
			}
			if r.Stale {
				r.PIDs.Baseline = "stale"
			}
		}
	}
	return r
}
