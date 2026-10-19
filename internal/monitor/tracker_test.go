package monitor

import (
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
)

const (
	up   = check.StatusUp
	deg  = check.StatusDegraded
	down = check.StatusDown
)

var base = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// feed observes statuses one minute apart and returns the transitions.
func feed(tr *Tracker, statuses ...check.Status) []Transition {
	var out []Transition
	for i, s := range statuses {
		if t, ok := tr.Observe(s, base.Add(time.Duration(i)*time.Minute)); ok {
			out = append(out, t)
		}
	}
	return out
}

func TestTrackerFirstUpIsImmediate(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 3, RecoverThreshold: 2})
	got := feed(tr, up)
	if len(got) != 1 || got[0].From != check.StatusUnknown || got[0].To != up {
		t.Fatalf("got %+v, want unknown->up", got)
	}
}

func TestTrackerSuppressesFlapping(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 3, RecoverThreshold: 2})
	// Isolated failures interrupted by successes never reach the threshold.
	got := feed(tr, up, down, down, up, down, up, down, down, up)
	if len(got) != 1 {
		t.Fatalf("got %d transitions %+v, want only the initial unknown->up", len(got), got)
	}
	if s, _ := tr.State(); s != up {
		t.Fatalf("state = %s, want up", s)
	}
}

func TestTrackerConfirmsAfterThreshold(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 3, RecoverThreshold: 2})
	got := feed(tr, up, down, down, down, up, up)
	if len(got) != 3 {
		t.Fatalf("got %+v, want 3 transitions", got)
	}
	if got[1].From != up || got[1].To != down {
		t.Fatalf("second transition = %+v, want up->down", got[1])
	}
	// The outage started at the first failing probe, not the confirming one.
	if want := base.Add(1 * time.Minute); !got[1].Since.Equal(want) {
		t.Fatalf("down since %v, want %v", got[1].Since, want)
	}
	if got[2].From != down || got[2].To != up || !got[2].Since.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("recovery = %+v", got[2])
	}
}

func TestTrackerRecoveryNeedsConsecutiveSuccesses(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 1, RecoverThreshold: 3})
	got := feed(tr, up, down, up, up, down, up, up)
	if len(got) != 2 {
		t.Fatalf("got %+v, want unknown->up and up->down only", got)
	}
	if s, _ := tr.State(); s != down {
		t.Fatalf("state = %s, want still down", s)
	}
	if got := feed(tr, up); len(got) != 1 || got[0].To != up {
		t.Fatalf("third consecutive success should recover, got %+v", got)
	}
}

func TestTrackerMixedFailureStreakConfirmsMildest(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 3, RecoverThreshold: 1})
	got := feed(tr, up, down, deg, down)
	if len(got) != 2 || got[1].To != deg {
		t.Fatalf("got %+v, want up->degraded after mixed bad streak", got)
	}
	// Further downs from degraded escalate after another full streak.
	got = feed(tr, down, down, down)
	if len(got) != 1 || got[0].From != deg || got[0].To != down {
		t.Fatalf("got %+v, want degraded->down", got)
	}
}

func TestTrackerUnknownToDownNeedsConfirmation(t *testing.T) {
	tr := NewTracker(Policy{FailThreshold: 2, RecoverThreshold: 1})
	if got := feed(tr, down); len(got) != 0 {
		t.Fatalf("single failure at start-up confirmed: %+v", got)
	}
	if got := feed(tr, down); len(got) != 1 || got[0].From != check.StatusUnknown || got[0].To != down {
		t.Fatalf("got %+v, want unknown->down", got)
	}
}
