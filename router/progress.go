package router

import "context"

// AttemptObserver receives live attempt updates during routing.
type AttemptObserver interface {
	OnAttempt(Attempt)
}

type attemptObserverKey struct{}

// WithAttemptObserver attaches an optional observer to ctx.
func WithAttemptObserver(ctx context.Context, obs AttemptObserver) context.Context {
	if obs == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptObserverKey{}, obs)
}

// AttemptObserverFrom returns the observer stored in ctx, if any.
func AttemptObserverFrom(ctx context.Context) (AttemptObserver, bool) {
	obs, ok := ctx.Value(attemptObserverKey{}).(AttemptObserver)
	return obs, ok && obs != nil
}

func notifyAttempt(ctx context.Context, att Attempt) {
	if obs, ok := AttemptObserverFrom(ctx); ok {
		obs.OnAttempt(att)
	}
}
