package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/uptime"
)

// Memory is an in-process Store. It is safe for concurrent use and intended
// for tests and ephemeral runs.
type Memory struct {
	mu        sync.RWMutex
	results   map[string][]check.Result // per check, in insertion order
	incidents []Incident
	nextID    int64
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory {
	return &Memory{results: make(map[string][]check.Result)}
}

var _ Store = (*Memory)(nil)

// SaveResult implements Store.
func (m *Memory) SaveResult(_ context.Context, r check.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[r.CheckID] = append(m.results[r.CheckID], r)
	return nil
}

// History implements Store.
func (m *Memory) History(_ context.Context, checkID string, limit int) ([]check.Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rs := m.results[checkID]
	out := make([]check.Result, 0, min(limit, len(rs)))
	for i := len(rs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, rs[i])
	}
	return out, nil
}

func inWindow(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }

func count(t *uptime.Tally, s check.Status) {
	switch s {
	case check.StatusUp:
		t.Up++
	case check.StatusDegraded:
		t.Degraded++
	case check.StatusDown:
		t.Down++
	}
}

// Tally implements Store.
func (m *Memory) Tally(_ context.Context, checkID string, from, to time.Time) (uptime.Tally, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var t uptime.Tally
	for _, r := range m.results[checkID] {
		if inWindow(r.At, from, to) {
			count(&t, r.Status)
		}
	}
	return t, nil
}

// Daily implements Store.
func (m *Memory) Daily(_ context.Context, checkID string, from, to time.Time) ([]uptime.Day, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byDay := map[time.Time]*uptime.Tally{}
	for _, r := range m.results[checkID] {
		if !inWindow(r.At, from, to) {
			continue
		}
		d := uptime.StartOfDay(r.At)
		if byDay[d] == nil {
			byDay[d] = &uptime.Tally{}
		}
		count(byDay[d], r.Status)
	}
	out := make([]uptime.Day, 0, len(byDay))
	for d, t := range byDay {
		out = append(out, uptime.Day{Day: d, Tally: *t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}

// OpenIncident implements Store.
func (m *Memory) OpenIncident(_ context.Context, inc *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	inc.ID = m.nextID
	m.incidents = append(m.incidents, *inc)
	return nil
}

func (m *Memory) find(id int64) *Incident {
	for i := range m.incidents {
		if m.incidents[i].ID == id {
			return &m.incidents[i]
		}
	}
	return nil
}

// EscalateIncident implements Store.
func (m *Memory) EscalateIncident(_ context.Context, id int64, s check.Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inc := m.find(id)
	if inc == nil {
		return ErrNotFound
	}
	if s.Severity() > inc.Status.Severity() {
		inc.Status = s
	}
	return nil
}

// CloseIncident implements Store.
func (m *Memory) CloseIncident(_ context.Context, id int64, ended time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inc := m.find(id)
	if inc == nil {
		return ErrNotFound
	}
	inc.Ended = &ended
	return nil
}

// ListIncidents implements Store.
func (m *Memory) ListIncidents(_ context.Context, f IncidentFilter) ([]Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Incident
	for i := len(m.incidents) - 1; i >= 0; i-- {
		inc := m.incidents[i]
		if f.CheckID != "" && inc.CheckID != f.CheckID {
			continue
		}
		if f.OpenOnly && !inc.Open() {
			continue
		}
		if inc.Ended != nil {
			e := *inc.Ended
			inc.Ended = &e
		}
		out = append(out, inc)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// Prune implements Store.
func (m *Memory) Prune(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, rs := range m.results {
		kept := rs[:0]
		for _, r := range rs {
			if r.At.Before(before) {
				n++
				continue
			}
			kept = append(kept, r)
		}
		m.results[id] = kept
	}
	return n, nil
}

// Close implements Store.
func (m *Memory) Close() error { return nil }
