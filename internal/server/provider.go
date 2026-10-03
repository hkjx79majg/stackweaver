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
}

// Provider executes single planned changes. An implementation applies exactly
// the one change in the request and returns its outcome as an error; the
// server performs no persistence of its own and never rolls changes back.
type Provider interface {
	// Apply executes one planned change. A non-nil error stops the apply
	// run immediately; its text is reported verbatim to the caller.
	Apply(req ChangeRequest) error
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
