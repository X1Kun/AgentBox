package qos

import (
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

const auxiliaryInterval = time.Second

// Optional families have independent availability. A missing io controller
// must not invalidate CPU/memory readings. Raw counters retain their own time.
type Auxiliary struct {
	At                           time.Time
	Duration                     time.Duration
	IOAvailable                  bool
	IOError                      string
	IO                           Pressure
	IOValid                      bool
	IOBaseline                   string
	IOInterval                   time.Duration
	IOSomePercent, IOFullPercent float64
	DevicesAvailable             bool
	DevicesError                 string
	Devices                      []cgroup.DeviceIO
	PIDsAvailable                bool
	PIDsError                    string
	PIDs                         cgroup.PIDStats
	PIDDeltaValid                bool
	PIDLimitHits                 uint64
	PIDBaseline                  string
}

type cachedAuxiliary struct {
	identity identity
	reading  Auxiliary
}

func cloneAuxiliary(a Auxiliary) Auxiliary {
	a.Devices = append([]cgroup.DeviceIO(nil), a.Devices...)
	return a
}

func readAuxiliary(source Source, read func(string) (string, error), previous cachedAuxiliary, id identity) Auxiliary {
	start := time.Now()
	a := Auxiliary{}
	name := "io"
	if source.Cgroup {
		name = "io.pressure"
	}
	data, err := read(name)
	if err == nil {
		a.IO, err = ParsePressure(data)
	}
	if err != nil {
		a.IOError = err.Error()
	} else {
		a.IOAvailable = true
	}
	if source.Cgroup {
		data, err = read("io.stat")
		if err == nil {
			a.Devices, err = cgroup.ParseIOStats(data)
		}
		if err != nil {
			a.DevicesError = err.Error()
		} else {
			a.DevicesAvailable = true
		}
		a.PIDs, err = cgroup.ReadPIDs(read)
		if err != nil {
			a.PIDsError = err.Error()
		} else {
			a.PIDsAvailable = true
		}
	}
	a.At = time.Now()
	a.Duration = a.At.Sub(start)
	p := previous.reading
	sameSource := previous.identity == id && !p.At.IsZero()
	elapsed := a.At.Sub(p.At)
	validInterval := sameSource && elapsed > 0 && elapsed <= 5*auxiliaryInterval
	if a.IOAvailable {
		a.IOBaseline = "initial"
		if sameSource && p.IOAvailable {
			switch {
			case !validInterval:
				a.IOBaseline = "sample_gap"
			case a.IO.Some.TotalUsec < p.IO.Some.TotalUsec || a.IO.HasFull != p.IO.HasFull || a.IO.Full.TotalUsec < p.IO.Full.TotalUsec:
				a.IOBaseline = "counter_reset"
			default:
				a.IOValid, a.IOBaseline, a.IOInterval = true, "", elapsed
				us := float64(elapsed) / float64(time.Microsecond)
				a.IOSomePercent = 100 * float64(a.IO.Some.TotalUsec-p.IO.Some.TotalUsec) / us
				a.IOFullPercent = 100 * float64(a.IO.Full.TotalUsec-p.IO.Full.TotalUsec) / us
			}
		}
	}
	if a.PIDsAvailable {
		a.PIDBaseline = "initial"
		if validInterval && p.PIDsAvailable {
			if a.PIDs.LimitHits < p.PIDs.LimitHits {
				a.PIDBaseline = "counter_reset"
			} else {
				a.PIDDeltaValid, a.PIDBaseline = true, ""
				a.PIDLimitHits = a.PIDs.LimitHits - p.PIDs.LimitHits
			}
		}
	}
	return a
}
