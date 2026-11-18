package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestTimingParse(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		a       timing
		wantErr string
		next    time.Time
	}{
		{"none", timing{}, "exactly one", time.Time{}},
		{"two", timing{In: "5m", Every: "1h"}, "exactly one", time.Time{}},
		{"in", timing{In: "20m"}, "", now.Add(20 * time.Minute)},
		{"in days", timing{In: "1d2h"}, "", now.Add(26 * time.Hour)},
		{"in too soon", timing{In: "10s"}, "at least a minute", time.Time{}},
		{"at in zone", timing{At: "2026-11-18 17:00", Zone: "America/New_York"}, "", time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)},
		{"at with offset", timing{At: "2026-11-18T17:00:00+01:00"}, "", time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)},
		{"at past", timing{At: "2026-11-17 09:00", Zone: "UTC"}, "in the past", time.Time{}},
		{"at garbage", timing{At: "tomorrow at 5"}, "want a date and time", time.Time{}},
		{"every", timing{Every: "2h"}, "", now.Add(2 * time.Hour)},
		{"every too often", timing{Every: "1m"}, "too often", time.Time{}},
		{"cron", timing{Cron: "0 8 * * *", Zone: "UTC"}, "", time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)},
		{"cron too often", timing{Cron: "* * * * *"}, "too often", time.Time{}},
		{"cron uneven gaps", timing{Cron: "0,1 9 * * *"}, "too often", time.Time{}},
		{"cron never", timing{Cron: "0 0 31 2 *"}, "never fires", time.Time{}},
		{"bad zone", timing{In: "1h", Zone: "Mars/Olympus"}, "IANA", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := tc.a.parse(now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			next, ok := w.next(now)
			if !ok || !next.Equal(tc.next) {
				t.Errorf("next = %v, %v; want %v", next, ok, tc.next)
			}
		})
	}
}

func TestEveryKeepsItsRhythm(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	w, err := timing{Every: "90m"}.parse(now)
	if err != nil {
		t.Fatal(err)
	}
	// However late it's asked, the next run is on the 90-minute grid.
	next, _ := w.next(now.Add(5*time.Hour + 7*time.Minute))
	if want := now.Add(6 * time.Hour); !next.Equal(want) {
		t.Errorf("next = %v; want %v", next, want)
	}
}

func TestGrace(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	daily, _ := timing{Cron: "0 8 * * *", Zone: "UTC"}.parse(now)
	due, _ := daily.next(now)
	if g := daily.grace(due); g != 12*time.Hour {
		t.Errorf("daily grace = %v", g)
	}
	once, _ := timing{In: "1h"}.parse(now)
	if g := once.grace(now.Add(time.Hour)); g < 100*365*24*time.Hour {
		t.Errorf("one-off grace = %v; want forever", g)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{2 * time.Hour: "2h", 90 * time.Minute: "1h30m", 5 * time.Minute: "5m", 90 * time.Second: "1m30s"} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q; want %q", d, got, want)
		}
	}
}
