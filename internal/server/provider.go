package server

import "context"

// Snapshot is the normalized view of one resource on one side of a change:
// before for the prior state, after for the desired configuration.
type Snapshot struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	DependsOn  []string       `json:"dependsOn"`
}

// ChangeRequest is one ordered, already planned change handed to a Provider.
// Create carries only After, delete only Before, update carries both.
//
// Context carries the inbound request context and must not be retained past
// the Apply call; the server never persists the request itself.
type ChangeRequest struct {
	Context context.Context `json:"-"`
	Action  string          `json:"action"`
	Address string          `json:"address"`
	Before  *Snapshot       `json:"before,omitempty"`
	After   *Snapshot       `json:"after,omitempty"`
	// IdempotencyKey identifies the planned change this request belongs to.
	// Within one top-level request every non-noop change gets a non-empty
	// key distinct from every other change's; all attempts of the same
	// change reuse it, and a later top-level request never reuses earlier
	// values. Providers that do not deduplicate executions may ignore it.
	IdempotencyKey string `json:"idempotencyKey"`
	// Attempt is the 1-based number of this execution attempt: 1 for the
	// first call of a change, incremented on every retry.
	Attempt int `json:"attempt"`
}

// Provider executes single planned changes. An implementation applies exactly
// the one change in the request and returns its outcome as an error; the
// server performs no persistence of its own and never rolls changes back.
type Provider interface {
	// Apply executes one planned change. A non-nil error stops the apply
	// run immediately and its text is reported verbatim to the caller,
	// unless it is a RetryableError reporting Retryable() true: then the
	// server retries the same change in place, at most three attempts in
	// total, reusing the IdempotencyKey and incrementing Attempt.
	Apply(req ChangeRequest) error
}

// RetryableError is an error a Provider may return from Apply to mark a
// failure as transient. The server retries the same change (same
// IdempotencyKey, next Attempt) while Retryable reports true, up to three
// attempts in total; a false value behaves exactly like a plain error and
// stops the run immediately.
type RetryableError interface {
	error
	// Retryable reports whether the failure is worth retrying.
	Retryable() bool
}

// Observer is an optional capability a Provider may additionally implement to
// support read-only drift detection. The server only ever reads through an
// Observer; it never applies changes, writes state, or retains the context.
type Observer interface {
	// Observe reads the remote view of the managed resource at address.
	// The passed context is the inbound request context, handed over
	// unchanged. A nil snapshot with a nil error reports that the resource
	// does not exist remotely; a non-nil error stops the drift run
	// immediately and its text is reported verbatim to the caller.
	Observe(ctx context.Context, address string) (*Snapshot, error)
}
