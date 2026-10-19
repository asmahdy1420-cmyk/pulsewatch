package check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func TestHTTPChecker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"healthy"}`)) })
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/teapot", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		name    string
		path    string
		opts    HTTPOptions
		want    Status
		wantMsg string
	}{
		{"up", "/ok", HTTPOptions{}, StatusUp, "HTTP 200"},
		{"body match", "/ok", HTTPOptions{BodyContains: "healthy"}, StatusUp, "HTTP 200"},
		{"body mismatch", "/ok", HTTPOptions{BodyContains: "ready"}, StatusDown, `does not contain "ready"`},
		{"bad status", "/fail", HTTPOptions{}, StatusDown, "unexpected status 503"},
		{"custom status range", "/teapot", HTTPOptions{ExpectStatusMin: 418, ExpectStatusMax: 418}, StatusUp, "HTTP 418"},
		{"latency over budget", "/slow", HTTPOptions{LatencyBudget: 10 * time.Millisecond}, StatusDegraded, "over budget"},
		{"latency within budget", "/slow", HTTPOptions{LatencyBudget: 5 * time.Second}, StatusUp, "HTTP 200"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewHTTP("web", srv.URL+tc.path, tc.opts, clock.NewFake(t0))
			r := c.Check(context.Background())
			if r.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", r.Status, r.Message, tc.want)
			}
			if !strings.Contains(r.Message, tc.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", r.Message, tc.wantMsg)
			}
			if r.CheckID != "web" || !r.At.Equal(t0) {
				t.Fatalf("result identity = %q at %v", r.CheckID, r.At)
			}
		})
	}
}

func TestHTTPCheckerConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r := NewHTTP("gone", url, HTTPOptions{}, clock.NewFake(t0)).Check(context.Background())
	if r.Status != StatusDown {
		t.Fatalf("status = %s, want down", r.Status)
	}
}

func TestHTTPCheckerTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := NewHTTP("hang", srv.URL, HTTPOptions{}, clock.NewFake(t0)).Check(ctx)
	if r.Status != StatusDown || !strings.Contains(r.Message, "deadline") {
		t.Fatalf("got %s %q, want down with deadline error", r.Status, r.Message)
	}
}

func TestHTTPCheckerCertificateExpiry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	notAfter := srv.Certificate().NotAfter

	// Pretend "now" is three days before the test certificate expires.
	clk := clock.NewFake(notAfter.Add(-72 * time.Hour))
	opts := HTTPOptions{CertExpiryWarn: 14 * 24 * time.Hour}
	r := NewHTTP("tls", srv.URL, opts, clk).WithClient(srv.Client()).Check(context.Background())
	if r.Status != StatusDegraded || !strings.Contains(r.Message, "expires in 3d") {
		t.Fatalf("got %s %q, want degraded with expiry warning", r.Status, r.Message)
	}

	// Far from expiry: healthy.
	clk.Set(notAfter.Add(-365 * 24 * time.Hour))
	r = NewHTTP("tls", srv.URL, opts, clk).WithClient(srv.Client()).Check(context.Background())
	if r.Status != StatusUp {
		t.Fatalf("got %s %q, want up", r.Status, r.Message)
	}
}

func TestTCPChecker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr := ln.Addr().String()

	c := NewTCP("db", addr, clock.NewFake(t0))
	if r := c.Check(context.Background()); r.Status != StatusUp {
		t.Fatalf("open port: status = %s (%s), want up", r.Status, r.Message)
	}

	_ = ln.Close()
	if r := c.Check(context.Background()); r.Status != StatusDown {
		t.Fatalf("closed port: status = %s, want down", r.Status)
	}
}

type fakeResolver struct {
	hosts map[string][]string
	txt   map[string][]string
}

func (f fakeResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if a, ok := f.hosts[h]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
}

func (f fakeResolver) LookupCNAME(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}

func (f fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	return nil, errors.New("not implemented")
}

func (f fakeResolver) LookupTXT(_ context.Context, h string) ([]string, error) {
	return f.txt[h], nil
}

func TestDNSChecker(t *testing.T) {
	r := fakeResolver{
		hosts: map[string][]string{"api.example.test": {"192.0.2.10", "192.0.2.11"}},
		txt:   map[string][]string{"example.test": {"v=spf1 -all"}},
	}
	clk := clock.NewFake(t0)
	tests := []struct {
		name string
		host string
		opts DNSOptions
		want Status
	}{
		{"resolves", "api.example.test", DNSOptions{}, StatusUp},
		{"expected address", "api.example.test", DNSOptions{Expect: "192.0.2.11"}, StatusUp},
		{"unexpected address", "api.example.test", DNSOptions{Expect: "198.51.100.1"}, StatusDown},
		{"nxdomain", "missing.example.test", DNSOptions{}, StatusDown},
		{"txt record", "example.test", DNSOptions{Record: "TXT", Expect: "spf1"}, StatusUp},
		{"no txt answers", "other.test", DNSOptions{Record: "TXT"}, StatusDown},
		{"bad record type", "example.test", DNSOptions{Record: "SRV"}, StatusDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewDNS("dns", tc.host, tc.opts, r, clk).Check(context.Background())
			if got.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", got.Status, got.Message, tc.want)
			}
		})
	}
}

func TestNewRejectsUnknownType(t *testing.T) {
	if _, err := New(Spec{ID: "x", Type: "icmp"}, clock.Real{}); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}
