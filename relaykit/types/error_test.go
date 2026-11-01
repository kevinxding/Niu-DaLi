package types

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestToClaudeErrorAlwaysNamesAType pins the contract that broke in production.
//
// An upstream sitting in front of Bedrock rejects a request with an error body
// that carries no code of its own, and the envelope used to render that absence
// as the literal "<nil>". Anthropic clients branch on error.type, so the field
// has to name a type they can act on in every case.
func TestToClaudeErrorAlwaysNamesAType(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		code       any
		want       string
	}{
		{"a rejected call", http.StatusBadRequest, nil, "invalid_request_error"},
		{"an oversized request", http.StatusRequestEntityTooLarge, nil, "request_too_large"},
		{"a forbidden call", http.StatusForbidden, nil, "permission_error"},
		{"an unknown route", http.StatusNotFound, nil, "not_found_error"},
		{"a rate limit", http.StatusTooManyRequests, nil, "rate_limit_error"},
		{"an upstream overload", statusCodeOverloaded, nil, "overloaded_error"},
		{"an upstream fault", http.StatusBadGateway, nil, "api_error"},
		{"an explicit string code", http.StatusBadRequest, "invalid_request", "invalid_request"},
		{"an explicit ErrorCode", http.StatusBadRequest, ErrorCodeInvalidRequest, "invalid_request"},
		{"an empty string code falls back to the status", http.StatusForbidden, "", "permission_error"},
		{"an empty ErrorCode falls back to the status", http.StatusForbidden, ErrorCode(""), "permission_error"},
		{"a numeric code is not a type", http.StatusNotFound, 404, "not_found_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiError := WithOpenAIError(OpenAIError{Message: "upstream said no", Code: tc.code}, tc.statusCode)

			got := apiError.ToClaudeError()

			require.Equal(t, tc.want, got.Type)
			require.NotEqual(t, "<nil>", got.Type)
			require.NotEmpty(t, got.Type)
		})
	}
}

// TestClaudeErrorTypeKeepsACodeThisProcessAssigned covers the codes that are not
// plain strings. An error rebuilt locally through NewOpenAIError carries an
// ErrorCode; dropping it here would silently replace the gateway's own
// classification with one derived from a status that never described it.
func TestClaudeErrorTypeKeepsACodeThisProcessAssigned(t *testing.T) {
	rebuilt := NewOpenAIError(errors.New("upstream said no"), ErrorCodeInvalidRequest, http.StatusInternalServerError)

	require.Equal(t, "invalid_request", rebuilt.ToClaudeError().Type)
	require.Equal(t, "invalid_request", rebuilt.ToOpenAIError().Type)
}

// TestErrorEnvelopeTypesSurviveDirectConstruction covers envelopes that are not
// built through WithOpenAIError or WithClaudeError: the boundary has to produce a
// usable type no matter how the error was assembled.
func TestErrorEnvelopeTypesSurviveDirectConstruction(t *testing.T) {
	unclassified := &NewAPIError{Err: errors.New("upstream said no"), StatusCode: http.StatusBadGateway}
	require.Equal(t, "api_error", unclassified.ToClaudeError().Type)
	require.Equal(t, "api_error", unclassified.ToOpenAIError().Type)

	claude := &NewAPIError{
		errorType:  ErrorTypeClaudeError,
		RelayError: ClaudeError{Message: "upstream said no"},
		StatusCode: http.StatusTooManyRequests,
	}
	require.Equal(t, "rate_limit_error", claude.ToOpenAIError().Type)
}

// TestOpenAIErrorEnvelopeAlwaysCarriesACode covers the other half of the same
// rule: a null code is as unusable to a client as a missing one, while a code the
// upstream did supply is passed through untouched.
func TestOpenAIErrorEnvelopeAlwaysCarriesACode(t *testing.T) {
	absent := WithOpenAIError(OpenAIError{Message: "upstream said no"}, http.StatusBadGateway)
	require.Equal(t, "unknown_error", absent.ToOpenAIError().Code)

	numeric := WithOpenAIError(OpenAIError{Message: "upstream said no", Code: 429}, http.StatusTooManyRequests)
	require.Equal(t, 429, numeric.ToOpenAIError().Code)
	require.Equal(t, "rate_limit_error", numeric.ToClaudeError().Type)
}

// TestPlaceholderCodeDoesNotBecomeTheErrorType guards the seam between the two
// rules above. Filling in `code` at construction time makes the placeholder
// indistinguishable from a code the upstream actually sent, and the type
// derivation then reports "unknown_error" for every status instead of mapping it.
// The code is therefore filled at emission, where it cannot feed the derivation.
func TestPlaceholderCodeDoesNotBecomeTheErrorType(t *testing.T) {
	apiError := WithOpenAIError(OpenAIError{Message: "upstream said no"}, http.StatusTooManyRequests)

	// The OpenAI envelope mirrors whatever type the upstream supplied, falling back
	// to the generic upstream_error, so its type is never empty either.
	envelope := apiError.ToOpenAIError()
	require.Equal(t, unknownErrorCode, envelope.Code)
	require.Equal(t, "upstream_error", envelope.Type)

	// The Claude envelope carries no code of its own, so it derives an actionable
	// type from the status instead of inheriting the placeholder.
	require.Equal(t, "rate_limit_error", apiError.ToClaudeError().Type)
}
