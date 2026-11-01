package common

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
		name            string
		global          bool
		headers         map[string]string
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
// sanitizer: gateway provenance and upstream identity must never reach a client,
// the upstream's own diagnosis must whenever pass-through is on.
func TestPassthroughRelayJSONKeepsUpstreamDiagnostics(t *testing.T) {
	original := []byte(`{"error":{"type":"invalid_request_error","message":"upstream said no","metadata":{"hint":"drop"}},"channel_name":"private-provider","upstream_url":"https://private.invalid","system_fingerprint":"private-fingerprint","usage":{"cost":0.4},"usage_source":"private-worker"}`)

	clean := PassthroughRelayJSON(original, "local-request")
	require.Contains(t, string(clean), "upstream said no")
	require.Contains(t, string(clean), "invalid_request_error")
	require.Contains(t, string(clean), "local-request")
	for _, leaked := range []string{"private-provider", "private.invalid", "private-fingerprint", `"usage_source"`, `"cost"`, `"hint"`} {
		require.NotContains(t, string(clean), leaked)
	}
	// The caller's copy must be untouched so accounting still sees provenance.
	require.Contains(t, string(original), "private-provider")
}

// TestNormalizeUpstreamResponseId covers the success-path half of the identity
// contract: the only vendor marker a client would otherwise be handed is the
// inference backend's prefix on the response id.
func TestNormalizeUpstreamResponseId(t *testing.T) {
	const bedrock = "msg_bdrk_zbt6blkv7awvm7gaz5yt3xkwzmq4yoiebzqzluqexdzxsai24icq"
	require.Equal(t, "msg_zbt6blkv7awvm7gaz5yt3xkwzmq4yoiebzqzluqexdzxsai24icq",
		NormalizeUpstreamResponseId(bedrock))

	// Ids that carry no known vendor marker, including another vendor's, are
	// left alone rather than guessed at.
	for _, untouched := range []string{"msg_abc", "chatcmpl-abc", "response-id", "", "msg_bdrk"} {
		require.Equal(t, untouched, NormalizeUpstreamResponseId(untouched))
	}
}

// TestScrubUpstreamDiagnostics covers the failure-text half.
//
// The upstream in front of Bedrock reports its own SDK wrapper, its cloud
// request id, its exception class names and its account balances inside the
// error message. A client can act on the schema error; it has no business
// seeing which company, region or account produced it.
func TestScrubUpstreamDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{
			name:  "sdk wrapper, cloud request id and exception class are unwrapped",
			input: "InvokeModel: operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 400, RequestID: c05a1280-5f13-486a-93b8-c7ba005a8238, ValidationException: messages.0: user messages must have non-empty content",
			want:  "messages.0: user messages must have non-empty content",
		},
		{
			name:  "a multi hop relay chain collapses to nothing",
			input: "rejected (request id: hop-a) (request id: hop-b) (request id: hop-c)",
			want:  "rejected",
		},
		{
			name:  "an upstream account diagnostic is replaced wholesale",
			input: "预扣费额度失败, 用户剩余额度: ＄78.481940, 需要预扣费额度: ＄580.000022",
			want:  upstreamAccountFailureMessage,
		},
		{
			name:  "an english balance diagnostic is replaced too",
			input: "insufficient credit balance: $12.00 available, $580.00 required",
			want:  upstreamAccountFailureMessage,
		},
		{
			name:  "a balance stated amount-first is replaced",
			input: "The upstream rejected the request for $3 in balance",
			want:  upstreamAccountFailureMessage,
		},
		{
			name:  "every exception class is unwrapped, not just the well known ones",
			input: "ServiceQuotaExceededException: too many tokens",
			want:  "too many tokens",
		},
		{
			// A schema error may quote an amount of its own. Replacing it would hide
			// the caller's actual mistake behind an unrelated billing message.
			name:  "a validation error that mentions an amount is kept",
			input: "messages.1: invalid value '$5' for field billing_address",
			want:  "messages.1: invalid value '$5' for field billing_address",
		},
		{
			name:  "a parameter error that names a credit field is kept",
			input: "credit_balance is not a supported parameter for this model",
			want:  "credit_balance is not a supported parameter for this model",
		},
		{
			name:  "a bare service hostname is removed",
			input: "bedrock-runtime.us-east-1.amazonaws.com timed out",
			want:  "timed out",
		},
		{
			// Budget numbers are not balances: a caller has to read its own limits.
			name:  "a quota limit that names the caller is kept",
			input: "Number of request tokens has exceeded your per-minute rate limit",
			want:  "Number of request tokens has exceeded your per-minute rate limit",
		},
		{
			name:  "an empty message stays empty",
			input: "",
			want:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ScrubUpstreamDiagnostics(tc.input))
		})
	}
}

