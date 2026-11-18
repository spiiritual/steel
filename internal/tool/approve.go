package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ApprovalTimeout is how long a tool waits for the user to answer. With
// the tool's own limit it must stay under the scratchpad's script timeout,
// or a slow answer resets the pad.
const ApprovalTimeout = 2 * time.Minute

// Approval asks the user whether a tool may do something.
type Approval struct {
	Tool    string
	Action  string   // what would run, shown to the user verbatim
	Reasons []string // why it needs approval
}

// Approver puts an Approval to the user and reports their answer. It
// returns an error if no answer came, e.g. because ctx ended.
type Approver func(ctx context.Context, a Approval) (allowed bool, err error)

type approverKey struct{}

// WithApprover returns ctx carrying f, for tools called with it to ask
// through.
func WithApprover(ctx context.Context, f Approver) context.Context {
	return context.WithValue(ctx, approverKey{}, f)
}

// Approve asks the user, through ctx's approver, whether a may go ahead,
// waiting up to ApprovalTimeout. It returns nil if they allow it, and an
// error for the model otherwise. Without an approver it refuses: access
// control fails closed.
func Approve(ctx context.Context, a Approval) error {
	why := strings.Join(a.Reasons, "; ")
	f, _ := ctx.Value(approverKey{}).(Approver)
	if f == nil {
		return fmt.Errorf("this needs the user's approval (%s), which can't be asked for here", why)
	}
	actx, cancel := context.WithTimeout(ctx, ApprovalTimeout)
	defer cancel()
	allowed, err := f(actx, a)
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the user didn't answer within %s whether this may run (%s). Don't retry it; tell the user what you want to run and why, and try again once they agree", ApprovalTimeout, why)
	case err != nil:
		return fmt.Errorf("asking the user's approval: %w", err)
	case !allowed:
		return fmt.Errorf("the user refused this (%s). Don't retry it or work around it; ask the user how to proceed", why)
	}
	return nil
}
