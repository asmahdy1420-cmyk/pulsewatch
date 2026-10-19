// Package notify delivers state-transition alerts. The monitor only calls a
// Notifier when a check's confirmed state changes, so implementations never
// need to de-duplicate.
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
)

// Event describes one confirmed state transition.
type Event struct {
	CheckID    string       `json:"check_id"`
	CheckName  string       `json:"check_name"`
	Target     string       `json:"target"`
	From       check.Status `json:"from"`
	To         check.Status `json:"to"`
	At         time.Time    `json:"at"`
	Message    string       `json:"message"`
	IncidentID int64        `json:"incident_id,omitempty"`
	// Downtime is set on recovery: how long the closed incident lasted.
	Downtime time.Duration `json:"-"`
}

// Recovery reports whether the event is a return to up.
func (e Event) Recovery() bool { return e.To == check.StatusUp }

// Summary is a one-line human description of the event.
func (e Event) Summary() string {
	if e.Recovery() {
		if e.Downtime > 0 {
			return fmt.Sprintf("RECOVERED: %s is up again after %s", e.CheckName, e.Downtime.Round(time.Second))
		}
		return fmt.Sprintf("RECOVERED: %s is up again", e.CheckName)
	}
	label := "DOWN"
	if e.To == check.StatusDegraded {
		label = "DEGRADED"
	}
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", label, e.CheckName)
	}
	return fmt.Sprintf("%s: %s - %s", label, e.CheckName, e.Message)
}

// Notifier sends an event somewhere.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Multi fans an event out to several notifiers. Every notifier is tried;
// errors are joined.
type Multi []Notifier

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, e Event) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
