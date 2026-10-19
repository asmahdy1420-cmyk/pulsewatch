// Package uptime turns raw probe counts into uptime percentages and the
// per-day bars shown on the status page.
package uptime

import (
	"math"
	"time"
)

// Tally counts probe outcomes in a window.
type Tally struct {
	Up       int `json:"up"`
	Degraded int `json:"degraded"`
	Down     int `json:"down"`
}

// Total is the number of probes counted.
func (t Tally) Total() int { return t.Up + t.Degraded + t.Down }

// Add returns the sum of two tallies.
func (t Tally) Add(o Tally) Tally {
	return Tally{Up: t.Up + o.Up, Degraded: t.Degraded + o.Degraded, Down: t.Down + o.Down}
}

// Percent returns the share of probes that were not down, in the range
// 0..100, rounded to three decimals. Degraded counts as available: the
// service answered, just slowly or with a warning. ok is false when there
// is no data.
func (t Tally) Percent() (pct float64, ok bool) {
	total := t.Total()
	if total == 0 {
		return 0, false
	}
	p := float64(t.Up+t.Degraded) / float64(total) * 100
	return math.Round(p*1000) / 1000, true
}

// Day is a tally for one UTC calendar day.
type Day struct {
	Day time.Time `json:"day"`
	Tally
}

// Level classifies a day for colouring the status bar.
type Level string

// Bar levels, from best to worst.
const (
	LevelNone     Level = "none"
	LevelOK       Level = "ok"
	LevelDegraded Level = "degraded"
	LevelPartial  Level = "partial"
	LevelDown     Level = "down"
)

// PartialThreshold is the uptime percentage at or above which a day with
// failures is shown as a partial outage rather than a full one.
const PartialThreshold = 90.0

// Bar is one rendered day.
type Bar struct {
	Day     time.Time
	Tally   Tally
	Percent float64
	HasData bool
	Level   Level
}

// LevelFor classifies a tally.
func LevelFor(t Tally) Level {
	pct, ok := t.Percent()
	switch {
	case !ok:
		return LevelNone
	case t.Down == 0 && t.Degraded == 0:
		return LevelOK
	case t.Down == 0:
		return LevelDegraded
	case pct >= PartialThreshold:
		return LevelPartial
	default:
		return LevelDown
	}
}

// StartOfDay truncates t to midnight UTC.
func StartOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Bars returns exactly n bars, oldest first, ending with the UTC day that
// contains now. Days missing from days are rendered as LevelNone.
func Bars(days []Day, now time.Time, n int) []Bar {
	byDay := make(map[time.Time]Tally, len(days))
	for _, d := range days {
		k := StartOfDay(d.Day)
		byDay[k] = byDay[k].Add(d.Tally)
	}
	last := StartOfDay(now)
	bars := make([]Bar, n)
	for i := 0; i < n; i++ {
		day := last.AddDate(0, 0, i-n+1)
		t := byDay[day]
		pct, ok := t.Percent()
		bars[i] = Bar{Day: day, Tally: t, Percent: pct, HasData: ok, Level: LevelFor(t)}
	}
	return bars
}

// Sum adds up all days.
func Sum(days []Day) Tally {
	var t Tally
	for _, d := range days {
		t = t.Add(d.Tally)
	}
	return t
}
