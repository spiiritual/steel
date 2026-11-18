package tool

import "context"

type callerKey struct{}

// WithCaller returns ctx naming session id as the caller of tools run
// with it, for tools whose effects belong somewhere, like a schedule
// posting back to the conversation that made it.
func WithCaller(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerKey{}, id)
}

// Caller returns the session calling a tool, or "" if unknown.
func Caller(ctx context.Context) string {
	id, _ := ctx.Value(callerKey{}).(string)
	return id
}
