// Package store persists probe results and incidents. Store is the
// interface the rest of the system depends on; SQLite backs production and
// Memory backs tests.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/uptime"
)

// ErrNotFound is returned when a referenced record does not exist.
var ErrNotFound = errors.New("not found")

// Incident is a period during which a check was degraded or down.
type Incident struct {
	ID      int64        `json:"id"`
	CheckID string       `json:"check_id"`
	Status  check.Status `json:"status"` // worst status seen during the incident
	Cause   string       `json:"cause"`
	Started time.Time    `json:"started_at"`
	Ended   *time.Time   `json:"ended_at,omitempty"`
}

// Open reports whether the incident is still ongoing.
func (i Incident) Open() bool { return i.Ended == nil }

// Duration is the length of the incident; for an open incident it is
// measured up to now.
func (i Incident) Duration(now time.Time) time.Duration {
	if i.Ended != nil {
		return i.Ended.Sub(i.Started)
	}
	return now.Sub(i.Started)
}

// IncidentFilter narrows ListIncidents. Zero values mean "no filter".
type IncidentFilter struct {
	CheckID  string
	OpenOnly bool
	Limit    int
}

// Store is the persistence boundary.
type Store interface {
	// SaveResult appends one probe result.
	SaveResult(ctx context.Context, r check.Result) error
	// History returns the most recent results for a check, newest first.
	History(ctx context.Context, checkID string, limit int) ([]check.Result, error)
	// Tally counts results for a check in [from, to).
	Tally(ctx context.Context, checkID string, from, to time.Time) (uptime.Tally, error)
	// Daily counts results per UTC day in [from, to), oldest first. Days
	// without results are omitted.
	Daily(ctx context.Context, checkID string, from, to time.Time) ([]uptime.Day, error)

	// OpenIncident inserts inc and sets its ID.
	OpenIncident(ctx context.Context, inc *Incident) error
	// EscalateIncident raises the recorded status of an open incident.
	EscalateIncident(ctx context.Context, id int64, s check.Status) error
	// CloseIncident sets the end time of an incident.
	CloseIncident(ctx context.Context, id int64, ended time.Time) error
	// ListIncidents returns incidents newest first.
	ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error)

	// Prune deletes results older than before and returns how many.
	Prune(ctx context.Context, before time.Time) (int64, error)
	Close() error
}
