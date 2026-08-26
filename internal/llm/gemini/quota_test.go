package gemini

import (
	"errors"
	"testing"

	"google.golang.org/genai"
)

// Being over the per-minute limit and being out of allowance for the day both
// arrive as 429, and they need opposite responses: one clears by waiting, the
// other cannot clear before tomorrow.
func TestQuotaExhaustedDistinguishesDailyFromTransient(t *testing.T) {
	daily := genai.APIError{
		Code:    429,
		Message: "You exceeded your current quota",
		Details: []map[string]any{{
			"violations": []any{map[string]any{
				"quotaId":     "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
				"quotaMetric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
			}},
		}},
	}
	if !quotaExhausted(daily) {
		t.Error("a per-day quota violation was not recognised")
	}

	perMinute := genai.APIError{
		Code:    429,
		Message: "Too many requests",
		Details: []map[string]any{{
			"violations": []any{map[string]any{
				"quotaId": "GenerateRequestsPerMinutePerProjectPerModel-FreeTier",
			}},
		}},
	}
	if quotaExhausted(perMinute) {
		t.Error("a per-minute limit was mistaken for an exhausted daily quota")
	}

	// A per-minute limit is transient and must still be retried.
	if !retryable(perMinute) {
		t.Error("a per-minute limit should be retried")
	}

	for _, err := range []error{
		genai.APIError{Code: 503},
		genai.APIError{Code: 400},
		errors.New("boom"),
	} {
		if quotaExhausted(err) {
			t.Errorf("%v was mistaken for an exhausted quota", err)
		}
	}
}

func TestQuotaExhaustedReadsTheMessageWhenDetailsAreAbsent(t *testing.T) {
	err := genai.APIError{
		Code:    429,
		Message: "Quota exceeded: GenerateRequestsPerDayPerProjectPerModel-FreeTier",
	}
	if !quotaExhausted(err) {
		t.Error("a per-day quota named only in the message was not recognised")
	}
}

func TestChainKeepsOrderAndDropsDuplicates(t *testing.T) {
	models := chain("primary", []string{"primary", "second", "", "third"})

	want := []string{"primary", "second", "third"}
	if len(models) != len(want) {
		t.Fatalf("chain() = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("chain() = %v, want %v", models, want)
		}
	}
}

func TestNextModelAdvancesOnceAndStops(t *testing.T) {
	p := &Provider{models: []string{"a", "b", "c"}}

	if got := p.Model(); got != "a" {
		t.Fatalf("Model() = %q, want a", got)
	}

	next, ok := p.nextModel("a")
	if !ok || next != "b" {
		t.Fatalf("nextModel(a) = %q, %v", next, ok)
	}

	// A second caller still holding the exhausted model must land where the
	// first one moved to, not skip past it.
	next, ok = p.nextModel("a")
	if !ok || next != "b" {
		t.Fatalf("a stale caller skipped a model: %q, %v", next, ok)
	}

	if next, ok = p.nextModel("b"); !ok || next != "c" {
		t.Fatalf("nextModel(b) = %q, %v", next, ok)
	}
	if _, ok = p.nextModel("c"); ok {
		t.Error("nextModel reported another model past the end of the chain")
	}
}
