// Package metrics contains bounded, transport-independent accumulators.
package metrics

import "time"

var durationBounds = [...]time.Duration{time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second, 10 * time.Second}

// Duration uses non-cumulative buckets. Callers provide synchronization.
type Duration struct {
	Count uint64
	Total time.Duration
	Max   time.Duration
	bins  [12]uint64
}

type Bucket struct {
	Upper    time.Duration
	Overflow bool
	Count    uint64
}

func (d *Duration) Record(elapsed time.Duration) {
	d.Count++
	d.Total += elapsed
	d.Max = max(d.Max, elapsed)
	index := len(durationBounds)
	for i, bound := range durationBounds {
		if elapsed <= bound {
			index = i
			break
		}
	}
	d.bins[index]++
}

func (d Duration) Buckets() []Bucket {
	result := make([]Bucket, len(d.bins))
	for i, count := range d.bins {
		result[i] = Bucket{Count: count, Overflow: i == len(durationBounds)}
		if i < len(durationBounds) {
			result[i].Upper = durationBounds[i]
		}
	}
	return result
}
