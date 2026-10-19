package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
)

// Header names used by the signed webhook.
const (
	HeaderSignature = "X-Pulsewatch-Signature"
	HeaderTimestamp = "X-Pulsewatch-Timestamp"
)

// Sign computes the webhook signature: hex(HMAC-SHA256(secret,
// "<unix timestamp>.<body>")), prefixed with "sha256=". Binding the
// timestamp into the MAC lets receivers reject replays.
func Sign(secret []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature in constant time and rejects timestamps more
// than tolerance away from now.
func Verify(secret []byte, sig string, ts int64, body []byte, now time.Time, tolerance time.Duration) bool {
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(Sign(secret, ts, body)))
}

// WebhookPayload is the JSON body sent by Webhook.
type WebhookPayload struct {
	Event
	Summary         string  `json:"summary"`
	DowntimeSeconds float64 `json:"downtime_seconds,omitempty"`
}

// Webhook POSTs a JSON payload, signed with HMAC-SHA256 when a secret is
// configured.
type Webhook struct {
	URL    string
	Secret []byte
	Client *http.Client
	Clock  clock.Clock
}

// NewWebhook returns a Webhook with a 10s client timeout.
func NewWebhook(url, secret string, clk clock.Clock) *Webhook {
	return &Webhook{URL: url, Secret: []byte(secret), Client: &http.Client{Timeout: 10 * time.Second}, Clock: clk}
}

// Notify implements Notifier.
func (w *Webhook) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(WebhookPayload{Event: e, Summary: e.Summary(), DowntimeSeconds: e.Downtime.Seconds()})
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if len(w.Secret) > 0 {
		ts := w.Clock.Now().Unix()
		headers[HeaderTimestamp] = strconv.FormatInt(ts, 10)
		headers[HeaderSignature] = Sign(w.Secret, ts, body)
	}
	return postJSON(ctx, w.Client, w.URL, body, headers)
}

// Slack posts a Slack-compatible {"text": ...} message to an incoming
// webhook URL. Mattermost, Rocket.Chat and Discord's /slack endpoint accept
// the same shape.
type Slack struct {
	URL    string
	Client *http.Client
}

// NewSlack returns a Slack notifier with a 10s client timeout.
func NewSlack(url string) *Slack {
	return &Slack{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}

// FormatSlack renders the message text for an event.
func FormatSlack(e Event) string {
	icon := ":red_circle:"
	switch e.To {
	case check.StatusUp:
		icon = ":large_green_circle:"
	case check.StatusDegraded:
		icon = ":large_yellow_circle:"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s*", icon, e.Summary())
	fmt.Fprintf(&b, "\n`%s` %s -> %s at %s", e.Target, e.From, e.To, e.At.UTC().Format(time.RFC3339))
	return b.String()
}

// Notify implements Notifier.
func (s *Slack) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(map[string]string{"text": FormatSlack(e)})
	if err != nil {
		return err
	}
	return postJSON(ctx, s.Client, s.URL, body, nil)
}

func postJSON(ctx context.Context, c *http.Client, url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pulsewatch")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify %s: status %d", url, resp.StatusCode)
	}
	return nil
}
