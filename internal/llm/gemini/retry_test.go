package gemini

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"
)

func TestRetryableOnlyForTransientFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"service overloaded", genai.APIError{Code: 503}, true},
		{"rate limited", genai.APIError{Code: 429}, true},
		{"gateway timeout", genai.APIError{Code: 504}, true},
		{"rejected schema", genai.APIError{Code: 400}, false},
		{"unknown model", genai.APIError{Code: 404}, false},
		{"bad api key", genai.APIError{Code: 403}, false},
		{"not an api error", errors.New("boom"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	calls := 0
	got, err := withRetry(context.Background(), 4, func() (string, error) {
		calls++
		if calls < 3 {
			return "", genai.APIError{Code: 503, Message: "high demand"}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if got != "ok" {
		t.Errorf("result = %q", got)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3", calls)
	}
}

// A schema the service rejects fails the same way every time. Retrying it turns
// a clear error into a slow one.
func TestWithRetryDoesNotRetryPermanentFailure(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), 4, func() (string, error) {
		calls++
		return "", genai.APIError{Code: 400, Message: "invalid schema"}
	})
	if err == nil {
		t.Fatal("withRetry hid a permanent failure")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1", calls)
	}
}

func TestWithRetryGivesUpAndReturnsLastError(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), 3, func() (string, error) {
		calls++
		return "", genai.APIError{Code: 503, Message: "high demand"}
	})
	if err == nil {
		t.Fatal("withRetry returned no error after exhausting attempts")
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3", calls)
	}

	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 503 {
		t.Errorf("lost the service's own error: %v", err)
	}
}

func TestWithRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := withRetry(ctx, 4, func() (string, error) {
		return "", genai.APIError{Code: 503}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("withRetry = %v, want context.Canceled", err)
	}
}

func TestBackoffGrowsAndStaysBounded(t *testing.T) {
	var previous time.Duration
	for attempt := range 8 {
		delay := backoff(attempt)
		if delay > maxDelay {
			t.Errorf("backoff(%d) = %v, exceeds the %v ceiling", attempt, delay, maxDelay)
		}
		if delay <= 0 {
			t.Errorf("backoff(%d) = %v, want a positive delay", attempt, delay)
		}
		if attempt < 4 && delay < previous {
			t.Errorf("backoff(%d) = %v shrank below the previous %v", attempt, delay, previous)
		}
		previous = delay
	}
}
