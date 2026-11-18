package tool

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// SecretTimeout is how long a tool waits for the user to enter a secret:
// long enough to go and copy a token, and with the tool's own work still
// under the scratchpad's script timeout.
const SecretTimeout = 3 * time.Minute

// SecretAsker asks the user to enter secret name's value privately, out of
// the model's sight, and passes it to save before telling the user it was
// saved. It returns an error if none came (the user cancelled, ctx ended)
// or save failed.
type SecretAsker func(ctx context.Context, name, purpose string, save func(value string) error) error

type secretAskerKey struct{}

// WithSecretAsker returns ctx carrying f, for tools called with it to ask
// through.
func WithSecretAsker(ctx context.Context, f SecretAsker) context.Context {
	return context.WithValue(ctx, secretAskerKey{}, f)
}

// AskSecret asks the user, through ctx's asker, for secret name's value,
// waiting up to SecretTimeout, and saves it with save. Errors are for the
// model.
func AskSecret(ctx context.Context, name, purpose string, save func(value string) error) error {
	f, _ := ctx.Value(secretAskerKey{}).(SecretAsker)
	if f == nil {
		return errors.New("the user can't be asked for a secret from here")
	}
	actx, cancel := context.WithTimeout(ctx, SecretTimeout)
	defer cancel()
	err := f(actx, name, purpose, save)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("the user didn't enter %s within %s. Don't ask again right away; tell the user what it's for, and that they can send /secret %s whenever they're ready", name, SecretTimeout, name)
	}
	return err
}
