package metrics

import (
	"testing"
	"time"
)

func TestDurationBucketsConserveAllObservations(t *testing.T) {
	var d Duration
	for _, elapsed := range []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 11 * time.Second} {
		d.Record(elapsed)
	}
	var count uint64
	buckets := d.Buckets()
	for _, bucket := range buckets {
		count += bucket.Count
	}
	if count != d.Count || d.Count != 4 || d.Total != 11*time.Second+3*time.Millisecond || d.Max != 11*time.Second || buckets[0].Count != 2 || !buckets[len(buckets)-1].Overflow || buckets[len(buckets)-1].Count != 1 {
		t.Fatalf("invalid distribution: %+v %+v", d, buckets)
	}
	buckets[0].Count = 999
	if d.Buckets()[0].Count != 2 {
		t.Fatal("snapshot mutated accumulator")
	}
}
