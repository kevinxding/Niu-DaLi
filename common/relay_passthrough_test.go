package common

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRelayErrorPassthroughFor locks the operator-facing contract of the relay
// error pass-through switch.
//
// Requirements it encodes:
//   - the default forwards upstream errors, because a client cannot fix a call
//     it is not allowed to diagnose;
//   - X-Error-Passthrough is the positive spelling and X-Error-Passthrough
//     wins when both headers are present;
//   - X-Error-No is the negative spelling operators asked for, so "false" (and
//     an absent header) means pass-through;
//   - an unparsable value falls back to the global setting instead of silently
//     switching redaction back on.
func TestRelayErrorPassthroughFor(t *testing.T) {
	previous := RelayErrorPassthrough
	t.Cleanup(func() { RelayErrorPassthrough = previous })

	header := func(values map[string]string) func(string) string {
		headers := http.Header{}
		for name, value := range values {
			headers.Set(name, value)
		}
		return headers.Get
	}

	for _, tc := range []struct {
		name           string
		global         bool
		headers        map[string]string
		wantPassthrough bool
	}{
		{"default is pass-through", true, nil, true},
		{"global redaction is honoured", false, nil, false},
		{"positive header turns it off", true, map[string]string{"X-Error-Passthrough": "false"}, false},
		{"positive header turns it on", false, map[string]string{"X-Error-Passthrough": "true"}, true},
		{"positive header accepts 1", false, map[string]string{"X-Error-Passthrough": "1"}, true},
		{"positive header is case insensitive", true, map[string]string{"x-error-passthrough": "FALSE"}, false},
		{"legacy header true hides errors", true, map[string]string{"X-Error-No": "true"}, false},
		{"legacy header false passes errors through", false, map[string]string{"X-Error-No": "false"}, true},
		{"positive header wins when both are set", true, map[string]string{"X-Error-Passthrough": "true", "X-Error-No": "true"}, true},
		{"unparsable positive header keeps the global setting", true, map[string]string{"X-Error-Passthrough": "maybe"}, true},
		{"unparsable legacy header keeps the global setting", false, map[string]string{"X-Error-No": "sure"}, false},
		{"empty header keeps the global setting", true, map[string]string{"X-Error-Passthrough": ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RelayErrorPassthrough = tc.global
			require.Equal(t, tc.wantPassthrough, RelayErrorPassthroughFor(header(tc.headers)))
		})
	}

	t.Run("nil getter reads the global setting", func(t *testing.T) {
		RelayErrorPassthrough = true
		require.True(t, RelayErrorPassthroughFor(nil))
		RelayErrorPassthrough = false
		require.False(t, RelayErrorPassthroughFor(nil))
	})
}

// TestPassthroughRelayJSONKeepsUpstreamDiagnostics separates the two jobs of the
// sanitizer: gateway provenance must never reach a client, upstream error text
// must whenever pass-through is on.
func TestPassthroughRelayJSONKeepsUpstreamDiagnostics(t *testing.T) {
	original := []byte(`{"error":{"type":"invalid_request_error","message":"upstream said no","metadata":{"hint":"keep"}},"channel_name":"private-provider","upstream_url":"https://private.invalid","system_fingerprint":"private-fingerprint","usage":{"cost":0.4},"usage_source":"private-worker"}`)

	clean := PassthroughRelayJSON(original)
	require.Contains(t, string(clean), "upstream said no")
	require.Contains(t, string(clean), "invalid_request_error")
	require.Contains(t, string(clean), "keep")
	for _, leaked := range []string{"private-provider", "private.invalid", "private-fingerprint", `"usage_source"`, `"cost"`} {
		require.NotContains(t, string(clean), leaked)
	}
	// The caller's copy must be untouched so accounting still sees provenance.
	require.Contains(t, string(original), "private-provider")
}

// TestPublicRelayJSONRedactsWhenAsked is the mirror of the test above: the
// privacy policy still has to work when an operator turns pass-through off.
func TestPublicRelayJSONRedactsWhenAsked(t *testing.T) {
	original := []byte(`{"error":{"type":"invalid_request_error","message":"upstream said no"},"channel_name":"private-provider"}`)

	clean := string(PublicRelayJSON(original, "local-request", true))
	require.NotContains(t, clean, "upstream said no")
	require.Contains(t, clean, "local-request")
	require.NotContains(t, clean, "private-provider")
}
