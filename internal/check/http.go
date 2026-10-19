package check

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

// maxBodyBytes caps how much of a response body is read when searching for
// an expected substring.
const maxBodyBytes = 1 << 20

// HTTPOptions configures an HTTP(S) check.
type HTTPOptions struct {
	Method string
	// ExpectStatusMin and ExpectStatusMax form the inclusive range of
	// acceptable status codes. Zero values default to 200..399.
	ExpectStatusMin int
	ExpectStatusMax int
	// BodyContains, if set, must appear in the first 1 MiB of the body.
	BodyContains string
	// LatencyBudget marks the check degraded when exceeded. Zero disables it.
	LatencyBudget time.Duration
	// CertExpiryWarn marks the check degraded when the leaf certificate
	// expires within this window. Zero disables it.
	CertExpiryWarn time.Duration
	Headers        map[string]string
	// InsecureSkipVerify disables TLS verification. Intended for internal
	// targets with private CAs; expiry warnings still apply.
	InsecureSkipVerify bool
}

// HTTP probes a URL.
type HTTP struct {
	id     string
	url    string
	opts   HTTPOptions
	clk    clock.Clock
	client *http.Client
}

// NewHTTP returns an HTTP checker with its own transport.
func NewHTTP(id, url string, opts HTTPOptions, clk clock.Clock) *HTTP {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if opts.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per check
	}
	return &HTTP{id: id, url: url, opts: opts, clk: clk, client: &http.Client{Transport: tr}}
}

// WithClient replaces the underlying HTTP client (used in tests to trust a
// test server's certificate).
func (h *HTTP) WithClient(c *http.Client) *HTTP {
	h.client = c
	return h
}

// Check performs one request and classifies the response.
func (h *HTTP) Check(ctx context.Context) Result {
	p := startProbe(h.id, h.clk)

	method := h.opts.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url, nil)
	if err != nil {
		return p.done(StatusDown, "build request: "+err.Error())
	}
	req.Header.Set("User-Agent", "pulsewatch")
	for k, v := range h.opts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return p.done(StatusDown, err.Error())
	}
	defer resp.Body.Close()

	lo, hi := h.opts.ExpectStatusMin, h.opts.ExpectStatusMax
	if lo == 0 {
		lo = 200
	}
	if hi == 0 {
		hi = 399
	}
	if resp.StatusCode < lo || resp.StatusCode > hi {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		return p.done(StatusDown, fmt.Sprintf("unexpected status %d (want %d-%d)", resp.StatusCode, lo, hi))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return p.done(StatusDown, "read body: "+err.Error())
	}
	if h.opts.BodyContains != "" && !strings.Contains(string(body), h.opts.BodyContains) {
		return p.done(StatusDown, fmt.Sprintf("body does not contain %q", h.opts.BodyContains))
	}

	res := p.done(StatusUp, fmt.Sprintf("HTTP %d", resp.StatusCode))

	// Degradations: report the first that applies, certificate first since
	// it needs human action.
	if h.opts.CertExpiryWarn > 0 && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		left := resp.TLS.PeerCertificates[0].NotAfter.Sub(h.clk.Now())
		if left < h.opts.CertExpiryWarn {
			res.Status = StatusDegraded
			res.Message = fmt.Sprintf("TLS certificate expires in %s", humanDuration(left))
			return res
		}
	}
	if h.opts.LatencyBudget > 0 && res.Latency > h.opts.LatencyBudget {
		res.Status = StatusDegraded
		res.Message = fmt.Sprintf("latency %s over budget %s", res.Latency.Round(time.Millisecond), h.opts.LatencyBudget)
	}
	return res
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s (expired)"
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return d.Round(time.Minute).String()
}
