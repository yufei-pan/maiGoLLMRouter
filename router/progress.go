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

// CommitObserver is told when a streamed request commits to an attempt, the
// moment its first output goes to the client. An AttemptObserver may also
// implement it.
type CommitObserver interface {
	OnCommit(provider, model string)
}

func notifyCommit(ctx context.Context, provider, model string) {
	if obs, ok := AttemptObserverFrom(ctx); ok {
		if c, ok := obs.(CommitObserver); ok {
			c.OnCommit(provider, model)
		}
	}
}
