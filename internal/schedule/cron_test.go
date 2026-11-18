package schedule

import (
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestCronNext(t *testing.T) {
	london := mustZone(t, "Europe/London")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		expr, from, want string
	}{
		{"0 8 * * 1-5", "2026-11-20 08:00", "2026-11-23 08:00"}, // Friday 8:00 → Monday
		{"0 8 * * mon-fri", "2026-11-17 07:59", "2026-11-17 08:00"},
		{"*/15 * * * *", "2026-11-17 10:07", "2026-11-17 10:15"},
		{"30 9 1 * *", "2026-11-17 00:00", "2026-12-01 09:30"},
		// Both day fields restricted: either matches.
		{"0 9 1 * mon", "2026-11-17 10:00", "2026-11-23 09:00"},
		{"0 0 29 2 *", "2026-11-17 00:00", "2028-02-29 00:00"},
		{"@daily", "2026-11-17 10:00", "2026-11-18 00:00"},
		{"0 12 * * 7", "2026-11-17 00:00", "2026-11-22 12:00"}, // 7 is Sunday
		// Clocks jump from 01:00 to 02:00 on 29 March: 01:30 doesn't
		// exist that day.
		{"30 1 * * *", "2026-03-28 02:00", "2026-03-30 01:30"},
	}
	for _, tc := range tests {
		c, err := parseCron(tc.expr)
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		got, ok := c.next(at(tc.from), london)
		if !ok || !got.Equal(at(tc.want)) {
			t.Errorf("%q after %s = %v, %v; want %s", tc.expr, tc.from, got, ok, tc.want)
		}
	}
}

func TestCronFiresOnceWhenClocksFallBack(t *testing.T) {
	london := mustZone(t, "Europe/London")
	c, err := parseCron("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 01:30 happens twice on 25 October 2026.
	first, _ := c.next(time.Date(2026, 10, 25, 0, 0, 0, 0, london), london)
	second, _ := c.next(first, london)
	if second.Sub(first) < 24*time.Hour {
		t.Errorf("fired at %v and again at %v", first, second)
	}
}

func TestCronRejectsBadExpressions(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "5-1 * * * *", "*/0 * * * *", "x * * * *"} {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("%q: no error", expr)
		}
	}
}

func TestCronThatNeverFires(t *testing.T) {
	c, err := parseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c.next(time.Now(), time.UTC); ok {
		t.Errorf("next = %v", got)
	}
}
