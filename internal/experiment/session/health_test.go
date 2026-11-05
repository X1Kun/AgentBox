package session

import (
	"github.com/X1Kun/AgentBox/internal/admission/health"
	"testing"
	"time"
)

func TestQualityWaitsForDeadlinesKeepsDenominatorAndFreezesWindow(t *testing.T) {
	start := time.Unix(2000, 0)
	c := Config{Targets: []Target{{ID: "ses-a", Revision: "v1"}}, MaxInflight: 4, MaxLagMS: 100, DeadlineMS: 1000}
	for i, id := range []string{"ok", "reject", "missing", "wrong"} {
		c.Requests = append(c.Requests, Request{ID: id, AtMS: i * 100, Target: 0, Action: "execute", Code: "print(42)", TimeoutSeconds: 2, ExpectedOut: "42\n"})
	}
	contract := health.Contract{Domain: "worker", Revision: "v1", Profile: "followup", WindowSeconds: 2, DeadlineSeconds: 1}
	q, err := NewQuality(c, contract, start)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Requests {
		q.Arrival(Event{ID: r.ID, SentMS: float64(r.AtMS)})
	}
	q.Record(Event{ID: "ok", Outcome: "correct", SentMS: 0, FinishedMS: 100})
	q.Record(Event{ID: "reject", Outcome: "rejected", SentMS: 100, FinishedMS: 150})
	q.Record(Event{ID: "wrong", Outcome: "wrong", SentMS: 300, FinishedMS: 400})
	if _, ok := q.Snapshot(start.Add(2900 * time.Millisecond)); ok {
		t.Fatal("window closed before deadlines")
	}
	s, ok := q.Snapshot(start.Add(3 * time.Second))
	if !ok || !s.Valid || s.Planned != 4 || s.Timely != 1 || s.Rejected != 1 || s.Timeouts != 1 || s.Errors != 1 {
		t.Fatal(s)
	}
	q.Record(Event{ID: "missing", Outcome: "correct", SentMS: 200, FinishedMS: 400})
	if next, _ := q.Snapshot(start.Add(3500 * time.Millisecond)); next != s {
		t.Fatal("late publication rewrote closed window", next)
	}
}
func TestQualityMissingDispatchAndDroppedArrivalsInvalidateWindow(t *testing.T) {
	start := time.Unix(2000, 0)
	c := Config{Targets: []Target{{ID: "ses-a", Revision: "v1"}}, MaxInflight: 1, MaxLagMS: 100, DeadlineMS: 1000, Requests: []Request{{ID: "one", Action: "execute", Code: "print(42)", TimeoutSeconds: 2}}}
	contract := health.Contract{Domain: "worker", Revision: "v1", Profile: "followup", WindowSeconds: 2, DeadlineSeconds: 1}
	q, err := NewQuality(c, contract, start)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := q.Snapshot(start.Add(3 * time.Second)); s.Valid || s.Timeouts != 1 || s.Planned != 1 {
		t.Fatal(s)
	}
}
