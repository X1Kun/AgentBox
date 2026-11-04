package session

import (
	"context"
	"encoding/json"
	"errors"
	client "github.com/X1Kun/AgentBox/internal/session/client"
	protocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	"sync/atomic"
	"testing"
	"time"
)

type backend struct {
	calls atomic.Int32
	block bool
}

func (b *backend) Execute(ctx context.Context, id string, r protocol.ExecuteRequest) client.Reply {
	b.calls.Add(1)
	if b.block {
		<-ctx.Done()
		return client.Failure(503, "operation_outcome_unknown", true)
	}
	raw, _ := json.Marshal(protocol.ExecutionView{SessionID: id, IdentityRevision: r.IdentityRevision, Execution: protocol.Execution{Stdout: "42\n"}})
	return client.Reply{HTTPStatus: 200, Response: raw}
}
func (b *backend) CloseSession(context.Context, string) client.Reply {
	return client.Reply{HTTPStatus: 202, Response: json.RawMessage(`{}`)}
}
func config() Config {
	return Config{Targets: []Target{{ID: "ses-test.uid", Revision: "rev"}}, Requests: []Request{{ID: "one", Action: "execute", Code: "print(42)", TimeoutSeconds: 1, ExpectedOut: "42\n"}}, MaxInflight: 1, MaxLagMS: 1000, DeadlineMS: 1000}
}
func TestCorrectnessAndLedgerFailureAreIndependentOfHTTP(t *testing.T) {
	c := config()
	b := &backend{}
	c.Requests[0].ExpectedOut = "wrong"
	s, e := Run(context.Background(), c, b, func(Event) error { return nil })
	if e != nil || s.Outcomes["wrong"] != 1 || s.Correct != 0 {
		t.Fatal(s, e)
	}
	c = config()
	s, e = Run(context.Background(), c, b, func(Event) error { return errors.New("disk failure") })
	if e == nil || s.Valid {
		t.Fatal("lost ledger was valid", s, e)
	}
}
func TestGeneratorDropsNeverBecomeServiceSuccessOrRetry(t *testing.T) {
	c := config()
	r := c.Requests[0]
	r.ID = "two"
	c.Requests = append(c.Requests, r)
	b := &backend{block: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	events := []Event{}
	s, e := Run(ctx, c, b, func(v Event) error { events = append(events, v); return nil })
	if e != nil || s.Planned != 2 || s.Sent != 1 || s.Dropped != 1 || s.Valid || b.calls.Load() != 1 || len(events) != 2 || s.Outcomes["unknown"] != 1 {
		t.Fatal(s, e, b.calls.Load(), events)
	}
}
func TestValidationRejectsAmbiguousAndUnboundedTraces(t *testing.T) {
	c := config()
	c.Requests = append(c.Requests, c.Requests[0])
	if Validate(c) == nil {
		t.Fatal("duplicate identity accepted")
	}
	c = config()
	c.Requests[0].Target = 1
	if Validate(c) == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestSharedStartAnchorDoesNotReplayLateArrivals(t *testing.T) {
	c := config()
	c.StartAt = "invalid"
	if Validate(c) == nil {
		t.Fatal("invalid timestamp accepted")
	}
	b := &backend{}
	c.StartAt = time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano)
	s, err := Run(context.Background(), c, b, func(Event) error { return nil })
	if err != nil || s.Valid || s.Dropped != 1 || b.calls.Load() != 0 {
		t.Fatal("overdue shared arrival was sent", s, err)
	}
	c.StartAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err = Run(context.Background(), c, b, func(Event) error { return nil }); err == nil {
		t.Fatal("unbounded wait accepted")
	}
	c.StartAt = time.Now().Add(20 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	var event Event
	s, err = Run(context.Background(), c, b, func(e Event) error { event = e; return nil })
	if err != nil || !s.Valid || s.Correct != 1 || event.StartedAt != c.StartAt || event.SentMS < 0 {
		t.Fatal("shared anchor lost", s, err, event)
	}
}

func TestActorsRouteRealBackendsAndReportEachPrincipal(t *testing.T) {
	c := config()
	c.Targets[0].User = "alice"
	second := c.Requests[0]
	second.ID = "bob"
	second.User = "bob"
	second.AtMS = 50
	c.Requests = append(c.Requests, second)
	a, b := &backend{}, &backend{}
	actors := Actors{"alice": a, "bob": b}
	if err := actors.Validate(c); err != nil {
		t.Fatal(err)
	}
	s, err := Run(context.Background(), c, actors, func(Event) error { return nil })
	if err != nil || a.calls.Load() != 1 || b.calls.Load() != 1 || s.ByUser["alice"]["correct"] != 1 || s.ByUser["bob"]["correct"] != 1 {
		t.Fatal(s, err)
	}
	delete(actors, "bob")
	if actors.Validate(c) == nil {
		t.Fatal("missing credential accepted")
	}
}
