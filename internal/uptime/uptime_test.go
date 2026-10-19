package uptime

import (
	"testing"
	"time"
)

func TestPercent(t *testing.T) {
	tests := []struct {
		name   string
		tally  Tally
		want   float64
		wantOK bool
	}{
		{"no data", Tally{}, 0, false},
		{"all up", Tally{Up: 10}, 100, true},
		{"degraded counts as available", Tally{Up: 2, Degraded: 2}, 100, true},
		{"one in eight down", Tally{Up: 7, Down: 1}, 87.5, true},
		{"rounded to 3 decimals", Tally{Up: 2, Down: 1}, 66.667, true},
		{"all down", Tally{Down: 4}, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.tally.Percent()
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Percent() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLevelFor(t *testing.T) {
	tests := []struct {
		tally Tally
		want  Level
	}{
		{Tally{}, LevelNone},
		{Tally{Up: 1440}, LevelOK},
		{Tally{Up: 1400, Degraded: 40}, LevelDegraded},
		{Tally{Up: 1430, Down: 10}, LevelPartial},
		{Tally{Up: 100, Down: 100}, LevelDown},
	}
	for _, tc := range tests {
		if got := LevelFor(tc.tally); got != tc.want {
			t.Errorf("LevelFor(%+v) = %s, want %s", tc.tally, got, tc.want)
		}
	}
}

func TestBarsFillsMissingDaysAndEndsToday(t *testing.T) {
	now := time.Date(2026, 3, 10, 15, 30, 0, 0, time.UTC)
	days := []Day{
		{Day: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), Tally: Tally{Up: 5}},
		{Day: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), Tally: Tally{Up: 1, Down: 1}},
		{Day: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Tally: Tally{Up: 1}}, // outside window
	}
	bars := Bars(days, now, 5)
	if len(bars) != 5 {
		t.Fatalf("len = %d, want 5", len(bars))
	}
	if first := bars[0].Day; !first.Equal(time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first bar day = %v", first)
	}
	wantLevels := []Level{LevelNone, LevelNone, LevelDown, LevelNone, LevelOK}
	for i, b := range bars {
		if b.Level != wantLevels[i] {
			t.Errorf("bar %d (%s) level = %s, want %s", i, b.Day.Format("Jan 2"), b.Level, wantLevels[i])
		}
	}
	if bars[2].Percent != 50 || !bars[2].HasData {
		t.Fatalf("bar 2 = %+v, want 50%%", bars[2])
	}
	if total, _ := Sum(days[:2]).Percent(); total != 85.714 {
		t.Fatalf("Sum percent = %v, want 85.714", total)
	}
}
