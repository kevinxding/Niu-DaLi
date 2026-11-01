package common

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RelayErrorPassthroughHeader overrides RelayErrorPassthrough for one request.
// "true" forwards upstream errors verbatim, "false" restores the privacy
// template. Unset or unparsable values fall back to the global setting.
const RelayErrorPassthroughHeader = "X-Error-Passthrough"

// RelayErrorPassthroughNoHeader is the inverted spelling of the same switch:
// operators ask for it as "X-Error-No", where true means "do not hand the
// upstream error back". Its default (absent/false) therefore matches the
// pass-through default. Unlike the header above, this one is negative, so the
// parsed value is inverted before use.
const RelayErrorPassthroughNoHeader = "X-Error-No"

// RelayErrorPassthroughFor resolves the effective error pass-through mode for a
// request. getHeader is called with the two header names above; passing nil
// reads the global setting only.
func RelayErrorPassthroughFor(getHeader func(string) string) bool {
	if getHeader != nil {
		if value, ok := parseOptionalBool(getHeader(RelayErrorPassthroughHeader)); ok {
			return value
		}
		if value, ok := parseOptionalBool(getHeader(RelayErrorPassthroughNoHeader)); ok {
			return !value
		}
	}
	return RelayErrorPassthrough
}

// parseOptionalBool reports a recognised boolean and whether the value carried
// one. An unknown value is ignored rather than treated as false, so a typo
// cannot silently turn error redaction back on.
func parseOptionalBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "yes", "on":
		return true, true
	case "0", "f", "false", "no", "off":
		return false, true
	}
	return false, false
}

// upstreamResponseIdPrefixes maps a vendor-issued response id prefix onto the
// protocol-neutral one. A response id is the one success-path field that names
// the upstream's inference backend verbatim, while clients treat it as an
// opaque conversation handle, so only the vendor marker is dropped and the
// remaining id is kept unique. Adding an entry is a client-visible change and
// must be declared alongside the other sanitization exceptions.
var upstreamResponseIdPrefixes = []struct{ vendor, neutral string }{
	{"msg_bdrk_", "msg_"},
}

// NormalizeUpstreamResponseId removes the upstream backend marker from a
// protocol response id. An id that carries no known marker is returned as-is.
func NormalizeUpstreamResponseId(id string) string {
	for _, replacement := range upstreamResponseIdPrefixes {
		if strings.HasPrefix(id, replacement.vendor) {
			return replacement.neutral + strings.TrimPrefix(id, replacement.vendor)
		}
	}
	return id
}

