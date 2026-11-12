package service

import "context"

type manualAccountTestContextKey struct{}

// WithManualAccountTest allows an explicit administrator test to inspect the
// upstream despite local scheduling and cooldown state. Only admin test
// handlers set this marker; scheduled probes and client requests do not.
func WithManualAccountTest(ctx context.Context) context.Context {
	return context.WithValue(ctx, manualAccountTestContextKey{}, true)
}

func isManualAccountTest(ctx context.Context) bool {
	return ctx.Value(manualAccountTestContextKey{}) == true
}
