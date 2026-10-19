// Package monitor ties probe results to state, incidents and alerts. It
// owns one Tracker per check and is the only writer of incidents.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/notify"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
)

// notifyTimeout bounds how long a single alert delivery may take.
const notifyTimeout = 15 * time.Second

// Snapshot is a point-in-time view of one check.
type Snapshot struct {
	Spec       check.Spec
	Status     check.Status
	Since      time.Time
	Last       *check.Result
	IncidentID int64
}

type entry struct {
	mu       sync.Mutex
	spec     check.Spec
	tracker  *Tracker
	last     *check.Result
	incident *store.Incident
}

// Monitor records results, runs the state machine and raises alerts.
type Monitor struct {
	store    store.Store
	notifier notify.Notifier
	clock    clock.Clock
	log      *slog.Logger

	entries map[string]*entry
	order   []string
}

// New returns a Monitor for specs. notifier may be nil.
func New(specs []check.Spec, st store.Store, n notify.Notifier, clk clock.Clock, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if n == nil {
		n = notify.Multi(nil)
	}
	m := &Monitor{store: st, notifier: n, clock: clk, log: log, entries: make(map[string]*entry, len(specs))}
	for _, s := range specs {
		m.entries[s.ID] = &entry{
			spec:    s,
			tracker: NewTracker(Policy{FailThreshold: s.FailThreshold, RecoverThreshold: s.RecoverThreshold}),
		}
		m.order = append(m.order, s.ID)
	}
	return m
}

// Restore reloads incidents left open by a previous run so that the next
// healthy streak closes them instead of leaving them dangling.
func (m *Monitor) Restore(ctx context.Context) error {
	open, err := m.store.ListIncidents(ctx, store.IncidentFilter{OpenOnly: true})
	if err != nil {
		return fmt.Errorf("load open incidents: %w", err)
	}
	for _, inc := range open {
		e, ok := m.entries[inc.CheckID]
		if !ok {
			// The check was removed from the config; close the orphan.
			if err := m.store.CloseIncident(ctx, inc.ID, m.clock.Now()); err != nil {
				return err
			}
			continue
		}
		e.mu.Lock()
		if e.incident == nil {
			inc := inc
			e.incident = &inc
			e.tracker.Restore(inc.Status, inc.Started)
		}
		e.mu.Unlock()
	}
	return nil
}

// Run executes one probe for check id and records it.
func (m *Monitor) Run(ctx context.Context, id string, c check.Checker) {
	r := c.Check(ctx)
	if err := m.Record(ctx, r); err != nil {
		m.log.Error("record result", "check", id, "err", err)
	}
}

// Record stores a result, advances the state machine and, on a confirmed
// transition, opens/escalates/closes the incident and sends one alert.
func (m *Monitor) Record(ctx context.Context, r check.Result) error {
	e, ok := m.entries[r.CheckID]
	if !ok {
		return fmt.Errorf("unknown check %q", r.CheckID)
	}
	// Persisting and the transition must not be cut short by shutdown
	// half-way, so detach from the caller's cancellation.
	ctx = context.WithoutCancel(ctx)

	e.mu.Lock()
	defer e.mu.Unlock()

	if err := m.store.SaveResult(ctx, r); err != nil {
		return fmt.Errorf("save result: %w", err)
	}
	last := r
	e.last = &last

	tr, changed := e.tracker.Observe(r.Status, r.At)
	if !changed {
		return nil
	}
	m.log.Info("state change", "check", r.CheckID, "from", tr.From, "to", tr.To, "detail", r.Message)

	ev := notify.Event{
		CheckID:   e.spec.ID,
		CheckName: e.spec.Name,
		Target:    e.spec.Target,
		From:      tr.From,
		To:        tr.To,
		At:        tr.Since,
		Message:   r.Message,
	}

	var err error
	switch tr.To {
	case check.StatusUp:
		if e.incident != nil {
			ev.IncidentID = e.incident.ID
			ev.Downtime = tr.Since.Sub(e.incident.Started)
			err = m.store.CloseIncident(ctx, e.incident.ID, tr.Since)
			e.incident = nil
		}
	case check.StatusDegraded, check.StatusDown:
		if e.incident == nil {
			inc := &store.Incident{CheckID: e.spec.ID, Status: tr.To, Cause: r.Message, Started: tr.Since}
			err = m.store.OpenIncident(ctx, inc)
			e.incident = inc
		} else if tr.To.Severity() > e.incident.Status.Severity() {
			err = m.store.EscalateIncident(ctx, e.incident.ID, tr.To)
			e.incident.Status = tr.To
		}
		ev.IncidentID = e.incident.ID
	}
	if err != nil {
		err = fmt.Errorf("update incident: %w", err)
	}

	// The first successful probe after start-up is not news.
	if tr.From == check.StatusUnknown && tr.To == check.StatusUp {
		return err
	}
	nctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if nerr := m.notifier.Notify(nctx, ev); nerr != nil {
		m.log.Warn("notify failed", "check", r.CheckID, "err", nerr)
	}
	return err
}

// Snapshot returns the current view of one check.
func (m *Monitor) Snapshot(id string) (Snapshot, bool) {
	e, ok := m.entries[id]
	if !ok {
		return Snapshot{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st, since := e.tracker.State()
	s := Snapshot{Spec: e.spec, Status: st, Since: since}
	if e.last != nil {
		l := *e.last
		s.Last = &l
	}
	if e.incident != nil {
		s.IncidentID = e.incident.ID
	}
	return s, true
}

// Snapshots returns every check in configuration order.
func (m *Monitor) Snapshots() []Snapshot {
	out := make([]Snapshot, 0, len(m.order))
	for _, id := range m.order {
		s, _ := m.Snapshot(id)
		out = append(out, s)
	}
	return out
}

// Overall summarises snapshots into the worst confirmed status.
func Overall(snaps []Snapshot) check.Status {
	worst := check.StatusUp
	known := false
	for _, s := range snaps {
		if s.Status == check.StatusUnknown {
			continue
		}
		known = true
		if s.Status.Severity() > worst.Severity() {
			worst = s.Status
		}
	}
	if !known {
		return check.StatusUnknown
	}
	return worst
}