// TestRelayJSONHidesUpstreamIdentity is the end-to-end contract for one relayed
// document: the diagnosis and the generated content survive untouched, while
// every upstream fingerprint in the envelope is gone.
func TestRelayJSONHidesUpstreamIdentity(t *testing.T) {
	original := []byte(`{"id":"msg_bdrk_abc123","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Bedrock Runtime is a well known topic"}],"error":{"type":"invalid_request_error","message":"InvokeModel: operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 400, RequestID: 39f63b08-3af3-46d5-9929-1446ccffd920, ValidationException: messages.0: empty (request id: hop-a) (request id: hop-b)","metadata":{"provider":"private-provider"}}}`)

	clean := PassthroughRelayJSON(original, "local-request")

	require.Equal(t, "msg_abc123", gjson.GetBytes(clean, "id").String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(clean, "error.type").String())
	require.Equal(t, "messages.0: empty (request id: local-request)", gjson.GetBytes(clean, "error.message").String())
	require.False(t, gjson.GetBytes(clean, "error.metadata").Exists())
	// Generated content is never traversed, even when the model names the backend.
	require.Equal(t, "Bedrock Runtime is a well known topic", gjson.GetBytes(clean, "content.0.text").String())
	require.NotContains(t, string(clean), "private-provider")
	// Accounting and operator diagnostics still see the original bytes.
	require.Equal(t, "msg_bdrk_abc123", gjson.GetBytes(original, "id").String())
}

// TestRelayJSONHidesUpstreamIdentityWhenNothingElseRemains covers the degenerate
// case: once the identity is gone there is no diagnosis left, so the operator
// template is published rather than an empty message.
func TestRelayJSONHidesUpstreamIdentityWhenNothingElseRemains(t *testing.T) {
	only := []byte(`{"error":{"type":"invalid_request_error","message":"RequestID: 39f63b08-3af3-46d5-9929-1446ccffd920"}}`)
	require.Equal(t, PublicRelayFailureMessage("local-request"),
		gjson.GetBytes(PassthroughRelayJSON(only, "local-request"), "error.message").String())

	account := []byte(`{"error":{"type":"permission_error","message":"预扣费额度失败, 用户剩余额度: ＄78.481940 (request id: up-hop)"}}`)
	require.Equal(t, upstreamAccountFailureMessage+" (request id: local-request)",
		gjson.GetBytes(PassthroughRelayJSON(account, "local-request"), "error.message").String())
}

// TestRelayJSONScrubsFlatErrorEvents covers the shape that carries a failure
// without an `error` object: OpenAI Responses and its SSE form put `type` and
// `message` side by side, so a sanitizer that only walks `error.message` leaves
// that path open under the default pass-through policy.
func TestRelayJSONScrubsFlatErrorEvents(t *testing.T) {
	flat := []byte(`{"type":"error","code":"server_error","message":"InvokeModel: operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 400, RequestID: 39f63b08-3af3-46d5-9929-1446ccffd920, ValidationException: bad (request id: hop-a)"}`)

	clean := PassthroughRelayJSON(flat, "local-request")

	require.Equal(t, "bad (request id: local-request)", gjson.GetBytes(clean, "message").String())
	require.Equal(t, "server_error", gjson.GetBytes(clean, "code").String())
	require.NotContains(t, string(clean), "Bedrock Runtime")
	require.NotContains(t, string(clean), "39f63b08")

	// A success frame of the same shape has an object at `message` and must not be
	// treated as a failure: its id is normalized as usual, but nothing else about
	// it is rewritten.
	start := []byte(`{"type":"message_start","message":{"id":"msg_bdrk_stream","usage":{"input_tokens":1}}}`)
	startClean := PassthroughRelayJSON(start, "local-request")
	require.Equal(t, "msg_stream", gjson.GetBytes(startClean, "message.id").String())
	require.Equal(t, "1", gjson.GetBytes(startClean, "message.usage.input_tokens").String())

	// A frame whose type merely mentions a completed response keeps its message
	// text: only an error frame carries a failure there.
	done := []byte(`{"type":"response.completed","message":"all done"}`)
	require.Equal(t, "all done", gjson.GetBytes(PassthroughRelayJSON(done, "local-request"), "message").String())
}

// TestRelayJSONKeepsProtocolDetailsField guards against over-deletion: Google
// shaped envelopes define error.details, so it is protocol content rather than an
// upstream side-channel.
func TestRelayJSONKeepsProtocolDetailsField(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"bad argument","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"temperature"}]}],"metadata":{"provider":"private-provider"}}}`)

	clean := PassthroughRelayJSON(body, "local-request")

	require.True(t, gjson.GetBytes(clean, "error.details").Exists(), "details is protocol content")
	require.Equal(t, "temperature", gjson.GetBytes(clean, "error.details.0.fieldViolations.0.field").String())
	require.False(t, gjson.GetBytes(clean, "error.metadata").Exists())
	require.NotContains(t, string(clean), "private-provider")
}

