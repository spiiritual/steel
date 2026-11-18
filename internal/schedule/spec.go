package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Parsed zones and cron expressions, by text: next runs in loops, and
// loading a zone reads a file.
var zones, crons sync.Map

// MinGap is the shortest time allowed between a recurring schedule's
// runs, so a confused model can't flood the user or run up costs.
const MinGap = 5 * time.Minute

// when is a schedule's timing: exactly one of at (once), every (a fixed
// interval from start), or cron, in zone.
type when struct {
	At    time.Time     `json:"at,omitzero"`
	Every time.Duration `json:"every,omitempty"`
	Start time.Time     `json:"start,omitzero"` // every's anchor
	Cron  string        `json:"cron,omitempty"`
	Zone  string        `json:"zone"`
}

func (w when) once() bool { return !w.At.IsZero() }

func (w when) location() (*time.Location, error) {
	if w.Zone == "" || w.Zone == "Local" {
		return time.Local, nil
	}
	if loc, ok := zones.Load(w.Zone); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(w.Zone)
	if err == nil {
		zones.Store(w.Zone, loc)
	}
	return loc, err
}

// next returns the first time the schedule is due after t, or false if it
// never is again.
func (w when) next(t time.Time) (time.Time, bool) {
	loc, err := w.location()
	if err != nil {
		return time.Time{}, false
	}
	switch {
	case w.once():
		if !w.At.After(t) {
			return time.Time{}, false
		}
		return w.At, true
	case w.Every > 0:
		if t.Before(w.Start) {
			return w.Start.In(loc), true
		}
		n := t.Sub(w.Start)/w.Every + 1
		return w.Start.Add(n * w.Every).In(loc), true
	default:
		c, ok := crons.Load(w.Cron)
		if !ok {
			parsed, err := parseCron(w.Cron)
			if err != nil {
				return time.Time{}, false
			}
			c, _ = crons.LoadOrStore(w.Cron, parsed)
		}
		return c.(cron).next(t, loc)
	}
}

// grace is how late a missed run may still happen: half the gap to the
// run after due, so a daily 8:00 job still runs at 11:00 but an hourly one
// two hours late is skipped. One-offs always run, however late.
func (w when) grace(due time.Time) time.Duration {
	if w.once() {
		return 1<<63 - 1
	}
	after, ok := w.next(due)
	if !ok {
		return 0
	}
	return after.Sub(due) / 2
}

// String describes the timing for people, e.g. "every 2h" or
// "cron 0 8 * * 1-5 (Europe/London)".
func (w when) String() string {
	switch {
	case w.once():
		return "once at " + w.format(w.At)
	case w.Every > 0:
		return "every " + formatDuration(w.Every)
	default:
		return fmt.Sprintf("cron %s (%s)", w.Cron, cmpZone(w.Zone))
	}
}

func cmpZone(z string) string {
	if z == "" {
		return time.Local.String()
	}
	return z
}

// format shows t in the schedule's zone, as formatTime does. Saved times
// lose their zone's name, so they're converted back first.
func (w when) format(t time.Time) string {
	if loc, err := w.location(); err == nil {
		t = t.In(loc)
	}
	return formatTime(t)
}

// formatTime shows t the way the user reads it: "Mon 18 Nov 08:00 GMT",
// with the year when it isn't this one.
func formatTime(t time.Time) string {
	if t.Year() != time.Now().In(t.Location()).Year() {
		return t.Format("Mon 2 Jan 2006 15:04 MST")
	}
	return t.Format("Mon 2 Jan 15:04 MST")
}

// formatDuration drops Go's zero units: "2h" rather than "2h0m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// parseDuration is time.ParseDuration plus a leading day count: "2d",
// "1d12h".
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var days time.Duration
	if d, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q isn't a duration like \"90m\", \"2h\" or \"1d\"", s)
		}
		days, s = time.Duration(n)*24*time.Hour, rest
		if s == "" {
			return days, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q isn't a duration like \"90m\", \"2h\" or \"1d\"", s)
	}
	return days + d, nil
}

// wallLayouts are the times "at" accepts, read in the schedule's zone.
var wallLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

func parseAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), nil
	}
	for _, l := range wallLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("at %q: want a date and time like \"2026-11-18 17:00\"", s)
}

// timing is the part of the add tool's arguments that says when.
type timing struct {
	At    string
	In    string
	Every string
	Cron  string
	Zone  string
}

// parse turns the tool's arguments into a when, checking it fires at
// least once and no more often than MinGap.
func (a timing) parse(now time.Time) (when, error) {
	set := 0
	for _, v := range []string{a.At, a.In, a.Every, a.Cron} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set != 1 {
		return when{}, errors.New(`give exactly one of "at", "in", "every" or "cron"`)
	}
	w := when{Zone: strings.TrimSpace(a.Zone)}
	loc, err := w.location()
	if err != nil {
		return when{}, fmt.Errorf("zone %q: want an IANA zone like \"Europe/London\"", w.Zone)
	}
	switch {
	case a.At != "":
		if w.At, err = parseAt(a.At, loc); err != nil {
			return when{}, err
		}
		if !w.At.After(now) {
			return when{}, fmt.Errorf("at %s is in the past (it's %s now)", formatTime(w.At), formatTime(now.In(loc)))
		}
	case a.In != "":
		d, err := parseDuration(a.In)
		if err != nil {
			return when{}, err
		}
		if d < time.Minute {
			return when{}, errors.New(`"in" must be at least a minute`)
		}
		w.At = now.Add(d).In(loc).Truncate(time.Second)
	case a.Every != "":
		if w.Every, err = parseDuration(a.Every); err != nil {
			return when{}, err
		}
		if w.Every < MinGap {
			return when{}, fmt.Errorf("every %s is too often; the least is %s", formatDuration(w.Every), formatDuration(MinGap))
		}
		w.Start = now.Add(w.Every).Truncate(time.Second)
	default:
		w.Cron = strings.Join(strings.Fields(a.Cron), " ")
		if _, err := parseCron(w.Cron); err != nil {
			return when{}, err
		}
		if err := w.checkGaps(now); err != nil {
			return when{}, err
		}
	}
	return w, nil
}

// checkGaps checks a cron schedule fires, and its next runs are at least
// MinGap apart.
func (w when) checkGaps(now time.Time) error {
	prev, ok := w.next(now)
	if !ok {
		return fmt.Errorf("cron %q never fires", w.Cron)
	}
	for range 200 {
		t, ok := w.next(prev)
		if !ok {
			return nil
		}
		if t.Sub(prev) < MinGap {
			return fmt.Errorf("cron %q fires too often (%s then %s); runs must be at least %s apart", w.Cron, formatTime(prev), formatTime(t), formatDuration(MinGap))
		}
		prev = t
	}
	return nil
}
