// Package config loads and validates the YAML configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
)

// Config is the root of the YAML file.
type Config struct {
	Listen    string     `yaml:"listen"`
	Database  string     `yaml:"database"`
	Workers   int        `yaml:"workers"`
	Retention Duration   `yaml:"retention"`
	Page      Page       `yaml:"status_page"`
	Defaults  Defaults   `yaml:"defaults"`
	Notifiers []Notifier `yaml:"notifiers"`
	Checks    []Check    `yaml:"checks"`
}

// Page configures the public status page.
type Page struct {
	Title string `yaml:"title"`
}

// Defaults apply to every check that does not override them.
type Defaults struct {
	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	Jitter           float64  `yaml:"jitter"`
	FailThreshold    int      `yaml:"fail_threshold"`
	RecoverThreshold int      `yaml:"recover_threshold"`
}

// Notifier is one alert destination.
type Notifier struct {
	Type   string `yaml:"type"` // webhook | slack
	URL    string `yaml:"url"`
	Secret string `yaml:"secret"`
}

// Check is one monitored target.
type Check struct {
	ID               string    `yaml:"id"`
	Name             string    `yaml:"name"`
	Type             string    `yaml:"type"`
	Target           string    `yaml:"target"`
	Interval         Duration  `yaml:"interval"`
	Timeout          Duration  `yaml:"timeout"`
	Jitter           *float64  `yaml:"jitter"`
	FailThreshold    int       `yaml:"fail_threshold"`
	RecoverThreshold int       `yaml:"recover_threshold"`
	HTTP             HTTPCheck `yaml:"http"`
	DNS              DNSCheck  `yaml:"dns"`
}

// HTTPCheck holds HTTP-specific settings.
type HTTPCheck struct {
	Method             string            `yaml:"method"`
	ExpectStatus       []int             `yaml:"expect_status"` // [min, max] or [exact]
	BodyContains       string            `yaml:"body_contains"`
	LatencyBudget      Duration          `yaml:"latency_budget"`
	CertExpiryWarn     *Duration         `yaml:"cert_expiry_warn"`
	Headers            map[string]string `yaml:"headers"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify"`
}

// DNSCheck holds DNS-specific settings.
type DNSCheck struct {
	Record string `yaml:"record"`
	Expect string `yaml:"expect"`
}

