package server

import (
	"context"
	"errors"
)

// executeChange runs one planned change through its Provider under the
// controlled retry policy: the change is attempted at most maxApplyAttempts
// times, serially, with Attempt 1, 2, 3 and the same IdempotencyKey on every
// call. A plain error, or one that is not retryable, stops the attempts at
// once; a retryable error schedules another attempt of this same change
// without advancing to later changes. Before preparing a retry the request
// context is consulted: once it has ended the Provider is not called again
// and the context's own error is returned, its text reaching the caller like
// any provider_error text. nil means the change succeeded; it is recorded by
// the caller exactly once.
func executeChange(ctx context.Context, provider Provider, change planChange, idempotencyKey string) error {
	var lastErr error
	for attempt := 1; attempt <= maxApplyAttempts; attempt++ {
		if attempt > 1 {
			// The first attempt follows the baseline behavior of calling
			// Apply with the inbound context as-is; only retries are gated on
			// the context still being alive.
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		req := ChangeRequest{
			Context:        ctx,
			IdempotencyKey: idempotencyKey,
			Attempt:        attempt,
			Action:         change.Action,
			Address:        change.Address,
			Before:         change.Before,
			After:          change.After,
		}
		err := provider.Apply(req)
		if err == nil {
			return nil
		}
		lastErr = err
		var retryable RetryableError
		if !errors.As(err, &retryable) || !retryable.Retryable() {
			// A non-retryable result stops immediately even when earlier
			// attempts of the same change failed retryably.
			return err
		}
	}
	// Every attempt failed retryably; report the last error verbatim.
	return lastErr
}
