package gemini

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/genai"
)

// quotaExhausted reports whether an error means this model has no free
// allowance left today, as opposed to the caller simply going too fast.
//
// The distinction matters because the two need opposite responses. A burst over
// the per-minute limit clears on its own, so waiting is right. A daily quota
// does not clear before tomorrow, so waiting is the one thing that cannot work:
// the only way forward is a different model.
func quotaExhausted(err error) bool {
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusTooManyRequests {
		return false
	}

	// The quota identifier names its own window, as in
	// "GenerateRequestsPerDayPerProjectPerModel-FreeTier".
	for _, detail := range apiErr.Details {
		violations, ok := detail["violations"].([]any)
		if !ok {
			continue
		}
		for _, raw := range violations {
			violation, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := violation["quotaId"].(string); ok && strings.Contains(id, "PerDay") {
				return true
			}
		}
	}

	// Some responses carry the window only in the message.
	return strings.Contains(apiErr.Message, "PerDay")
}
