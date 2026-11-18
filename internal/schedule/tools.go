package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spiiritual/steel/internal/tool"
)

// Tools returns the model's way into s: tools.schedule.add, .list and
// .cancel.
func Tools(s *Scheduler) []tool.Tool {
	type addArgs struct {
		Name    string `json:"name" desc:"Short title, e.g. \"Inbox summary\"; a recurring schedule's topic is named after it"`
		At      string `json:"at,omitempty" desc:"Once, at a wall-clock time in zone: \"2026-11-18 17:00\""`
		In      string `json:"in,omitempty" desc:"Once, after a delay: \"20m\", \"2h\", \"1d\""`
		Every   string `json:"every,omitempty" desc:"Repeatedly, at this interval from now: \"90m\", \"1d\""`
		Cron    string `json:"cron,omitempty" desc:"Repeatedly, at times matching a 5-field cron expression in zone: \"0 8 * * 1-5\" is 08:00 on weekdays"`
		Zone    string `json:"zone,omitempty" desc:"IANA time zone for at and cron, e.g. \"Europe/London\"; default steel's"`
		Message string `json:"message,omitempty" desc:"Text sent to the user as it is, without you: for plain reminders"`
		Prompt  string `json:"prompt,omitempty" desc:"Instructions you'll get at that time, for work that needs tools or thought; write them for a future you without this conversation"`
	}
	type added struct {
		ID    string   `json:"id"`
		When  string   `json:"when"`
		Next  []string `json:"next"`
		Where string   `json:"where"`
	}
	type idArgs struct {
		ID string `json:"id" desc:"The schedule's id, from add or list"`
	}
	type listed struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		When     string   `json:"when"`
		Next     []string `json:"next"`
		Message  string   `json:"message,omitempty"`
		Prompt   string   `json:"prompt,omitempty"`
		LastRun  string   `json:"lastRun,omitempty"`
		MadeHere bool     `json:"madeHere"`
	}
	type listResult struct {
		Now       string   `json:"now"`
		Schedules []listed `json:"schedules"`
	}
	upcoming := func(sc Schedule, n int) []string {
		var out []string
		for _, t := range sc.Upcoming(n) {
			out = append(out, sc.When.format(t))
		}
		return out
	}
	return []tool.Tool{
		tool.New("schedule.add",
			fmt.Sprintf("Do something later: send a message, or give yourself a prompt to work on. Give a name, exactly one of at/in/every/cron, and exactly one of message/prompt. A one-off posts back into this conversation; a recurring schedule gets a topic of its own, and each run starts a fresh conversation there with the previous run's reply. Recurring runs must be at least %s apart; at most %d schedules. Returns the next run times: check them against what the user asked for.", formatDuration(MinGap), MaxSchedules),
			func(ctx context.Context, a addArgs) (added, error) {
				from := tool.Caller(ctx)
				if from == "" {
					return added{}, errors.New("schedules can't be made from here")
				}
				sc, err := s.Add(ctx, from, AddArgs(a))
				if err != nil {
					return added{}, err
				}
				where := "this conversation"
				if sc.ownPlace() {
					where = fmt.Sprintf("its own topic, %q", sc.Name)
				} else if !sc.When.once() {
					where = "this conversation (no topic could be made for it)"
				}
				return added{ID: sc.ID, When: sc.When.String(), Next: upcoming(sc, 3), Where: where}, nil
			}),
		tool.New("schedule.list",
			"List every schedule, soonest first, with the current time.",
			func(ctx context.Context, _ struct{}) (listResult, error) {
				from := tool.Caller(ctx)
				out := listResult{Now: formatTime(time.Now()), Schedules: []listed{}}
				for _, sc := range s.List() {
					l := listed{ID: sc.ID, Name: sc.Name, When: sc.When.String(), Next: upcoming(sc, 3),
						Message: sc.Message, Prompt: sc.Prompt, MadeHere: sc.From == from}
					if r := sc.Last; r != nil {
						l.LastRun = sc.When.format(r.Due)
						if r.Err != "" {
							l.LastRun += ", failed: " + r.Err
						}
					}
					out.Schedules = append(out.Schedules, l)
				}
				return out, nil
			}),
		tool.New("schedule.cancel",
			"Delete a schedule for good, and its topic if it has one.",
			func(ctx context.Context, a idArgs) (string, error) {
				sc, err := s.Cancel(ctx, a.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Cancelled %q.", sc.Name), nil
			}),
	}
}
