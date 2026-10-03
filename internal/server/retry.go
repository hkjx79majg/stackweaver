package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync/atomic"
)

// maxApplyAttempts bounds the controlled retry of one change: a change whose
// Apply fails with a RetryableError is attempted at most three times in
// total, numbered 1, 2, 3.
const maxApplyAttempts = 3

// idempotencyRunCounter makes run prefixes unique across top-level requests
// even in the practically impossible case that crypto/rand fails.
var idempotencyRunCounter atomic.Uint64

// newIdempotencyKeys returns one non-empty, mutually distinct key per change
// of a single top-level request. Every call draws a fresh run prefix, so a
// later request never reuses the keys of an earlier one; the attempts of one
// change share its key.
func newIdempotencyKeys(count int) []string {
	prefix := strconv.FormatUint(idempotencyRunCounter.Add(1), 10)
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		prefix += "-" + hex.EncodeToString(random[:])
	}

	keys := make([]string, count)
	for i := range keys {
		keys[i] = prefix + "/" + strconv.Itoa(i)
	}
	return keys
}

// applyChange executes one planned change through provider with controlled
// retries. A nil error ends the loop; a plain error or a RetryableError
// reporting Retryable() false is returned as is, exactly like the baseline's
// single call. While the failure reports Retryable() true the same change is
// called again in place — same idempotency key, next attempt number — until
// it succeeds or reaches maxApplyAttempts, whose final error is returned. No
// other change runs meanwhile. Before a retry is prepared, a finished
// request context aborts the loop with the context's own error and no
// further Provider call.
func applyChange(ctx context.Context, provider Provider, change planChange, idempotencyKey string) error {
	for attempt := 1; ; attempt++ {
		err := provider.Apply(ChangeRequest{
			Context:        ctx,
			Action:         change.Action,
			Address:        change.Address,
			Before:         change.Before,
			After:          change.After,
			IdempotencyKey: idempotencyKey,
			Attempt:        attempt,
		})
		if err == nil {
			return nil
		}
		retryable, isRetryable := err.(RetryableError)
		if !isRetryable || !retryable.Retryable() || attempt == maxApplyAttempts {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
}