// upstreamDiagnosticPatterns name the upstream's inference backend, its cloud
// account or a hop in its proxy chain. They are stripped from client-visible
// failure text: the caller keeps the upstream's diagnosis, never its identity.
var upstreamDiagnosticPatterns = []*regexp.Regexp{
	// The AWS SDK v2 "operation error" wrapper as an upstream body spells it:
	// InvokeModel: operation error Bedrock Runtime: InvokeModel, https response error StatusCode: 400,
	regexp.MustCompile(`(?:[A-Za-z0-9]+: )?operation error [^,]+: [^,]+, ?https response error StatusCode: \d+,?\s*`),
	// The cloud-side request id, which correlates with the upstream's own cloud account.
	regexp.MustCompile(`\bRequestID:\s*[0-9a-fA-F][0-9a-fA-F-]{6,}\s*,?\s*`),
	// Cloud SDK exception classes. Every one of them ends in "Exception" and the
	// class name carries the service, so the whole token goes; the schema error
	// that follows is the part a caller can act on.
	regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*Exception:\s*`),
	// Bare service hostnames, for the case where one appears outside the wrapper.
	regexp.MustCompile(`\b[a-z0-9][a-z0-9.-]*amazonaws\.com[a-z0-9./_-]*`),
	// Every hop of a relay chain decorates the same message with its own id. The
	// gateway republishes exactly one id, its own, after this runs.
	regexp.MustCompile(`(?:\s*\(request id: [^()]*\))+`),
}

// upstreamBalanceDiagnostics recognises failure text that describes the
// upstream's own account rather than the caller's request.
//
// A balance the upstream reports about itself always pairs an amount with a
// balance word, in one of two orders: "用户剩余额度: ＄78.48", "balance: $12.00",
// "$3 in balance". Requiring that *pair* is what keeps an actionable schema
// error — which may legitimately quote a currency amount of its own, as in
// "invalid value '$5' for field billing_address" — from being replaced by a
// message that hides the caller's actual mistake.
//
// This is a heuristic on free text, so it is deliberately conservative: a
// balance stated with no amount, or with an unrecognised currency, is left as
// the upstream wrote it rather than guessed at.
var upstreamBalanceDiagnostics = []*regexp.Regexp{
	// "额度: ＄78.48", "balance: $12.00", "quota is $5"
	regexp.MustCompile(`(?:余额|额度|预扣|quota|balance)[^;,\n]{0,12}?[＄$￥¥€£]\s?\d`),
	// "$78.48 remaining balance", "$3 in balance"
	regexp.MustCompile(`[＄$￥¥€£]\s?\d[\d.,]*[^;,\n]{0,12}?(?:余额|额度|quota|balance)`),
}

// upstreamAccountFailureMessage replaces an account diagnostic. The condition
// is real and worth reporting; the amounts and the balances are not.
const upstreamAccountFailureMessage = "This request cannot be funded. Contact support with the request ID."

func isUpstreamAccountDiagnostic(message string) bool {
	for _, pattern := range upstreamBalanceDiagnostics {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// ScrubUpstreamDiagnostics removes upstream identity from failure text while
// keeping the diagnosis a client needs in order to fix its call. It is applied
// to protocol error messages only; generated text is never traversed.
func ScrubUpstreamDiagnostics(message string) string {
	if message == "" {
		return message
	}
	if isUpstreamAccountDiagnostic(message) {
		return upstreamAccountFailureMessage
	}
	for _, pattern := range upstreamDiagnosticPatterns {
		message = pattern.ReplaceAllString(message, "")
	}
	return strings.TrimSpace(message)
}

// PublicRelayFailureMessage keeps upstream diagnostics out of client responses.
func PublicRelayFailureMessage(requestID string) string {
	return MessageWithRequestId("The model request failed. Contact support with the request ID.", requestID)
}

// PublicRelayJSON removes routing/accounting provenance from a client copy.
// Only protocol envelopes are inspected: generated text, tool arguments, image
// data and user metadata must never be traversed or rewritten. Callers retain
// the original upstream bytes for usage accounting and server-side diagnostics.
//
// Gateway provenance and upstream identity are removed unconditionally; when
// redactErrors is set, upstream failure text is replaced as well. See
// PassthroughRelayJSON for the pass-through contract.
func PublicRelayJSON(data []byte, requestID string, redactErrors bool) []byte {
	return publicRelayJSON(data, requestID, redactErrors, redactErrors)
}

// PassthroughRelayJSON is PublicRelayJSON for relay error pass-through. It
// strips gateway provenance (channel names, upstream URLs, cost fields) and the
// upstream's own identity (backend markers in the response id, cloud request
// ids, account diagnostics), but leaves the upstream's diagnosis intact — its
// status, its error codes and the text that says what was wrong with the
// request — so a client can still act on it.
//
// This is the declared exception to byte-for-byte forwarding on the documented
// path: an upstream that names its own backend, account or proxy chain in an
// error body does not get to publish that through us.
func PassthroughRelayJSON(data []byte, requestID string) []byte {
	return publicRelayJSON(data, requestID, false, false)
}

// scrubMessageAt rewrites one client-visible failure message. folded is the raw
// metadata this gateway appended to it, or "" when there is none to remove.
func scrubMessageAt(data []byte, path, folded, requestID string) []byte {
	message := gjson.GetBytes(data, path)
	if message.Type != gjson.String {
		return data
	}
	text := message.String()
	if folded != "" {
		// Only ever removes the exact text this gateway appended; a message that
		// does not end with it is returned untouched.
		text = strings.TrimSuffix(text, " ("+folded+")")
	}
	scrubbed := ScrubUpstreamDiagnostics(text)
	if scrubbed == "" {
		// The whole message was upstream identity; fall back to the operator
		// template rather than publishing an empty or id-only message.
		scrubbed = PublicRelayFailureMessage("")
	}
	decorated := MessageWithRequestId(scrubbed, requestID)
	if decorated == message.String() {
		return data
	}
	if clean, err := sjson.SetBytes(data, path, decorated); err == nil {
		return clean
	}
	return data
}

// scrubUpstreamIdentity removes upstream vendor fingerprints from the only two
// client-visible places on a relayed body that can carry them: protocol
// response ids and failure text.
//
// Generated content, tool arguments, media and user metadata are never
// traversed. The upstream's language belongs to the caller and rewriting it
// would corrupt the protocol; the upstream's identity does not.
func scrubUpstreamIdentity(data []byte, prefixes []string, requestID string) []byte {
	for _, prefix := range prefixes {
		id := gjson.GetBytes(data, prefix+"id")
		if id.Type != gjson.String {
			continue
		}
		normalized := NormalizeUpstreamResponseId(id.String())
		if normalized == id.String() {
			continue
		}
		if clean, err := sjson.SetBytes(data, prefix+"id", normalized); err == nil {
			data = clean
		}
	}

	for _, prefix := range []string{"", "response.", "message."} {
		errorPath := prefix + "error"
		// `metadata` and `debug` are upstream side-channels with no protocol
		// meaning on any supported format, so they go under every error policy.
		// `details` deliberately stays: Google-shaped envelopes define it
		// (fieldViolations), so removing it would delete protocol content. The
		// redaction policy drops it as part of discarding the error body anyway.
		//
		// The metadata text can be present a second time inside the message:
		// rebuilding an upstream error through WithOpenAIError folds it in as a
		// trailing parenthetical, so dropping the field alone leaves a copy.
		folded := gjson.GetBytes(data, errorPath+".metadata")
		for _, field := range []string{"metadata", "debug"} {
			if !gjson.GetBytes(data, errorPath+"."+field).Exists() {
				continue
			}
			if clean, err := sjson.DeleteBytes(data, errorPath+"."+field); err == nil {
				data = clean
			}
		}
		data = scrubMessageAt(data, errorPath+".message", folded.Raw, requestID)
	}

	// OpenAI Responses and its SSE form report a failure flat: `type` and
	// `message` sit beside each other with no `error` object, so the pass above
	// never reaches them. A success frame of the same shape carries an object at
	// `message`, which the string check rejects.
	for _, prefix := range []string{"", "response."} {
		if !strings.Contains(gjson.GetBytes(data, prefix+"type").String(), "error") {
			continue
		}
		if gjson.GetBytes(data, prefix+"error").Exists() {
			continue
		}
		data = scrubMessageAt(data, prefix+"message", "", requestID)
	}
	return data
}

func publicRelayJSON(data []byte, requestID string, redactErrors, redactFailureFields bool) []byte {
	if !gjson.ValidBytes(data) {
		return data
	}
	if gjson.ParseBytes(data).IsArray() {
		items := gjson.ParseBytes(data).Array()
		public := make([]RawMessage, len(items))
		for i, item := range items {
			public[i] = publicRelayJSON([]byte(item.Raw), requestID, redactErrors, redactFailureFields)
		}
		if clean, err := Marshal(public); err == nil {
			return clean
		}
		return data
	}
	prefixes := []string{"", "response.", "message."}
	if gjson.GetBytes(data, "data.task_id").Exists() {
		prefixes = append(prefixes, "data.")
	}
	for _, prefix := range prefixes {
		for _, key := range []string{"system_fingerprint", "usage_source", "usage_semantic", "upstream", "upstream_url", "upstream_request_id", "trace_id", "request_id", "provider_metadata", "provider", "provider_name", "channel_id", "channel_name", "usage.usage_source", "usage.usage_semantic", "usage.billing_usage", "usage.cost", "usage.cost_details", "usageMetadata.usage_source", "usageMetadata.usage_semantic", "usageMetadata.provider"} {
			path := prefix + key
			if !gjson.GetBytes(data, path).Exists() {
				continue
			}
			if clean, err := sjson.DeleteBytes(data, path); err == nil {
				data = clean
			}
		}
	}

	// Upstream identity never survives into a client copy, whichever error policy
	// is in force: pass-through publishes the upstream's diagnosis, not the name
	// of the company and account that produced it.
	data = scrubUpstreamIdentity(data, prefixes, requestID)

	if redactFailureFields {
		for _, prefix := range prefixes {
			for _, field := range []string{"fail_reason", "failReason", "failure_reason", "error_message"} {
				value := gjson.GetBytes(data, prefix+field)
				if !value.Exists() || value.Type == gjson.Null || value.Raw == `""` {
					continue
				}
				if clean, err := sjson.SetBytes(data, prefix+field, PublicRelayFailureMessage(requestID)); err == nil {
					data = clean
				}
			}
		}
	}

	if !redactErrors {
		return data
	}
	// Upstream failures can contain URLs, credentials, HTML and provider names.
	// Keep the protocol's error shape and correlation IDs, not its diagnostic text.
	for _, prefix := range []string{"", "response."} {
		path := prefix + "error"
		upstreamError := gjson.GetBytes(data, path)
		eventType := gjson.GetBytes(data, prefix+"type").String()
		if !strings.Contains(eventType, "error") && (!upstreamError.Exists() || upstreamError.Type == gjson.Null || upstreamError.Raw == `""` || upstreamError.Raw == `{}`) {
			continue
		}
		if !upstreamError.Exists() && eventType != "error" && eventType != "upstream_error" {
			continue
		}
		message := PublicRelayFailureMessage(requestID)
		code := "upstream_error"
		// These protocol codes drive client retry/cancellation behavior.
		switch candidate := upstreamError.Get("code").String(); candidate {
		case "invalid_input", "server_error", "invalid_request", "invalid_request_error", "access_denied", "insufficient_quota", "insufficient_user_quota", "rate_limit_exceeded", "response_not_found", "response_not_active", "response_already_completed", "context_length_exceeded", "model_not_found", "unsupported_parameter":
			code = candidate
		}
		errorType := "api_error"
		switch candidate := upstreamError.Get("type").String(); candidate {
		case "invalid_request_error", "server_error", "rate_limit_error", "authentication_error", "permission_error", "not_found_error", "overloaded_error":
			errorType = candidate
		}
		if !upstreamError.Exists() {
			// Responses SSE uses a flat error event, unlike WebSocket errors.
			if clean, err := sjson.SetBytes(data, prefix+"message", message); err == nil {
				data = clean
			}
			// Keep a standardized error code; never copy diagnostic fields.
			if candidate := gjson.GetBytes(data, prefix+"code").String(); candidate == "server_error" || candidate == "context_length_exceeded" || candidate == "invalid_request_error" {
				code = candidate
			}
			if clean, err := sjson.SetBytes(data, prefix+"code", code); err == nil {
				data = clean
			}
		} else {
			publicError := map[string]any{"code": code, "message": message}
			if upstreamError.Get("type").Exists() {
				publicError["type"] = errorType
			}
			if number := upstreamError.Get("code"); number.Type == gjson.Number && number.Int() >= 400 && number.Int() <= 599 && number.Float() == float64(number.Int()) {
				publicError["code"] = number.Int()
			}
			switch status := upstreamError.Get("status").String(); status {
			case "INVALID_ARGUMENT", "UNAUTHENTICATED", "PERMISSION_DENIED", "NOT_FOUND", "RESOURCE_EXHAUSTED", "INTERNAL", "UNAVAILABLE", "DEADLINE_EXCEEDED":
				publicError["status"] = status
			}
			if clean, err := sjson.DeleteBytes(data, prefix+"message"); err == nil {
				data = clean
			}
			errorJSON, err := Marshal(publicError)
			if err != nil {
				continue
			}
			if clean, err := sjson.SetRawBytes(data, path, errorJSON); err == nil {
				data = clean
			}
		}
		for _, field := range []string{"metadata", "debug", "details"} {
			if clean, err := sjson.DeleteBytes(data, prefix+field); err == nil {
				data = clean
			}
		}
	}
	return data
}

// PublicRelaySSE filters complete SSE frames, including multiline data events.
// Comments and event framing are kept; [DONE] is not interpreted as JSON.
//
// redactErrors selects the error policy for the payload: true replaces upstream
// failure text with the privacy template, false (relay error pass-through)
// removes gateway provenance only and keeps the upstream error as written.
func PublicRelaySSE(frame []byte, requestID string, redactErrors bool) []byte {
	newline := []byte("\n")
	if bytes.Contains(frame, []byte("\r\n")) {
		newline = []byte("\r\n")
	}
	lines := bytes.Split(frame, newline)
	var payload []byte
	errorEvent := false
	first := -1
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("event:")) && strings.Contains(strings.ToLower(string(line[6:])), "error") {
			errorEvent = true
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first >= 0 {
			payload = append(payload, '\n')
		} else {
			first = i
		}
		payload = append(payload, bytes.TrimPrefix(line[5:], []byte(" "))...)
	}
	if first < 0 {
		return frame
	}
	input := payload
	if errorEvent && redactErrors {
		// The client needs a parseable error envelope to react to. Under
		// pass-through an unparsable frame is forwarded as the upstream wrote it.
		if !gjson.ValidBytes(input) {
			input = []byte(`{"type":"error"}`)
		} else if clean, err := sjson.SetBytes(input, "type", "error"); err == nil {
			input = clean
		}
	}
	clean := publicRelayJSON(input, requestID, redactErrors, redactErrors)
	if bytes.Equal(clean, payload) {
		return frame
	}
	var out bytes.Buffer
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if i != first {
				continue
			}
			out.WriteString("data: ")
			// Sanitization can retain newlines from a multiline upstream JSON object.
			out.Write(bytes.ReplaceAll(clean, []byte("\n"), append(append([]byte{}, newline...), []byte("data: ")...)))
		} else {
			out.Write(line)
		}
		if i < len(lines)-1 {
			out.Write(newline)
		}
	}
	return out.Bytes()
}

// relayProtocolHeaderFamilies lists header families whose names carry a
// variable suffix and therefore cannot be enumerated as exact names. Anthropic
// reports per-window rate limits this way, and those numbers decide whether a
// client should back off, so they are protocol-visible. None of them carry
// credentials or upstream topology.
var relayProtocolHeaderFamilies = []string{"Anthropic-Ratelimit-"}

// RelayProtocolHeader is an allowlist, so new infrastructure fingerprint headers
// cannot accidentally become public. Gateway-owned CORS/security/request headers
// are applied separately; an upstream must not overwrite them.
func RelayProtocolHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Content-Type", "Content-Disposition", "Content-Encoding", "Cache-Control", "Expires", "Last-Modified", "Etag", "Retry-After", "Content-Range", "Accept-Ranges", "X-Accel-Buffering", "X-Reasoning-Included", "X-Codex-Turn-State":
		return true
	}
	for _, family := range relayProtocolHeaderFamilies {
		if strings.HasPrefix(canonical, family) {
			return true
		}
	}
	return false
}
