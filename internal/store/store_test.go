package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/uptime"
)

// Both implementations must satisfy the same contract, so the same suite
// runs against each of them.
func TestStores(t *testing.T) {
	impls := map[string]func(t *testing.T) Store{
		"memory": func(*testing.T) Store { return NewMemory() },
		"sqlite": func(t *testing.T) Store {
			s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "pw.db"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, open := range impls {
		t.Run(name, func(t *testing.T) {
			t.Run("results", func(t *testing.T) {
				s := open(t)
				defer s.Close()
				testResults(t, s)
			})
			t.Run("incidents", func(t *testing.T) {
				s := open(t)
				defer s.Close()
				testIncidents(t, s)
			})
		})
	}
}

var day0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func testResults(t *testing.T, s Store) {
	ctx := context.Background()
	add := func(id string, at time.Time, st check.Status) {
		t.Helper()
		r := check.Result{CheckID: id, At: at, Status: st, Latency: 1500 * time.Microsecond, Message: string(st)}
		if err := s.SaveResult(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Day 0: 3 up, 1 down. Day 1: 1 up, 1 degraded. Other check: noise.
	add("a", day0.Add(1*time.Hour), check.StatusUp)
	add("a", day0.Add(2*time.Hour), check.StatusUp)
	add("a", day0.Add(3*time.Hour), check.StatusDown)
	add("a", day0.Add(4*time.Hour), check.StatusUp)
	add("a", day0.Add(25*time.Hour), check.StatusUp)
	add("a", day0.Add(26*time.Hour), check.StatusDegraded)
	add("b", day0.Add(1*time.Hour), check.StatusDown)

	hist, err := s.History(ctx, "a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 || !hist[0].At.Equal(day0.Add(26*time.Hour)) || hist[0].Status != check.StatusDegraded {
		t.Fatalf("history = %+v, want newest first, limited to 3", hist)
	}
	if hist[0].Latency != 1500*time.Microsecond || hist[0].Message != "degraded" || hist[0].CheckID != "a" {
		t.Fatalf("history round-trip lost data: %+v", hist[0])
	}

	tally, err := s.Tally(ctx, "a", day0, day0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tally != (uptime.Tally{Up: 3, Down: 1}) {
		t.Fatalf("tally = %+v", tally)
	}

	days, err := s.Daily(ctx, "a", day0, day0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []uptime.Day{
		{Day: day0, Tally: uptime.Tally{Up: 3, Down: 1}},
		{Day: day0.Add(24 * time.Hour), Tally: uptime.Tally{Up: 1, Degraded: 1}},
	}
	if len(days) != len(want) {
		t.Fatalf("daily = %+v, want %+v", days, want)
	}
	for i := range want {
		if !days[i].Day.Equal(want[i].Day) || days[i].Tally != want[i].Tally {
			t.Fatalf("daily[%d] = %+v, want %+v", i, days[i], want[i])
		}
	}

	n, err := s.Prune(ctx, day0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("pruned %d rows, want 5", n)
	}
	if hist, _ := s.History(ctx, "a", 100); len(hist) != 2 {
		t.Fatalf("history after prune = %d rows, want 2", len(hist))
	}
}

func testIncidents(t *testing.T, s Store) {
	ctx := context.Background()
	first := &Incident{CheckID: "a", Status: check.StatusDegraded, Cause: "slow", Started: day0}
	second := &Incident{CheckID: "b", Status: check.StatusDown, Cause: "refused", Started: day0.Add(time.Hour)}
	for _, inc := range []*Incident{first, second} {
		if err := s.OpenIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
	}
	if first.ID == 0 || first.ID == second.ID {
		t.Fatalf("ids not assigned: %d, %d", first.ID, second.ID)
	}

	if err := s.EscalateIncident(ctx, first.ID, check.StatusDown); err != nil {
		t.Fatal(err)
	}
	// Escalation never downgrades.
	if err := s.EscalateIncident(ctx, first.ID, check.StatusDegraded); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIncident(ctx, first.ID, day0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIncident(ctx, 9999, day0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("close missing incident: err = %v, want ErrNotFound", err)
	}

	all, err := s.ListIncidents(ctx, IncidentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != second.ID {
		t.Fatalf("list = %+v, want newest first", all)
	}
	got := all[1]
	if got.Status != check.StatusDown || got.Open() || got.Duration(day0.Add(48*time.Hour)) != 30*time.Minute {
		t.Fatalf("closed incident = %+v", got)
	}

	open, _ := s.ListIncidents(ctx, IncidentFilter{OpenOnly: true})
	if len(open) != 1 || open[0].ID != second.ID || !open[0].Open() {
		t.Fatalf("open filter = %+v", open)
	}
	byCheck, _ := s.ListIncidents(ctx, IncidentFilter{CheckID: "a"})
	if len(byCheck) != 1 || byCheck[0].ID != first.ID {
		t.Fatalf("check filter = %+v", byCheck)
	}
	limited, _ := s.ListIncidents(ctx, IncidentFilter{Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("limit filter = %d rows", len(limited))
	}
}
