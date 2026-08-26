package gemini

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
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

	// maxServerDelay caps how long a service may ask this client to wait. A
	// free tier that answers "come back tomorrow" should surface as an error a
	// person can act on, not as a session that appears to hang.
	maxServerDelay = 90 * time.Second
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

// retryAfter reads the delay the service asked for.
//
// A rate limiter knows exactly when its window rolls over and says so, which is
// better information than any backoff curve can guess. Ignoring it and retrying
// on a schedule of our own is how a caller burns every attempt inside a window
// that had not reopened yet.
func retryAfter(err error) (time.Duration, bool) {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}

	for _, detail := range apiErr.Details {
		kind, _ := detail["@type"].(string)
		if !strings.Contains(kind, "RetryInfo") {
			continue
		}

		raw, ok := detail["retryDelay"].(string)
		if !ok {
			continue
		}
		delay, err := time.ParseDuration(raw)
		if err != nil || delay <= 0 {
			continue
		}
		if delay > maxServerDelay {
			delay = maxServerDelay
		}
		// A second of slack, so the retry lands after the window rolls over
		// rather than exactly on its edge.
		return delay + time.Second, true
	}
	return 0, false
}

// backoff returns how long to wait before attempt n, counting from zero.
//
// The delay doubles and carries jitter, so several sessions retrying after the
// same outage do not synchronise into a second one. It is the fallback for when
// the service did not say how long to wait.
func backoff(attempt int) time.Duration {
	delay := defaultBaseDelay << attempt
	if delay > maxDelay {
		delay = maxDelay
	}
	jitter := time.Duration(rand.Int64N(int64(delay / 2)))
	return delay/2 + jitter
}

// waitFor picks how long to hold before the next attempt, preferring what the
// service asked for over a curve of our own.
func waitFor(err error, attempt int) (time.Duration, bool) {
	if delay, asked := retryAfter(err); asked {
		return delay, true
	}
	return backoff(attempt), false
}

// onWait is told how long the next attempt is being held for, so a session that
// pauses for most of a minute reads as waiting rather than as hanging.
type onWait func(delay time.Duration, requested bool)

// withRetry runs call until it succeeds, fails for a reason retrying cannot
// fix, or runs out of attempts. The last error is returned unwrapped so the
// caller sees what the service actually said.
func withRetry[T any](ctx context.Context, attempts int, notify onWait, call func() (T, error)) (T, error) {
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

		delay, requested := waitFor(err, attempt)
		if notify != nil {
			notify(delay, requested)
		}

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}
	return result, err
}
