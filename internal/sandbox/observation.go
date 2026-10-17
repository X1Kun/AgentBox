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
	report := &api.QoSReport{Mode: "observe", State: snapshot.State, IntervalMS: ms(snapshot.Interval), AgeMS: ms(snapshot.Age), CollectionMS: ms(snapshot.Duration), Stale: snapshot.Stale, Sources: make([]api.ResourceObservation, 0, len(snapshot.Sources))}
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
		CPUUsageUsec: s.CPUUsageUsec, CPUPeriods: s.CPUPeriods, CPUThrottledPeriods: s.CPUThrottledPeriods, CPUThrottledUsec: s.CPUThrottledUsec,
		CPUQuota: api.ResourceLimit{Unlimited: s.CPUQuota.Unlimited, Value: s.CPUQuota.Value}, CPUPeriodUsec: s.CPUPeriodUsec,
		MemoryCurrentBytes: s.MemoryCurrentBytes, MemoryHigh: api.ResourceLimit{Unlimited: s.MemoryHigh.Unlimited, Value: s.MemoryHigh.Value}, MemoryMax: api.ResourceLimit{Unlimited: s.MemoryMax.Unlimited, Value: s.MemoryMax.Value},
		MemoryHighEvents: s.MemoryHighEvents, MemoryMaxEvents: s.MemoryMaxEvents, OOMEvents: s.OOMEvents, OOMKills: s.OOMKills,
	}
	if d.Valid {
		report.Delta = &api.CgroupDelta{CPUUsageCores: d.CPUUsageCores, CPUPeriods: d.CPUPeriods, CPUThrottledPeriods: d.CPUThrottledPeriods, CPUThrottledUsec: d.CPUThrottledUsec, MemoryHighEvents: d.MemoryHighEvents, MemoryMaxEvents: d.MemoryMaxEvents, OOMEvents: d.OOMEvents, OOMKills: d.OOMKills}
	}
	return report
}