// Duration accepts Go duration strings ("30s", "2h") plus a "d" day suffix
// ("90d").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// ParseDuration is time.ParseDuration with support for whole days ("7d").
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && fmt.Sprint(n) == days {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Load reads, expands ${ENV} references in, and validates the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes and validates YAML bytes. ${VAR} references are expanded
// from the environment so secrets need not live in the file.
func Parse(raw []byte) (*Config, error) {
	expanded := os.Expand(string(raw), os.Getenv)
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Database == "" {
		c.Database = "pulsewatch.db"
	}
	if c.Workers <= 0 {
		c.Workers = 8
	}
	if c.Retention == 0 {
		c.Retention = Duration(90 * 24 * time.Hour)
	}
	if c.Page.Title == "" {
		c.Page.Title = "Service status"
	}
	d := &c.Defaults
	if d.Interval == 0 {
		d.Interval = Duration(60 * time.Second)
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(10 * time.Second)
	}
	if d.Jitter == 0 {
		d.Jitter = 0.1
	}
	if d.FailThreshold == 0 {
		d.FailThreshold = 3
	}
	if d.RecoverThreshold == 0 {
		d.RecoverThreshold = 2
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.Defaults.Jitter < 0 || c.Defaults.Jitter > 0.5 {
		errs = append(errs, errors.New("defaults.jitter must be between 0 and 0.5"))
	}
	for i, n := range c.Notifiers {
		if n.Type != "webhook" && n.Type != "slack" {
			errs = append(errs, fmt.Errorf("notifiers[%d]: type must be webhook or slack", i))
		}
		if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			errs = append(errs, fmt.Errorf("notifiers[%d]: url must be http(s)", i))
		}
	}
	if len(c.Checks) == 0 {
		errs = append(errs, errors.New("at least one check is required"))
	}
	seen := map[string]bool{}
	for i, ch := range c.Checks {
		where := fmt.Sprintf("checks[%d] (%s)", i, ch.ID)
		if !idPattern.MatchString(ch.ID) {
			errs = append(errs, fmt.Errorf("%s: id must match %s", where, idPattern))
		}
		if seen[ch.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id", where))
		}
		seen[ch.ID] = true
		if ch.Target == "" {
			errs = append(errs, fmt.Errorf("%s: target is required", where))
		}
		switch check.Type(ch.Type) {
		case check.TypeHTTP:
			if u, err := url.Parse(ch.Target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("%s: http target must be an absolute http(s) URL", where))
			}
			if n := len(ch.HTTP.ExpectStatus); n > 2 {
				errs = append(errs, fmt.Errorf("%s: expect_status takes [code] or [min, max]", where))
			}
		case check.TypeTCP:
			if _, _, err := net.SplitHostPort(ch.Target); err != nil {
				errs = append(errs, fmt.Errorf("%s: tcp target must be host:port", where))
			}
		case check.TypeDNS:
		default:
			errs = append(errs, fmt.Errorf("%s: type must be http, tcp or dns", where))
		}
		if ch.Jitter != nil && (*ch.Jitter < 0 || *ch.Jitter > 0.5) {
			errs = append(errs, fmt.Errorf("%s: jitter must be between 0 and 0.5", where))
		}
		if ch.FailThreshold < 0 || ch.RecoverThreshold < 0 {
			errs = append(errs, fmt.Errorf("%s: thresholds must be positive", where))
		}
	}
	return errors.Join(errs...)
}

// Specs converts the checks into check.Specs with defaults applied.
func (c *Config) Specs() []check.Spec {
	out := make([]check.Spec, 0, len(c.Checks))
	for _, ch := range c.Checks {
		s := check.Spec{
			ID:               ch.ID,
			Name:             ch.Name,
			Type:             check.Type(ch.Type),
			Target:           ch.Target,
			Interval:         time.Duration(pick(ch.Interval, c.Defaults.Interval)),
			Timeout:          time.Duration(pick(ch.Timeout, c.Defaults.Timeout)),
			Jitter:           c.Defaults.Jitter,
			FailThreshold:    pick(ch.FailThreshold, c.Defaults.FailThreshold),
			RecoverThreshold: pick(ch.RecoverThreshold, c.Defaults.RecoverThreshold),
			DNS:              check.DNSOptions{Record: ch.DNS.Record, Expect: ch.DNS.Expect},
		}
		if s.Name == "" {
			s.Name = ch.ID
		}
		if ch.Jitter != nil {
			s.Jitter = *ch.Jitter
		}
		h := ch.HTTP
		s.HTTP = check.HTTPOptions{
			Method:             h.Method,
			BodyContains:       h.BodyContains,
			LatencyBudget:      time.Duration(h.LatencyBudget),
			CertExpiryWarn:     14 * 24 * time.Hour,
			Headers:            h.Headers,
			InsecureSkipVerify: h.InsecureSkipVerify,
		}
		if h.CertExpiryWarn != nil {
			s.HTTP.CertExpiryWarn = time.Duration(*h.CertExpiryWarn)
		}
		switch len(h.ExpectStatus) {
		case 1:
			s.HTTP.ExpectStatusMin, s.HTTP.ExpectStatusMax = h.ExpectStatus[0], h.ExpectStatus[0]
		case 2:
			s.HTTP.ExpectStatusMin, s.HTTP.ExpectStatusMax = h.ExpectStatus[0], h.ExpectStatus[1]
		}
		out = append(out, s)
	}
	return out
}

func pick[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
