package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cron is a parsed five-field cron expression: minute, hour, day of month,
// month, day of week. Each field is a set of allowed values.
type cron struct {
	minute, hour, dom, month, dow uint64 // bit i set: value i allowed
	// With both day fields restricted, a day matches either (the classic
	// cron rule): "0 9 1 * MON" is the 1st and every Monday.
	domStar, dowStar bool
}

var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	dayNames   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

func parseCron(expr string) (cron, error) {
	expr = strings.TrimSpace(expr)
	if m, ok := cronMacros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return cron{}, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(f))
	}
	var c cron
	var err error
	if c.minute, err = parseField(f[0], 0, 59, nil); err != nil {
		return cron{}, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	if c.hour, err = parseField(f[1], 0, 23, nil); err != nil {
		return cron{}, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	if c.dom, err = parseField(f[2], 1, 31, nil); err != nil {
		return cron{}, fmt.Errorf("cron %q: day of month: %w", expr, err)
	}
	if c.month, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return cron{}, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	if c.dow, err = parseField(f[4], 0, 7, dayNames); err != nil {
		return cron{}, fmt.Errorf("cron %q: day of week: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*", f[4] == "*"
	return c, nil
}

// parseField parses a comma-separated list of "*", "n", "a-b", each
// optionally "/step". names, if set, are accepted for values from lo.
func parseField(s string, lo, hi int, names []string) (uint64, error) {
	var bits uint64
	for part := range strings.SplitSeq(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		var a, b int
		switch {
		case rng == "*":
			a, b = lo, hi
		case strings.Contains(rng, "-"):
			x, y, _ := strings.Cut(rng, "-")
			var err error
			if a, err = value(x, lo, hi, names); err != nil {
				return 0, err
			}
			if b, err = value(y, lo, hi, names); err != nil {
				return 0, err
			}
			if a > b {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		default:
			var err error
			if a, err = value(rng, lo, hi, names); err != nil {
				return 0, err
			}
			b = a
			if hasStep { // "5/15" means 5-hi/15
				b = hi
			}
		}
		for v := a; v <= b; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func value(s string, lo, hi int, names []string) (int, error) {
	for i, n := range names {
		if strings.EqualFold(s, n) {
			return lo + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%q is not a number from %d to %d", s, lo, hi)
	}
	return v, nil
}

func (c cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

// cronHorizon bounds the search for the next match; an expression that
// matches nothing (e.g. "0 0 30 2 *") gives up after it.
const cronHorizon = 5 * 366

// next returns the first matching time after t, in loc. It walks wall-clock
// times, so daylight saving time is handled the way people expect: a time
// that doesn't exist that day (clocks jump past it) is skipped, and a time
// that happens twice (clocks fall back) fires once.
func (c cron) next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	for range cronHorizon {
		y, mo, d := day.Date()
		if c.month&(1<<int(mo)) != 0 && c.dayMatches(day) {
			for h := range 24 {
				if c.hour&(1<<h) == 0 {
					continue
				}
				for m := range 60 {
					if c.minute&(1<<m) == 0 {
						continue
					}
					at := time.Date(y, mo, d, h, m, 0, 0, loc)
					if at.Hour() != h || at.Minute() != m {
						continue // doesn't exist today: clocks jumped past it
					}
					if at.After(t) {
						return at, true
					}
				}
			}
		}
		day = time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
	}
	return time.Time{}, false
}
