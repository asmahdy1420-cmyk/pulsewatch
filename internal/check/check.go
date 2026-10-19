// Package check defines monitored targets, probe results and the HTTP, TCP
// and DNS checkers that produce them.
package check

import (
	"context"
	"fmt"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

// Status is the health of a check, either as observed by a single probe or
// as the confirmed state of the check.
type Status string

// Known statuses. StatusUnknown is only ever a confirmed state (before the
// first probe completes); probes always report up, degraded or down.
const (
	StatusUnknown  Status = "unknown"
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// Severity orders statuses from healthy to failing. Unknown sorts lowest.
func (s Status) Severity() int {
	switch s {
	case StatusUp:
		return 1
	case StatusDegraded:
		return 2
	case StatusDown:
		return 3
	default:
		return 0
	}
}

// Type identifies the kind of probe.
type Type string

// Supported check types.
const (
	TypeHTTP Type = "http"
	TypeTCP  Type = "tcp"
	TypeDNS  Type = "dns"
)

// Spec describes one monitored target.
type Spec struct {
	ID       string
	Name     string
	Type     Type
	Target   string
	Interval time.Duration
	Timeout  time.Duration
	// Jitter is the fraction (0..0.5) by which each interval is randomly
	// stretched or shrunk, so checks do not fire in lockstep.
	Jitter float64
	// FailThreshold is the number of consecutive bad probes needed before
	// the check is declared degraded or down.
	FailThreshold int
	// RecoverThreshold is the number of consecutive better probes needed
	// before the check is declared recovered.
	RecoverThreshold int

	HTTP HTTPOptions
	DNS  DNSOptions
}

// Result is the outcome of a single probe.
type Result struct {
	CheckID string        `json:"check_id"`
	At      time.Time     `json:"at"`
	Status  Status        `json:"status"`
	Latency time.Duration `json:"-"`
	Message string        `json:"message,omitempty"`
}

// Checker runs one probe. Implementations must honour ctx cancellation and
// never panic on network errors; failures are reported as StatusDown.
type Checker interface {
	Check(ctx context.Context) Result
}

// New builds the Checker for spec. clk is used for timestamps and for TLS
// certificate expiry calculations.
func New(spec Spec, clk clock.Clock) (Checker, error) {
	switch spec.Type {
	case TypeHTTP:
		return NewHTTP(spec.ID, spec.Target, spec.HTTP, clk), nil
	case TypeTCP:
		return NewTCP(spec.ID, spec.Target, clk), nil
	case TypeDNS:
		return NewDNS(spec.ID, spec.Target, spec.DNS, nil, clk), nil
	default:
		return nil, fmt.Errorf("check %q: unsupported type %q", spec.ID, spec.Type)
	}
}

// probe captures the timestamp (from the injectable clock) and the
// monotonic start time (for latency) of a single probe.
type probe struct {
	id    string
	at    time.Time
	began time.Time
}

func startProbe(id string, clk clock.Clock) probe {
	return probe{id: id, at: clk.Now(), began: time.Now()}
}

func (p probe) done(s Status, msg string) Result {
	return Result{CheckID: p.id, At: p.at, Status: s, Latency: time.Since(p.began), Message: msg}
}
