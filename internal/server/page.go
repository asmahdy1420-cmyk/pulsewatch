package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/asmahdy1420-cmyk/pulsewatch/internal/check"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/monitor"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/store"
	"github.com/asmahdy1420-cmyk/pulsewatch/internal/uptime"
)

// BarDays is the number of daily bars on the status page.
const BarDays = 90

var funcs = template.FuncMap{
	"statusLabel": statusLabel,
	"pct": func(p float64) string {
		return fmt.Sprintf("%.2f%%", p)
	},
	"day": func(t time.Time) string { return t.Format("2 Jan 2006") },
	"ts":  func(t time.Time) string { return t.UTC().Format("2 Jan 2006 15:04 UTC") },
	"dur": func(d time.Duration) string {
		if d < time.Minute {
			return d.Round(time.Second).String()
		}
		return d.Round(time.Minute).String()
	},
}

func statusLabel(s check.Status) string {
	switch s {
	case check.StatusUp:
		return "Operational"
	case check.StatusDegraded:
		return "Degraded"
	case check.StatusDown:
		return "Outage"
	default:
		return "Pending"
	}
}

type pageCheck struct {
	Name     string
	Status   check.Status
	Uptime   float64
	HasData  bool
	Bars     []uptime.Bar
	LastMsg  string
	LastSeen time.Time
}

type pageIncident struct {
	CheckName string
	Status    check.Status
	Cause     string
	Started   time.Time
	Open      bool
	Duration  time.Duration
}

type pageData struct {
	Title     string
	Overall   check.Status
	Headline  string
	Checks    []pageCheck
	Incidents []pageIncident
	UpdatedAt time.Time
	Days      int
}

func headline(s check.Status) string {
	switch s {
	case check.StatusUp:
		return "All systems operational"
	case check.StatusDegraded:
		return "Some systems are degraded"
	case check.StatusDown:
		return "Some systems are experiencing an outage"
	default:
		return "Collecting first results"
	}
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.clock.Now()
	from := uptime.StartOfDay(now).AddDate(0, 0, -(BarDays - 1))
	to := now.Add(time.Nanosecond)

	snaps := s.mon.Snapshots()
	data := pageData{
		Title:     s.title,
		Overall:   monitor.Overall(snaps),
		UpdatedAt: now,
		Days:      BarDays,
	}
	data.Headline = headline(data.Overall)

	for _, snap := range snaps {
		days, err := s.store.Daily(ctx, snap.Spec.ID, from, to)
		if err != nil {
			s.internalError(w, err)
			return
		}
		pc := pageCheck{Name: snap.Spec.Name, Status: snap.Status, Bars: uptime.Bars(days, now, BarDays)}
		pc.Uptime, pc.HasData = uptime.Sum(days).Percent()
		if snap.Last != nil {
			pc.LastMsg, pc.LastSeen = snap.Last.Message, snap.Last.At
		}
		data.Checks = append(data.Checks, pc)
	}

	incs, err := s.incidents(ctx, store.IncidentFilter{Limit: 10})
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, inc := range incs {
		data.Incidents = append(data.Incidents, pageIncident{
			CheckName: inc.CheckName,
			Status:    inc.Status,
			Cause:     inc.Cause,
			Started:   inc.Started,
			Open:      inc.Open,
			Duration:  inc.Duration(now),
		})
	}

	var buf bytes.Buffer
	if err := s.page.Execute(&buf, data); err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = buf.WriteTo(w)
}
