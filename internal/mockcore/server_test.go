package mockcore

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestFixedWorkAndRequestBoundary(t *testing.T) {
	h, err := Handler(Config{Iterations: 1024, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	var checksum string
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
		var result struct {
			Iterations int    `json:"iterations"`
			Checksum   string `json:"checksum"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || result.Iterations != 1024 || len(result.Checksum) != 64 {
			t.Fatalf("result: %d %+v", w.Code, result)
		}
		if i > 0 && result.Checksum != checksum {
			t.Fatal("workload changed between requests")
		}
		checksum = result.Checksum
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/work?iterations=2000000", nil))
	if w.Code != 400 {
		t.Fatal("request changed fixed workload", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Work(ctx, 1024); err == nil {
		t.Fatal("cancelled work continued")
	}
}
