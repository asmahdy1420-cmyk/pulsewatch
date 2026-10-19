package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func downEvent() Event {
	return Event{
		CheckID: "api", CheckName: "Public API", Target: "https://api.example.test",
		From: check.StatusUp, To: check.StatusDown, At: now, Message: "connection refused", IncidentID: 7,
	}
}

type captured struct {
	headers http.Header
	body    []byte
}

func captureServer(t *testing.T, status int) (*httptest.Server, <-chan captured) {
	t.Helper()
	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- captured{headers: r.Header.Clone(), body: b}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func TestWebhookSignsPayload(t *testing.T) {
	srv, got := captureServer(t, http.StatusNoContent)
	secret := "s3cret"
	w := NewWebhook(srv.URL, secret, clock.NewFake(now))
	if err := w.Notify(context.Background(), downEvent()); err != nil {
		t.Fatal(err)
	}
	req := <-got

	ts, err := strconv.ParseInt(req.headers.Get(HeaderTimestamp), 10, 64)
	if err != nil || ts != now.Unix() {
		t.Fatalf("timestamp header = %q", req.headers.Get(HeaderTimestamp))
	}
	sig := req.headers.Get(HeaderSignature)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("signature header = %q", sig)
	}
	if !Verify([]byte(secret), sig, ts, req.body, now, 5*time.Minute) {
		t.Fatal("receiver could not verify signature")
	}
	// Tampered body, wrong secret and stale timestamp must all fail.
	tampered := []byte(strings.Replace(string(req.body), "down", "up", 1))
	if Verify([]byte(secret), sig, ts, tampered, now, 5*time.Minute) {
		t.Fatal("tampered body verified")
	}
	if Verify([]byte("other"), sig, ts, req.body, now, 5*time.Minute) {
		t.Fatal("wrong secret verified")
	}
	if Verify([]byte(secret), sig, ts, req.body, now.Add(time.Hour), 5*time.Minute) {
		t.Fatal("replayed (stale) request verified")
	}

	var p map[string]any
	if err := json.Unmarshal(req.body, &p); err != nil {
		t.Fatal(err)
	}
	if p["check_id"] != "api" || p["to"] != "down" || p["summary"] != "DOWN: Public API - connection refused" {
		t.Fatalf("payload = %v", p)
	}
}

func TestWebhookWithoutSecretIsUnsigned(t *testing.T) {
	srv, got := captureServer(t, http.StatusOK)
	if err := NewWebhook(srv.URL, "", clock.NewFake(now)).Notify(context.Background(), downEvent()); err != nil {
		t.Fatal(err)
	}
	if h := (<-got).headers.Get(HeaderSignature); h != "" {
		t.Fatalf("unexpected signature %q", h)
	}
}

func TestWebhookReportsReceiverErrors(t *testing.T) {
	srv, _ := captureServer(t, http.StatusInternalServerError)
	err := NewWebhook(srv.URL, "x", clock.NewFake(now)).Notify(context.Background(), downEvent())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want status 500 error", err)
	}
}

func TestSlackFormat(t *testing.T) {
	srv, got := captureServer(t, http.StatusOK)
	rec := downEvent()
	rec.From, rec.To, rec.Downtime = check.StatusDown, check.StatusUp, 3*time.Minute+20*time.Second
	if err := NewSlack(srv.URL).Notify(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var p struct{ Text string }
	if err := json.Unmarshal((<-got).body, &p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{":large_green_circle:", "RECOVERED: Public API is up again after 3m20s", "down -> up"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("slack text %q missing %q", p.Text, want)
		}
	}
	if s := FormatSlack(downEvent()); !strings.HasPrefix(s, ":red_circle: *DOWN: Public API") {
		t.Errorf("down text = %q", s)
	}
}
