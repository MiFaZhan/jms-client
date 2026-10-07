package api

import "context"

// retryBudgetKey is the context key carrying a per-call retry override.
type retryBudgetKey struct{}

// WithRetryBudget returns a context that caps how many times requests made
// through it are retried.
//
// It exists for callers that are probing rather than working: deciding
// whether an endpoint is usable should not pay the full retry schedule on a
// server that answers 502 after a five-second stall — four attempts turned
// one probe into twenty-four seconds of dead time.
//
// A budget of 0 means a single attempt. A negative budget or an absent value
// means the client default.
func WithRetryBudget(ctx context.Context, max int) context.Context {
	return context.WithValue(ctx, retryBudgetKey{}, max)
}

// budgetFrom returns the retry budget carried by ctx, or -1 when none is set.
func budgetFrom(ctx context.Context) int {
	if ctx == nil {
		return -1
	}
	if v, ok := ctx.Value(retryBudgetKey{}).(int); ok {
		return v
	}
	return -1
}
