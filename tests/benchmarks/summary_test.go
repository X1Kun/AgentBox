//go:build integration

package benchmarks

import (
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
)

func TestBenchmarkSummaryKeepsFailuresOutsideSuccessPercentiles(t *testing.T) {
	rows := []sample{
		{HTTP: 200, Status: "succeeded", Cleanup: true, WallMS: 10, Timings: api.Timings{SetupMS: 1, ExecutionMS: 8}},
		{HTTP: 200, Status: "succeeded", Cleanup: true, WallMS: 20, Timings: api.Timings{SetupMS: 2, ExecutionMS: 16}},
		{HTTP: 503, WallMS: 1},
		{HTTP: 500, WallMS: 1000},
		{HTTP: 200, Status: "timeout", Cleanup: true, WallMS: 30000},
	}
	s := collectSummary("warm", "steady", rows, time.Second)
	if s.Offered != 5 || s.Accepted != 3 || s.Completed != 2 || s.Rejected != 1 || s.Errors != 2 || s.P50MS != 10 || s.P99MS != 20 || s.CompletionRate != 2 {
		t.Fatalf("misleading summary: %+v", s)
	}
}
