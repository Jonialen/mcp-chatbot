package gemini

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"google.golang.org/genai"
)

// Retry defaults. The free tier answers 503 under load often enough that a
// live demonstration cannot depend on the first attempt succeeding, and 429
// means the quota window simply has not rolled over yet.
const (
	defaultMaxAttempts = 4
	defaultBaseDelay   = 1 * time.Second
	maxDelay           = 20 * time.Second
)

// retryable reports whether an error is worth another attempt.
//
// Only overload and rate limiting qualify. A rejected schema, a missing model
// or a malformed conversation fail the same way every time, and retrying them
// turns a clear error into a slow one.
func retryable(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// backoff returns how long to wait before attempt n, counting from zero.
//
// The delay doubles and carries jitter, so several sessions retrying after the
// same outage do not synchronise into a second one.
func backoff(attempt int) time.Duration {
	delay := defaultBaseDelay << attempt
	if delay > maxDelay {
		delay = maxDelay
	}
	jitter := time.Duration(rand.Int64N(int64(delay / 2)))
	return delay/2 + jitter
}

// withRetry runs call until it succeeds, fails for a reason retrying cannot
// fix, or runs out of attempts. The last error is returned unwrapped so the
// caller sees what the service actually said.
func withRetry[T any](ctx context.Context, attempts int, call func() (T, error)) (T, error) {
	var (
		result T
		err    error
	)

	for attempt := range attempts {
		result, err = call()
		if err == nil || !retryable(err) {
			return result, err
		}
		if attempt == attempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return result, err
}
