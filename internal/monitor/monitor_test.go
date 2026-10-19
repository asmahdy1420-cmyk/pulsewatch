package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/notify"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Notify(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) all() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

type harness struct {
	mon   *Monitor
	store *store.Memory
	notes *recorder
	clock *clock.Fake
}

func newHarness(fail, recover int) *harness {
	h := &harness{store: store.NewMemory(), notes: &recorder{}, clock: clock.NewFake(base)}
	spec := check.Spec{ID: "api", Name: "Public API", Target: "https://api.example.test", FailThreshold: fail, RecoverThreshold: recover}
	h.mon = New([]check.Spec{spec}, h.store, h.notes, h.clock, nil)
	return h
}

// probe records one result at the current fake time, then advances it by
// one minute.
func (h *harness) probe(t *testing.T, s check.Status, msg string) {
	t.Helper()
	r := check.Result{CheckID: "api", At: h.clock.Now(), Status: s, Message: msg}
	if err := h.mon.Record(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
}

func TestMonitorNotifiesOnlyOnTransitions(t *testing.T) {
	h := newHarness(2, 2)
	for _, s := range []check.Status{up, up, down, up, up, down, down, down, down, up, up, up} {
		h.probe(t, s, string(s))
	}
	ev := h.notes.all()
	if len(ev) != 2 {
		t.Fatalf("got %d notifications %+v, want exactly down + recovery", len(ev), ev)
	}
	if ev[0].From != up || ev[0].To != down || ev[0].Recovery() {
		t.Fatalf("first event = %+v, want up->down", ev[0])
	}
	if !ev[1].Recovery() || ev[1].Downtime != 4*time.Minute {
		t.Fatalf("second event = %+v, want recovery after 4m", ev[1])
	}
	if !strings.HasPrefix(ev[1].Summary(), "RECOVERED: Public API is up again after 4m0s") {
		t.Fatalf("recovery summary = %q", ev[1].Summary())
	}
	if ev[0].IncidentID == 0 || ev[0].IncidentID != ev[1].IncidentID {
		t.Fatalf("events should reference the same incident: %d vs %d", ev[0].IncidentID, ev[1].IncidentID)
	}
}

func TestMonitorIncidentLifecycle(t *testing.T) {
	h := newHarness(2, 1)
	ctx := context.Background()

	h.probe(t, up, "")                   // 00:00
	h.probe(t, deg, "latency 900ms")     // 00:01 incident starts here
	h.probe(t, deg, "latency 950ms")     // 00:02 confirmed degraded; cause recorded
	h.probe(t, down, "connection reset") // 00:03
	h.probe(t, down, "connection reset") // 00:04 escalated to down

	open, _ := h.store.ListIncidents(ctx, store.IncidentFilter{OpenOnly: true})
	if len(open) != 1 {
		t.Fatalf("open incidents = %d, want 1", len(open))
	}
	inc := open[0]
	if inc.Status != down || inc.Cause != "latency 950ms" || !inc.Started.Equal(base.Add(time.Minute)) {
		t.Fatalf("incident = %+v, want escalated to down, started 00:01", inc)
	}
	if snap, _ := h.mon.Snapshot("api"); snap.Status != down || snap.IncidentID != inc.ID {
		t.Fatalf("snapshot = %+v", snap)
	}

	h.probe(t, up, "") // 00:05 recovered (threshold 1)

	all, _ := h.store.ListIncidents(ctx, store.IncidentFilter{})
	if len(all) != 1 || all[0].Open() {
		t.Fatalf("incidents = %+v, want one closed", all)
	}
	if d := all[0].Duration(h.clock.Now()); d != 4*time.Minute {
		t.Fatalf("incident duration = %s, want 4m", d)
	}
	if hist, _ := h.store.History(ctx, "api", 100); len(hist) != 6 {
		t.Fatalf("history length = %d, want every probe stored", len(hist))
	}
	// up->degraded, degraded->down, down->up
	if n := len(h.notes.all()); n != 3 {
		t.Fatalf("notifications = %d, want 3", n)
	}
}

func TestMonitorRestoreClosesIncidentFromPreviousRun(t *testing.T) {
	h := newHarness(1, 1)
	ctx := context.Background()
	prev := &store.Incident{CheckID: "api", Status: down, Cause: "timeout", Started: base.Add(-time.Hour)}
	if err := h.store.OpenIncident(ctx, prev); err != nil {
		t.Fatal(err)
	}
	orphan := &store.Incident{CheckID: "removed", Status: down, Started: base.Add(-time.Hour)}
	_ = h.store.OpenIncident(ctx, orphan)

	if err := h.mon.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	h.probe(t, up, "")

	open, _ := h.store.ListIncidents(ctx, store.IncidentFilter{OpenOnly: true})
	if len(open) != 0 {
		t.Fatalf("open incidents after recovery = %+v", open)
	}
	ev := h.notes.all()
	if len(ev) != 1 || !ev[0].Recovery() || ev[0].Downtime != time.Hour {
		t.Fatalf("events = %+v, want one recovery after 1h", ev)
	}
}

func TestOverall(t *testing.T) {
	snaps := []Snapshot{{Status: up}, {Status: check.StatusUnknown}, {Status: deg}}
	if got := Overall(snaps); got != deg {
		t.Fatalf("Overall = %s, want degraded", got)
	}
	if got := Overall([]Snapshot{{Status: check.StatusUnknown}}); got != check.StatusUnknown {
		t.Fatalf("Overall = %s, want unknown", got)
	}
}
