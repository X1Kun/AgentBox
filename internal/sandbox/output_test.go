package sandbox

import (
	"io"
	"strings"
	"sync"
	"testing"
)

func TestSharedOutputBudgetAndDrain(t *testing.T) {
	collector := newOutputCollector(17)
	var wg sync.WaitGroup
	for _, stderr := range []bool{false, true} {
		wg.Add(1)
		go func(stderr bool) {
			defer wg.Done()
			n, err := io.Copy(collector.writer(stderr), strings.NewReader(strings.Repeat("x", 100000)))
			if err != nil || n != 100000 {
				t.Errorf("drain: %d %v", n, err)
			}
		}(stderr)
	}
	wg.Wait()
	out, errOut, truncated := collector.result()
	if len(out)+len(errOut) != 17 || !truncated {
		t.Fatalf("budget: %d %d %v", len(out), len(errOut), truncated)
	}
	select {
	case <-collector.limitExceeded:
	default:
		t.Fatal("overflow did not notify cancellation")
	}
}

func TestExactOutputLimitIsAllowed(t *testing.T) {
	collector := newOutputCollector(5)
	_, _ = collector.writer(false).Write([]byte("hello"))
	_, _, truncated := collector.result()
	if truncated {
		t.Fatal("exactly at limit should be allowed")
	}
}
