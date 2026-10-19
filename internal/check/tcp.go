package check

import (
	"context"
	"net"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

// TCP checks that a TCP connection to host:port can be established.
type TCP struct {
	id     string
	addr   string
	clk    clock.Clock
	dialer net.Dialer
}

// NewTCP returns a TCP checker for addr ("host:port").
func NewTCP(id, addr string, clk clock.Clock) *TCP {
	return &TCP{id: id, addr: addr, clk: clk}
}

// Check dials the address and closes the connection immediately.
func (t *TCP) Check(ctx context.Context) Result {
	p := startProbe(t.id, t.clk)
	conn, err := t.dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return p.done(StatusDown, err.Error())
	}
	_ = conn.Close()
	return p.done(StatusUp, "connected")
}
