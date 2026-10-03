package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

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
//
// IdempotencyKey identifies one change within one top-level request across
// every attempt the server makes at it: it is non-empty and unique among the
// non-noop changes of a single request and stays the same on retry, while a
// new top-level request always mints fresh keys. Attempt is 1-based and
// counts the calls made for the same change (1, 2, 3). Providers that ignore
// the new fields keep working unchanged.
type ChangeRequest struct {
	Context        context.Context `json:"-"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Attempt        int             `json:"attempt"`
	Action         string          `json:"action"`
	Address        string          `json:"address"`
	Before         *Snapshot       `json:"before,omitempty"`
	After          *Snapshot       `json:"after,omitempty"`
}

// Provider executes single planned changes. An implementation applies exactly
// the one change in the request and returns its outcome as an error; the
// server performs no persistence of its own and never rolls changes back.
type Provider interface {
	// Apply executes one planned change. A non-nil error stops the apply
	// run immediately; its text is reported verbatim to the caller. An
	// error implementing RetryableError with Retryable() == true is retried
	// according to the server's attempt budget before the run stops.
	Apply(req ChangeRequest) error
}

// RetryableError is implemented by errors returned from Provider.Apply that
// the server may safely retry. A plain error, or one whose Retryable reports
// false, keeps the baseline behavior: the run stops immediately with
// provider_error. One whose Retryable reports true triggers another attempt
// of the same change, up to the server's attempt budget; an error that stops
// being retryable on a later attempt stops the run at once.
type RetryableError interface {
	error
	Retryable() bool
}

// maxApplyAttempts is the inclusive number of Apply calls the server makes
// for one change: Attempt 1 plus up to two retries (Attempts 2 and 3).
const maxApplyAttempts = 3

// idempotencyCounter only backs the (practically unreachable) crypto/rand
// failure path, so a broken entropy source still yields non-empty distinct
// keys within and across requests.
var idempotencyCounter atomic.Uint64

// mintIdempotencyKeys allocates one non-empty, pairwise-distinct key per
// non-noop change of a new top-level request. A later top-level request
// mints a fresh set, so keys are never reused across requests.
func mintIdempotencyKeys(n int) []string {
	keys := make([]string, n)
	seen := make(map[string]struct{}, n)
	for i := range keys {
		for {
			key := "sw-" + randKey()
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				keys[i] = key
				break
			}
		}
	}
	return keys
}

// randKey returns 16 random bytes hex-encoded; crypto/rand makes accidental
// cross-request reuse negligibly likely without any persisted counter.
func randKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand fails only when the system entropy source is
		// unavailable; fall back to a time/counter suffix so keys stay
		// non-empty and distinct rather than duplicated or blank.
		return fmt.Sprintf("%x-%d", time.Now().UnixNano(), idempotencyCounter.Add(1))
	}
	return hex.EncodeToString(b[:])
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
