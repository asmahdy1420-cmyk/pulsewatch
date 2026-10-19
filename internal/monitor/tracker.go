package monitor

import (
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
)

// Policy sets how many consecutive observations confirm a state change.
type Policy struct {
	// FailThreshold confirms a move to a worse state (up -> degraded/down).
	FailThreshold int
	// RecoverThreshold confirms a move to a better state.
	RecoverThreshold int
}

func (p Policy) need(from, to check.Status) int {
	n := p.RecoverThreshold
	if to.Severity() > from.Severity() {
		n = p.FailThreshold
	}
	return max(n, 1)
}

// Transition is a confirmed state change.
type Transition struct {
	From check.Status
	To   check.Status
	// Since is when the first observation of the confirming streak was
	// made, i.e. the best estimate of when the change really happened.
	Since time.Time
}

// Tracker is the per-check state machine. It turns a stream of noisy probe
// outcomes into confirmed states:
//
//	unknown --first up--> up
//	up <--N bad / M good--> degraded <--N bad / M good--> down
//
// A change is only confirmed after a streak of observations on the same
// side of the current state. Mixed streaks are merged conservatively:
// while worsening, a streak of {down, degraded, down} confirms the milder
// "degraded"; while improving, {up, degraded, up} confirms the worse
// "degraded". A single observation equal to the current state resets the
// streak, which is what suppresses flapping.
//
// Tracker is not safe for concurrent use; the monitor serialises access.
type Tracker struct {
	policy    Policy
	state     check.Status
	since     time.Time
	candidate check.Status
	streak    int
	streakAt  time.Time
}

// NewTracker returns a tracker in the unknown state.
func NewTracker(p Policy) *Tracker {
	return &Tracker{policy: p, state: check.StatusUnknown}
}

// State returns the confirmed state and when it was entered.
func (t *Tracker) State() (check.Status, time.Time) { return t.state, t.since }

// Restore forces the confirmed state, e.g. from an incident left open by a
// previous run, so that the next healthy streak closes it.
func (t *Tracker) Restore(s check.Status, since time.Time) {
	t.state, t.since = s, since
	t.reset()
}

func (t *Tracker) reset() {
	t.candidate, t.streak, t.streakAt = "", 0, time.Time{}
}

// Observe feeds one probe outcome observed at time at. It returns the
// transition when the observation confirms a state change.
func (t *Tracker) Observe(s check.Status, at time.Time) (Transition, bool) {
	if s == t.state {
		t.reset()
		return Transition{}, false
	}
	// The very first healthy probe is trusted immediately: there is nothing
	// to flap from, and no alert is sent for it.
	if t.state == check.StatusUnknown && s == check.StatusUp {
		return t.commit(s, at), true
	}

	worse := s.Severity() > t.state.Severity()
	switch {
	case t.candidate == "":
		t.candidate, t.streak, t.streakAt = s, 0, at
	case worse == (t.candidate.Severity() > t.state.Severity()):
		// Same direction as the running streak: merge conservatively.
		if worse && s.Severity() < t.candidate.Severity() ||
			!worse && s.Severity() > t.candidate.Severity() {
			t.candidate = s
		}
	default:
		// Direction flipped: start a new streak.
		t.candidate, t.streak, t.streakAt = s, 0, at
	}
	t.streak++

	if t.streak < t.policy.need(t.state, t.candidate) {
		return Transition{}, false
	}
	return t.commit(t.candidate, t.streakAt), true
}

func (t *Tracker) commit(s check.Status, since time.Time) Transition {
	tr := Transition{From: t.state, To: s, Since: since}
	t.state, t.since = s, since
	t.reset()
	return tr
}
