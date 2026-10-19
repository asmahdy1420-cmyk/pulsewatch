package check

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

// DNSOptions configures a DNS check.
type DNSOptions struct {
	// Record is one of A (default; any address), CNAME, MX or TXT.
	Record string
	// Expect, if set, must be a substring of at least one returned answer.
	Expect string
}

// Resolver is the subset of *net.Resolver used by the DNS checker.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DNS checks that a name resolves, optionally to an expected answer.
type DNS struct {
	id       string
	host     string
	opts     DNSOptions
	resolver Resolver
	clk      clock.Clock
}

// NewDNS returns a DNS checker. A nil resolver uses net.DefaultResolver.
func NewDNS(id, host string, opts DNSOptions, r Resolver, clk clock.Clock) *DNS {
	if r == nil {
		r = net.DefaultResolver
	}
	return &DNS{id: id, host: host, opts: opts, resolver: r, clk: clk}
}

// Check performs the lookup.
func (d *DNS) Check(ctx context.Context) Result {
	p := startProbe(d.id, d.clk)
	answers, err := d.lookup(ctx)
	if err != nil {
		return p.done(StatusDown, err.Error())
	}
	if len(answers) == 0 {
		return p.done(StatusDown, "no answers")
	}
	if d.opts.Expect != "" {
		found := false
		for _, a := range answers {
			if strings.Contains(a, d.opts.Expect) {
				found = true
				break
			}
		}
		if !found {
			return p.done(StatusDown, fmt.Sprintf("no answer contains %q (got %s)", d.opts.Expect, strings.Join(answers, ", ")))
		}
	}
	return p.done(StatusUp, strings.Join(answers, ", "))
}

func (d *DNS) lookup(ctx context.Context) ([]string, error) {
	switch strings.ToUpper(d.opts.Record) {
	case "", "A", "AAAA":
		return d.resolver.LookupHost(ctx, d.host)
	case "CNAME":
		c, err := d.resolver.LookupCNAME(ctx, d.host)
		if err != nil {
			return nil, err
		}
		return []string{c}, nil
	case "MX":
		mxs, err := d.resolver.LookupMX(ctx, d.host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			out = append(out, fmt.Sprintf("%s (%d)", mx.Host, mx.Pref))
		}
		return out, nil
	case "TXT":
		return d.resolver.LookupTXT(ctx, d.host)
	default:
		return nil, fmt.Errorf("unsupported record type %q", d.opts.Record)
	}
}
