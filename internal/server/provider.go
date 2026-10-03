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

// Observer is an optional Provider capability: a Provider that also
// implements Observer lets GET /v1/state/drift compare the managed resources
// recorded in the state file against the live cloud snapshot. Observation is
// read-only; it never mutates the remote side or the state file.
type Observer interface {
	// Observe fetches the current remote view of one managed resource. The
	// context is the inbound request context and must not be retained past
	// the call. A nil Snapshot with a nil error means the resource does not
	// exist remotely; a non-nil error stops the drift run immediately.
	Observe(ctx context.Context, address string) (*Snapshot, error)
}
