package tool

import "context"

type discloseKey struct{}

// WithDisclose returns ctx carrying f, which documents tools to the model
// with the result of the script whose call carries ctx.
func WithDisclose(ctx context.Context, f func(names ...string)) context.Context {
	return context.WithValue(ctx, discloseKey{}, f)
}

// Disclose documents the named tools with the current script's result, so
// the model can use them in its next script: for a tool that adds tools.
func Disclose(ctx context.Context, names ...string) {
	if f, _ := ctx.Value(discloseKey{}).(func(names ...string)); f != nil {
		f(names...)
	}
}
