package config

import (
	"strings"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
)

const sample = `
listen: ":9090"
database: /var/lib/pulsewatch/pw.db
retention: 30d
status_page:
  title: Acme status
defaults:
  interval: 30s
  fail_threshold: 2
notifiers:
  - type: webhook
    url: https://hooks.example.test/pulsewatch
    secret: ${PW_TEST_SECRET}
checks:
  - id: api
    name: Public API
    type: http
    target: https://api.example.test/healthz
    interval: 15s
    jitter: 0.2
    http:
      expect_status: [200, 204]
      body_contains: ok
      latency_budget: 800ms
      cert_expiry_warn: 7d
  - id: db
    type: tcp
    target: db.internal:5432
    recover_threshold: 5
  - id: mx
    type: dns
    target: example.test
    dns:
      record: MX
      expect: mail
`

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	t.Setenv("PW_TEST_SECRET", "from-env")
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" || c.Workers != 8 || time.Duration(c.Retention) != 30*24*time.Hour {
		t.Fatalf("top-level = %+v", c)
	}
	if c.Notifiers[0].Secret != "from-env" {
		t.Fatalf("secret not expanded from environment: %q", c.Notifiers[0].Secret)
	}

	specs := c.Specs()
	api, db, mx := specs[0], specs[1], specs[2]
	if api.Interval != 15*time.Second || api.Jitter != 0.2 || api.FailThreshold != 2 || api.RecoverThreshold != 2 {
		t.Fatalf("api spec = %+v", api)
	}
	if api.HTTP.ExpectStatusMin != 200 || api.HTTP.ExpectStatusMax != 204 ||
		api.HTTP.LatencyBudget != 800*time.Millisecond || api.HTTP.CertExpiryWarn != 7*24*time.Hour {
		t.Fatalf("api http = %+v", api.HTTP)
	}
	if db.Name != "db" || db.Interval != 30*time.Second || db.Timeout != 10*time.Second || db.RecoverThreshold != 5 {
		t.Fatalf("db spec = %+v", db)
	}
	if db.HTTP.CertExpiryWarn != 14*24*time.Hour {
		t.Fatalf("default cert warning = %s", db.HTTP.CertExpiryWarn)
	}
	if mx.Type != check.TypeDNS || mx.DNS.Record != "MX" || mx.DNS.Expect != "mail" {
		t.Fatalf("mx spec = %+v", mx)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	bad := `
notifiers:
  - type: pager
    url: ftp://x
checks:
  - id: API
    type: http
    target: not-a-url
  - id: db
    type: tcp
    target: db.internal
  - id: db
    type: icmp
    target: 10.0.0.1
`
	_, err := Parse([]byte(bad))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"type must be webhook or slack",
		"url must be http(s)",
		"id must match",
		"absolute http(s) URL",
		"host:port",
		"duplicate id",
		"type must be http, tcp or dns",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("checks:\n  - id: a\n    type: tcp\n    target: a:1\n    intervall: 5s\n"))
	if err == nil || !strings.Contains(err.Error(), "intervall") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1h30m": 90 * time.Minute, "500ms": 500 * time.Millisecond} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ParseDuration("1.5d"); err == nil {
		t.Error("fractional days should be rejected")
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	t.Setenv("PULSEWATCH_WEBHOOK_SECRET", "example")
	c, err := Load("../../pulsewatch.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Specs()) != 4 || len(c.Notifiers) != 2 {
		t.Fatalf("example config: %d checks, %d notifiers", len(c.Specs()), len(c.Notifiers))
	}
}
