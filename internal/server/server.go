// Package server exposes the JSON API and the server-rendered status page.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/clock"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/monitor"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
)

//go:embed web
var webFS embed.FS

// Monitor is the read side of monitor.Monitor used by the server.
type Monitor interface {
	Snapshots() []monitor.Snapshot
	Snapshot(id string) (monitor.Snapshot, bool)
}

// Server serves HTTP.
type Server struct {
	mon   Monitor
	store store.Store
	clock clock.Clock
	title string
	log   *slog.Logger
	page  *template.Template
}

// New builds a Server.
func New(mon Monitor, st store.Store, clk clock.Clock, title string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tmpl, err := template.New("status.html").Funcs(funcs).ParseFS(webFS, "web/status.html")
	if err != nil {
		return nil, err
	}
	return &Server{mon: mon, store: st, clock: clk, title: title, log: log, page: tmpl}, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(webFS, "web/static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/checks", s.handleChecks)
	mux.HandleFunc("GET /api/checks/{id}", s.handleCheck)
	mux.HandleFunc("GET /api/checks/{id}/history", s.handleHistory)
	mux.HandleFunc("GET /api/incidents", s.handleIncidents)
	return s.logRequests(mux)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "dur", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ---- JSON API ---------------------------------------------------------------

type resultJSON struct {
	At        time.Time    `json:"at"`
	Status    check.Status `json:"status"`
	LatencyMS float64      `json:"latency_ms"`
	Message   string       `json:"message,omitempty"`
}

func toResultJSON(r check.Result) resultJSON {
	return resultJSON{At: r.At, Status: r.Status, LatencyMS: float64(r.Latency.Microseconds()) / 1000, Message: r.Message}
}

type checkJSON struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Type            check.Type          `json:"type"`
	Target          string              `json:"target"`
	IntervalSeconds float64             `json:"interval_seconds"`
	Status          check.Status        `json:"status"`
	Since           *time.Time          `json:"since,omitempty"`
	LastResult      *resultJSON         `json:"last_result,omitempty"`
	OpenIncidentID  int64               `json:"open_incident_id,omitempty"`
	Uptime          map[string]*float64 `json:"uptime"`
}

var uptimeWindows = []struct {
	label string
	d     time.Duration
}{
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"90d", 90 * 24 * time.Hour},
}

func (s *Server) checkJSON(ctx context.Context, snap monitor.Snapshot) (checkJSON, error) {
	c := checkJSON{
		ID:              snap.Spec.ID,
		Name:            snap.Spec.Name,
		Type:            snap.Spec.Type,
		Target:          snap.Spec.Target,
		IntervalSeconds: snap.Spec.Interval.Seconds(),
		Status:          snap.Status,
		OpenIncidentID:  snap.IncidentID,
		Uptime:          map[string]*float64{},
	}
	if !snap.Since.IsZero() {
		since := snap.Since
		c.Since = &since
	}
	if snap.Last != nil {
		r := toResultJSON(*snap.Last)
		c.LastResult = &r
	}
	now := s.clock.Now()
	for _, w := range uptimeWindows {
		t, err := s.store.Tally(ctx, snap.Spec.ID, now.Add(-w.d), now.Add(time.Nanosecond))
		if err != nil {
			return c, err
		}
		if pct, ok := t.Percent(); ok {
			c.Uptime[w.label] = &pct
		} else {
			c.Uptime[w.label] = nil
		}
	}
	return c, nil
}

func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	snaps := s.mon.Snapshots()
	out := make([]checkJSON, 0, len(snaps))
	for _, snap := range snaps {
		c, err := s.checkJSON(r.Context(), snap)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out})
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.mon.Snapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "check not found")
		return
	}
	c, err := s.checkJSON(r.Context(), snap)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.mon.Snapshot(id); !ok {
		writeError(w, http.StatusNotFound, "check not found")
		return
	}
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rs, err := s.store.History(r.Context(), id, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]resultJSON, 0, len(rs))
	for _, res := range rs {
		out = append(out, toResultJSON(res))
	}
	writeJSON(w, http.StatusOK, map[string]any{"check_id": id, "results": out})
}

type incidentJSON struct {
	store.Incident
	CheckName       string  `json:"check_name"`
	Open            bool    `json:"open"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func (s *Server) incidents(ctx context.Context, f store.IncidentFilter) ([]incidentJSON, error) {
	incs, err := s.store.ListIncidents(ctx, f)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	out := make([]incidentJSON, 0, len(incs))
	for _, inc := range incs {
		name := inc.CheckID
		if snap, ok := s.mon.Snapshot(inc.CheckID); ok {
			name = snap.Spec.Name
		}
		out = append(out, incidentJSON{
			Incident:        inc,
			CheckName:       name,
			Open:            inc.Open(),
			DurationSeconds: inc.Duration(now).Seconds(),
		})
	}
	return out, nil
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(r, "limit", 50, 1, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.IncidentFilter{CheckID: q.Get("check"), Limit: limit}
	if v := q.Get("open"); v != "" {
		f.OpenOnly, err = strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "open must be a boolean")
			return
		}
	}
	out, err := s.incidents(r.Context(), f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": out})
}

func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, errors.New(name + " must be an integer between " + strconv.Itoa(lo) + " and " + strconv.Itoa(hi))
	}
	return n, nil
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
