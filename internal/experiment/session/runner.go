// Package session drives bounded, open-arrival experiments through the product API.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/X1Kun/AgentBox/internal/requestid"
	"sort"
	"sync"
	"time"

	client "github.com/X1Kun/AgentBox/internal/session/client"
	protocol "github.com/X1Kun/AgentBox/internal/session/protocol"
)

type Target struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	User     string `json:"user,omitempty"`
}
type Request struct {
	ClientTimeoutMS int    `json:"client_timeout_ms,omitempty"`
	User            string `json:"user,omitempty"`
	ID              string `json:"id"`
	AtMS            int    `json:"at_ms"`
	Target          int    `json:"target"`
	Action          string `json:"action"`
	Code            string `json:"code"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	ExpectedOut     string `json:"expected_out"`
	ExpectedExit    int    `json:"expected_exit"`
}
type Config struct {
	StartAt     string    `json:"start_at,omitempty"`
	Targets     []Target  `json:"targets"`
	Requests    []Request `json:"requests"`
	MaxInflight int       `json:"max_inflight"`
	MaxLagMS    int       `json:"max_lag_ms"`
	DeadlineMS  int       `json:"deadline_ms"`
}
type Event struct {
	StartedAt string `json:"trace_started_at"`
	RequestID string `json:"request_id"`
	User      string `json:"user,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`

	ID           string  `json:"id"`
	Target       string  `json:"session_id"`
	Action       string  `json:"action"`
	PlannedMS    int     `json:"planned_ms"`
	SentMS       float64 `json:"sent_ms"`
	FinishedMS   float64 `json:"finished_ms"`
	HTTP         int     `json:"http_status"`
	Outcome      string  `json:"outcome"`
	OutputSHA256 string  `json:"output_sha256,omitempty"`
	Timely       bool    `json:"timely"`
}
type Summary struct {
	Planned       int                       `json:"planned"`
	Sent          int                       `json:"sent"`
	Dropped       int                       `json:"dropped"`
	Late          int                       `json:"late"`
	Correct       int                       `json:"correct"`
	TimelyCorrect int                       `json:"timely_correct"`
	Outcomes      map[string]int            `json:"outcomes"`
	ByUser        map[string]map[string]int `json:"by_user"`
	Valid         bool                      `json:"measurement_valid"`
	P50MS         float64                   `json:"p50_ms"`
	P95MS         float64                   `json:"p95_ms"`
	MaxMS         float64                   `json:"max_ms"`
}
type Backend interface {
	Execute(context.Context, string, protocol.ExecuteRequest) client.Reply
	CloseSession(context.Context, string) client.Reply
}

