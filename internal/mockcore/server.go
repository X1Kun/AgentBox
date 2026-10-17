// Package mockcore provides a bounded, fixed-workload HTTP service for experiments.
package mockcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Config struct{ Iterations, Concurrency int }

func (c Config) Validate() error {
	if c.Iterations < 1 || c.Iterations > 2000000 {
		return fmt.Errorf("iterations must be 1..2000000")
	}
	if c.Concurrency < 1 || c.Concurrency > 16 {
		return fmt.Errorf("concurrency must be 1..16")
	}
	return nil
}

// Work does a deterministic amount of hashing. Elapsed time is deliberately not
// the stopping condition, since scheduling delays must remain measurable.
func Work(ctx context.Context, iterations int) (string, error) {
	sum := sha256.Sum256([]byte("agentbox-mock-core"))
	for i := 0; i < iterations; i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		sum = sha256.Sum256(sum[:])
	}
	return hex.EncodeToString(sum[:]), nil
}
func Handler(c Config) (http.Handler, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	slots := make(chan struct{}, c.Concurrency)
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]bool{"alive": true}) })
	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			reply(w, 400, map[string]string{"error": "workload is configured by the operator; request parameters are not accepted"})
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			reply(w, 503, map[string]string{"error": "core capacity exhausted"})
			return
		}
		defer func() { <-slots }()
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		checksum, err := Work(ctx, c.Iterations)
		if err != nil {
			reply(w, 408, map[string]string{"error": "work cancelled"})
			return
		}
		reply(w, 200, struct {
			Iterations int     `json:"iterations"`
			Checksum   string  `json:"checksum"`
			WorkMS     float64 `json:"work_ms"`
		}{c.Iterations, checksum, float64(time.Since(start)) / float64(time.Millisecond)})
	})
	return mux, nil
}
