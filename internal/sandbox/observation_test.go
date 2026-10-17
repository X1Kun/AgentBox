package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/qos"
)

func TestObservationDistinguishesUnknownFromZero(t *testing.T) {
	pressure := qos.Pressure{Some: qos.PressureValue{TotalUsec: 123}}
	for _, valid := range []bool{false, true} {
		report := pressureReport(pressure, 0, 0, valid)
		if report.Full != nil || (report.Some.StallPercent != nil) != valid {
			t.Fatalf("unexpected pressure availability: %+v", report)
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		want := `"stall_percent":null`
		if valid {
			want = `"stall_percent":0`
		}
		if !strings.Contains(string(encoded), want) || !strings.Contains(string(encoded), `"full":null`) {
			t.Fatalf("unknown/zero distinction lost: %s", encoded)
		}
		resources := cgroupObservation(cgroup.Stats{MemoryHigh: cgroup.Limit{Unlimited: true}}, qos.Delta{Valid: valid})
		if (resources.Delta != nil) != valid || !resources.MemoryHigh.Unlimited || resources.MemoryMax.Unlimited {
			t.Fatalf("resource availability/limits lost: %+v", resources)
		}
	}
	if (&Engine{}).observationReport() != nil {
		t.Fatal("off mode must omit observation report")
	}
}