// TestRelayJSONDropsFoldedUpstreamMetadata covers the second copy of the same
// text. Rebuilding an upstream error through WithOpenAIError folds its metadata
// into the message as a trailing parenthetical, so removing the field is not
// enough on its own.
func TestRelayJSONDropsFoldedUpstreamMetadata(t *testing.T) {
	original := []byte(`{"error":{"type":"invalid_request_error","message":"upstream said no ({\"provider\":\"private-provider\"})","metadata":{"provider":"private-provider"}}}`)

	clean := PassthroughRelayJSON(original, "local-request")

	require.Equal(t, "upstream said no (request id: local-request)", gjson.GetBytes(clean, "error.message").String())
	require.False(t, gjson.GetBytes(clean, "error.metadata").Exists())
	require.NotContains(t, string(clean), "private-provider")

	// A message that merely ends in a parenthetical of its own is not touched:
	// only the exact text this gateway appended is removed.
	plain := []byte(`{"error":{"type":"invalid_request_error","message":"bad request (see the docs)","metadata":{"provider":"private-provider"}}}`)
	require.Equal(t, "bad request (see the docs) (request id: local-request)",
		gjson.GetBytes(PassthroughRelayJSON(plain, "local-request"), "error.message").String())
}

// TestPublicRelayJSONHidesNestedResponseId covers the formats that nest the
// response id one level down, including the streaming frame that clients key
// their conversation state on.
func TestPublicRelayJSONHidesNestedResponseId(t *testing.T) {
	start := []byte(`{"type":"message_start","message":{"id":"msg_bdrk_stream","usage":{"input_tokens":1}}}`)
	clean := PublicRelayJSON(start, "local-request", false)
	require.Equal(t, "msg_stream", gjson.GetBytes(clean, "message.id").String())
	require.Equal(t, "1", gjson.GetBytes(clean, "message.usage.input_tokens").String())
}

// TestPublicRelaySSEHidesStreamingUpstreamIdentity covers the same document
// inside a frame: the sanitizer has to keep the framing byte-exact.
func TestPublicRelaySSEHidesStreamingUpstreamIdentity(t *testing.T) {
	frame := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bdrk_abc\",\"model\":\"claude-sonnet-4-6\"}}\n\n")
	clean := string(PublicRelaySSE(frame, "local-request", false))

	require.Contains(t, clean, "event: message_start")
	require.Contains(t, clean, `"id":"msg_abc"`)
	require.Contains(t, clean, `"model":"claude-sonnet-4-6"`)
	require.NotContains(t, clean, "msg_bdrk_")
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

// TestRelayProtocolHeader covers the response-header allowlist.
//
// It exists because the list was exact-name only, which cannot express
// Anthropic's rate-limit family: those names carry a per-window suffix, and a
// client has to read them to decide whether to back off. Everything that is not
// on the list stays private, including upstream-chosen credentials and the
// gateway's own provenance.
func TestRelayProtocolHeader(t *testing.T) {
	allowed := []string{
		"Content-Type",
		"Retry-After",
		"X-Accel-Buffering",
		"Anthropic-Ratelimit-Requests-Limit",
		"anthropic-ratelimit-requests-remaining",
		"Anthropic-Ratelimit-Tokens-Reset",
	}
	for _, name := range allowed {
		require.True(t, RelayProtocolHeader(name), "%s must be published", name)
	}

	denied := []string{
		"Authorization",
		"Set-Cookie",
		"Server",
		"Via",
		"X-Oneapi-Request-Id",
		"X-Upstream-Request-Id",
		"Anthropic-Version",
		"Content-Length",
		"",
	}
	for _, name := range denied {
		require.False(t, RelayProtocolHeader(name), "%s must stay private", name)
	}
}

// TestMessageWithRequestIdDecoratesOncePerChain covers the correlation id on the
// public error message, the other half of what a client can see in place of the
// upstream diagnostic.
//
// Relay chains put several gateways between the client and the upstream and each
// hop decorates the same message, so a multi-hop failure used to arrive carrying
// one id per hop instead of the originating one.
func TestMessageWithRequestIdDecoratesOncePerChain(t *testing.T) {
	const message = "The upstream rejected the request."
	decorated := MessageWithRequestId(message, "req-first-hop")
	require.Equal(t, "The upstream rejected the request. (request id: req-first-hop)", decorated)

	// Re-entering the chain with the same or a later hop must not append again.
	require.Equal(t, decorated, MessageWithRequestId(decorated, "req-first-hop"))
	require.Equal(t, decorated, MessageWithRequestId(decorated, "req-second-hop"))
	require.NotContains(t, MessageWithRequestId(decorated, "req-second-hop"), "req-second-hop")

	// A message with no id to add is returned untouched rather than decorated
	// with an empty one.
	require.Equal(t, "plain failure", MessageWithRequestId("plain failure", ""))
}
