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
	got, err := withRetry(context.Background(), 4, nil, func() (string, error) {
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
	_, err := withRetry(context.Background(), 4, nil, func() (string, error) {
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
	_, err := withRetry(context.Background(), 3, nil, func() (string, error) {
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

	_, err := withRetry(ctx, 4, nil, func() (string, error) {
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

// A rate limiter knows exactly when its window rolls over and says so. Retrying
// on a schedule of our own burns every attempt inside a window that had not
// reopened yet, which is what an interactive session experiences as a failure
// that a moment's patience would have avoided.
func TestRetryAfterPrefersTheDelayTheServiceAsksFor(t *testing.T) {
	err := genai.APIError{
		Code:    429,
		Message: "Too many requests",
		Details: []map[string]any{{
			"@type":      "type.googleapis.com/google.rpc.RetryInfo",
			"retryDelay": "53s",
		}},
	}

	delay, asked := retryAfter(err)
	if !asked {
		t.Fatal("the requested delay was not read")
	}
	// A second of slack so the retry lands after the window, not on its edge.
	if delay < 53*time.Second || delay > 55*time.Second {
		t.Errorf("delay = %v, want about 54s", delay)
	}

	// waitFor must prefer it over the backoff curve, which at attempt 0 is
	// about a second.
	chosen, requested := waitFor(err, 0)
	if !requested || chosen != delay {
		t.Errorf("waitFor chose %v (requested %v), want the service's %v", chosen, requested, delay)
	}
}

func TestRetryAfterFallsBackToBackoff(t *testing.T) {
	for _, err := range []error{
		genai.APIError{Code: 503},
		genai.APIError{Code: 429, Details: []map[string]any{{"@type": "something.else"}}},
		genai.APIError{Code: 429, Details: []map[string]any{{
			"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "not a duration",
		}}},
		errors.New("boom"),
	} {
		if _, asked := retryAfter(err); asked {
			t.Errorf("a delay was read out of %v", err)
		}
		if delay, requested := waitFor(err, 1); requested || delay <= 0 {
			t.Errorf("waitFor(%v) = %v, requested %v; want a positive backoff", err, delay, requested)
		}
	}
}

// A free tier that answers "come back tomorrow" must surface as an error a
// person can act on, not as a session that appears to hang.
func TestRequestedDelayIsCapped(t *testing.T) {
	err := genai.APIError{
		Code: 429,
		Details: []map[string]any{{
			"@type":      "type.googleapis.com/google.rpc.RetryInfo",
			"retryDelay": "3600s",
		}},
	}

	delay, asked := retryAfter(err)
	if !asked {
		t.Fatal("the requested delay was not read")
	}
	if delay > maxServerDelay+time.Second {
		t.Errorf("delay = %v, over the %v ceiling", delay, maxServerDelay)
	}
}

// A long hold has to announce itself, or it is indistinguishable from a hang.
func TestWaitIsAnnounced(t *testing.T) {
	var (
		announced bool
		gotDelay  time.Duration
		gotAsked  bool
	)

	calls := 0
	_, err := withRetry(context.Background(), 2,
		func(delay time.Duration, requested bool) {
			announced, gotDelay, gotAsked = true, delay, requested
		},
		func() (string, error) {
			calls++
			return "", genai.APIError{
				Code: 429,
				Details: []map[string]any{{
					"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.01s",
				}},
			}
		})

	if err == nil {
		t.Fatal("withRetry hid an exhausted rate limit")
	}
	if !announced {
		t.Fatal("the wait was never announced")
	}
	if !gotAsked {
		t.Error("the wait was not reported as one the service asked for")
	}
	if gotDelay <= 0 {
		t.Errorf("announced delay = %v", gotDelay)
	}
}
