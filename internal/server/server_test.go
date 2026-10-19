package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/monitor"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(now.Add(-2 * time.Hour))
	st := store.NewMemory()
	specs := []check.Spec{
		{ID: "api", Name: "Public API", Type: check.TypeHTTP, Target: "https://api.example.test", Interval: time.Minute, FailThreshold: 1, RecoverThreshold: 1},
		{ID: "db", Name: "Database", Type: check.TypeTCP, Target: "db:5432", Interval: time.Minute, FailThreshold: 1, RecoverThreshold: 1},
	}
	mon := monitor.New(specs, st, nil, clk, nil)

	// api: up, down, down, up -> one closed incident lasting 2 minutes.
	// db: three ups.
	for i, s := range []check.Status{check.StatusUp, check.StatusDown, check.StatusDown, check.StatusUp} {
		at := clk.Now().Add(time.Duration(i) * time.Minute)
		_ = mon.Record(ctx, check.Result{CheckID: "api", At: at, Status: s, Latency: 42 * time.Millisecond, Message: "probe " + string(s)})
	}
	for i := range 3 {
		at := clk.Now().Add(time.Duration(i) * time.Minute)
		_ = mon.Record(ctx, check.Result{CheckID: "db", At: at, Status: check.StatusUp, Message: "connected"})
	}
	clk.Set(now)

	srv, err := New(mon, st, clk, "Example status", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string, wantCode int, into any) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantCode {
		t.Fatalf("GET %s = %d, want %d: %s", url, resp.StatusCode, wantCode, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return string(body)
}

func TestAPIChecks(t *testing.T) {
	ts := newTestServer(t)
	var out struct {
		Checks []struct {
			ID         string              `json:"id"`
			Status     string              `json:"status"`
			Uptime     map[string]*float64 `json:"uptime"`
			LastResult *struct {
				LatencyMS float64 `json:"latency_ms"`
			} `json:"last_result"`
		} `json:"checks"`
	}
	get(t, ts.URL+"/api/checks", http.StatusOK, &out)
	if len(out.Checks) != 2 || out.Checks[0].ID != "api" || out.Checks[1].ID != "db" {
		t.Fatalf("checks = %+v, want config order", out.Checks)
	}
	api := out.Checks[0]
	if api.Status != "up" || api.LastResult == nil || api.LastResult.LatencyMS != 42 {
		t.Fatalf("api = %+v", api)
	}
	if u := api.Uptime["24h"]; u == nil || *u != 50 {
		t.Fatalf("api 24h uptime = %v, want 50", u)
	}

	var one struct{ ID, Name string }
	get(t, ts.URL+"/api/checks/db", http.StatusOK, &one)
	if one.Name != "Database" {
		t.Fatalf("check = %+v", one)
	}
	get(t, ts.URL+"/api/checks/nope", http.StatusNotFound, nil)
}

func TestAPIHistory(t *testing.T) {
	ts := newTestServer(t)
	var out struct {
		Results []struct{ Status string } `json:"results"`
	}
	get(t, ts.URL+"/api/checks/api/history?limit=3", http.StatusOK, &out)
	if len(out.Results) != 3 || out.Results[0].Status != "up" || out.Results[1].Status != "down" {
		t.Fatalf("history = %+v, want newest first", out.Results)
	}
	get(t, ts.URL+"/api/checks/api/history?limit=0", http.StatusBadRequest, nil)
	get(t, ts.URL+"/api/checks/api/history?limit=abc", http.StatusBadRequest, nil)
	get(t, ts.URL+"/api/checks/nope/history", http.StatusNotFound, nil)
}

func TestAPIIncidents(t *testing.T) {
	ts := newTestServer(t)
	var out struct {
		Incidents []struct {
			CheckID         string  `json:"check_id"`
			CheckName       string  `json:"check_name"`
			Open            bool    `json:"open"`
			DurationSeconds float64 `json:"duration_seconds"`
		} `json:"incidents"`
	}
	get(t, ts.URL+"/api/incidents", http.StatusOK, &out)
	if len(out.Incidents) != 1 {
		t.Fatalf("incidents = %+v", out.Incidents)
	}
	inc := out.Incidents[0]
	if inc.CheckID != "api" || inc.CheckName != "Public API" || inc.Open || inc.DurationSeconds != 120 {
		t.Fatalf("incident = %+v", inc)
	}

	out.Incidents = nil
	get(t, ts.URL+"/api/incidents?open=true", http.StatusOK, &out)
	if len(out.Incidents) != 0 {
		t.Fatalf("open incidents = %+v, want none", out.Incidents)
	}
	get(t, ts.URL+"/api/incidents?open=maybe", http.StatusBadRequest, nil)
}

func TestStatusPage(t *testing.T) {
	ts := newTestServer(t)
	html := get(t, ts.URL+"/", http.StatusOK, nil)
	for _, want := range []string{
		"<title>Example status</title>",
		"All systems operational",
		"Public API",
		"Database",
		"50.00% uptime",
		"Recent incidents",
		`class="bar bar-down"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("status page missing %q", want)
		}
	}
	if n := strings.Count(html, `class="bar bar-`); n != 2*BarDays {
		t.Errorf("rendered %d bars, want %d", n, 2*BarDays)
	}

	css := get(t, ts.URL+"/static/style.css", http.StatusOK, nil)
	if !strings.Contains(css, ".bar-ok") {
		t.Error("embedded stylesheet not served")
	}
	get(t, ts.URL+"/does-not-exist", http.StatusNotFound, nil)
}