func Validate(c Config) error {
	if c.StartAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, c.StartAt); err != nil {
			return errors.New("invalid experiment start time")
		}
	}
	if len(c.Targets) == 0 || len(c.Targets) > 16 || len(c.Requests) == 0 || len(c.Requests) > 2000 || c.MaxInflight < 1 || c.MaxInflight > 16 || c.MaxLagMS < 1 || c.MaxLagMS > 5000 || c.DeadlineMS < 1 || c.DeadlineMS > 120000 {
		return errors.New("invalid experiment bounds")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if !client.ValidID(t.ID) || t.Revision == "" || (t.User != "" && !client.ValidRequestID(t.User)) {
			return errors.New("invalid target")
		}
	}
	for _, r := range c.Requests {
		if r.ClientTimeoutMS < 0 || r.ClientTimeoutMS > 70000 {
			return errors.New("invalid client timeout")
		}
		if (r.User != "" && !client.ValidRequestID(r.User)) || !client.ValidRequestID(r.ID) || seen[r.ID] || r.Target < 0 || r.Target >= len(c.Targets) || r.AtMS < 0 || r.AtMS > 120000 {
			return errors.New("invalid trace identity or schedule")
		}
		seen[r.ID] = true
		if r.Action != "execute" && r.Action != "close" {
			return errors.New("invalid action")
		}
		if r.Action == "execute" && (r.Code == "" || len(r.Code) > protocol.MaxCodeBytes || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 60) {
			return errors.New("invalid execution")
		}
	}
	return nil
}
func classify(r Request, reply client.Reply) (string, string) {
	var envelope struct {
		Unknown bool   `json:"outcome_unknown"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(reply.Response, &envelope)
	if envelope.Unknown || envelope.Error == "execution_timeout" || envelope.Error == "operation_outcome_unknown" {
		return "unknown", ""
	}
	if reply.HTTPStatus == 409 || reply.HTTPStatus == 429 {
		return "rejected", ""
	}
	if reply.HTTPStatus < 200 || reply.HTTPStatus >= 300 {
		return "error", ""
	}
	if r.Action == "close" {
		return "close_acknowledged", ""
	}
	var v protocol.ExecutionView
	if json.Unmarshal(reply.Response, &v) != nil {
		return "wrong", ""
	}
	h := sha256.Sum256([]byte(v.Stdout))
	digest := hex.EncodeToString(h[:])
	if v.ExitCode != r.ExpectedExit || v.Stdout != r.ExpectedOut {
		return "wrong", digest
	}
	return "correct", digest
}

// Run never queues an overdue arrival behind active requests and never retries
// a mutation. All planned arrivals receive a terminal ledger entry, including drops.
func Run(ctx context.Context, c Config, b Backend, emit func(Event) error) (Summary, error) {
	if err := Validate(c); err != nil {
		return Summary{}, err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	requests := append([]Request(nil), c.Requests...)
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].AtMS < requests[j].AtMS })
	start := time.Now()
	if c.StartAt != "" {
		anchor, _ := time.Parse(time.RFC3339Nano, c.StartAt)
		if time.Until(anchor) > 30*time.Second {
			return Summary{}, errors.New("experiment start exceeds scheduling budget")
		}
		// Preserve a monotonic clock after converting the shared wall-clock anchor.
		start = start.Add(anchor.Sub(start))
	}
	slots := make(chan struct{}, c.MaxInflight)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var outputErr error
	result := Summary{Planned: len(requests), Outcomes: map[string]int{}, ByUser: map[string]map[string]int{}, Valid: true}
	latencies := []float64{}
	save := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		result.Outcomes[e.Outcome]++
		if result.ByUser[e.User] == nil {
			result.ByUser[e.User] = map[string]int{}
		}
		result.ByUser[e.User][e.Outcome]++
		if e.SentMS < 0 {
			result.Dropped++
			result.Valid = false
		} else {
			result.Sent++
			latencies = append(latencies, e.FinishedMS-e.SentMS)
			if e.SentMS-float64(e.PlannedMS) > float64(c.MaxLagMS) {
				result.Late++
				result.Valid = false
			}
		}
		if e.Outcome == "correct" {
			result.Correct++
			if e.Timely {
				result.TimelyCorrect++
			}
		}
		if err := emit(e); err != nil && outputErr == nil {
			outputErr = err
			result.Valid = false
			stop()
		}
	}
	for _, r := range requests {
		wait := time.Until(start.Add(time.Duration(r.AtMS) * time.Millisecond))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		e := Event{StartedAt: start.UTC().Format(time.RFC3339Nano), ID: r.ID, Target: c.Targets[r.Target].ID, Action: r.Action, PlannedMS: r.AtMS, RequestID: requestid.From(requestid.With(context.Background(), "")), SentMS: -1, Outcome: "dropped"}
		e.User = c.Targets[r.Target].User
		if r.User != "" {
			e.User = r.User
		}
		if ctx.Err() != nil || time.Since(start) > time.Duration(r.AtMS+c.MaxLagMS)*time.Millisecond {
			save(e)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			save(e)
			continue
		}
		e.SentMS = float64(time.Since(start)) / float64(time.Millisecond)
		wg.Add(1)
		go func(r Request, e Event) {
			defer wg.Done()
			defer func() { <-slots }()
			var reply client.Reply
			duration := time.Duration(r.TimeoutSeconds+10) * time.Second
			if r.ClientTimeoutMS > 0 {
				duration = min(duration, time.Duration(r.ClientTimeoutMS)*time.Millisecond)
			}
			budget, cancel := context.WithTimeout(requestid.With(context.WithValue(ctx, actorKey{}, e.User), e.RequestID), duration)
			defer cancel()
			if r.Action == "close" {
				reply = b.CloseSession(budget, e.Target)
			} else {
				reply = b.Execute(budget, e.Target, protocol.ExecuteRequest{IdentityRevision: c.Targets[r.Target].Revision, Code: r.Code, TimeoutSeconds: r.TimeoutSeconds})
			}
			var failure struct {
				Code string `json:"error"`
			}
			_ = json.Unmarshal(reply.Response, &failure)
			e.ErrorCode = failure.Code
			e.HTTP = reply.HTTPStatus
			e.Outcome, e.OutputSHA256 = classify(r, reply)
			e.FinishedMS = float64(time.Since(start)) / float64(time.Millisecond)
			e.Timely = e.Outcome == "correct" && e.FinishedMS-float64(r.AtMS) <= float64(c.DeadlineMS)
			save(e)
		}(r, e)
	}
	wg.Wait()
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		result.P50MS = latencies[(len(latencies)-1)/2]
		result.P95MS = latencies[(len(latencies)*95-1)/100]
		result.MaxMS = latencies[len(latencies)-1]
	}
	if result.Sent+result.Dropped != result.Planned {
		return result, fmt.Errorf("ledger does not balance")
	}
	return result, outputErr
}

// Actors selects an actual configured credential, not merely a reporting label.
type actorKey struct{}
type Actors map[string]Backend

func (a Actors) Execute(ctx context.Context, id string, r protocol.ExecuteRequest) client.Reply {
	b := a[actor(ctx)]
	if b == nil {
		return client.Failure(400, "unknown_actor", false)
	}
	return b.Execute(ctx, id, r)
}
func (a Actors) CloseSession(ctx context.Context, id string) client.Reply {
	b := a[actor(ctx)]
	if b == nil {
		return client.Failure(400, "unknown_actor", false)
	}
	return b.CloseSession(ctx, id)
}
func actor(ctx context.Context) string { s, _ := ctx.Value(actorKey{}).(string); return s }
func (a Actors) Validate(c Config) error {
	for _, r := range c.Requests {
		user := c.Targets[r.Target].User
		if r.User != "" {
			user = r.User
		}
		if a[user] == nil {
			return errors.New("trace actor has no credential")
		}
	}
	return nil
}
