package dto

import (
	"encoding/json"
	"testing"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The envelope has to survive two different round trips: declared fields are
// typed and validated, while a top-level field the struct does not track must be
// preserved verbatim instead of being silently dropped when the body is rebuilt.
// These tests pin both halves of that contract.

func TestClaudeRequestSplitsDeclaredFromUndeclaredTopLevelFields(t *testing.T) {
	body := []byte(`{"model":"claude-3-7-sonnet","max_tokens":1024,` +
		`"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	var req ClaudeRequest
	require.NoError(t, kitutil.Unmarshal(body, &req))

	assert.Equal(t, "claude-3-7-sonnet", req.Model)
	require.NotNil(t, req.MaxTokens)
	assert.Equal(t, uint(1024), *req.MaxTokens)
	require.Len(t, req.Messages, 1)

	// Only the undeclared name lands in Extra; declared names must not be copied
	// there, otherwise they would be written twice on the way out.
	require.Len(t, req.Extra, 1)
	assert.JSONEq(t, `"high"`, string(req.Extra["reasoning_effort"]))
	assert.NotContains(t, req.Extra, "model")
	assert.NotContains(t, req.Extra, "max_tokens")
	assert.NotContains(t, req.Extra, "messages")
}

func TestClaudeRequestRoundTripKeepsUndeclaredFieldsFlat(t *testing.T) {
	body := []byte(`{"model":"claude-3-7-sonnet","max_tokens":64,` +
		`"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	var req ClaudeRequest
	require.NoError(t, kitutil.Unmarshal(body, &req))

	encoded, err := kitutil.Marshal(req)
	require.NoError(t, err)

	var flat map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &flat))
	assert.Equal(t, `"claude-3-7-sonnet"`, string(flat["model"]))
	assert.Equal(t, `"high"`, string(flat["reasoning_effort"]))
	// The carrier itself must never appear on the wire.
	assert.NotContains(t, flat, "Extra")

	// A client that echoes the body back gets the same envelope it sent.
	var echoed ClaudeRequest
	require.NoError(t, kitutil.Unmarshal(encoded, &echoed))
	require.Len(t, echoed.Extra, 1)
	assert.JSONEq(t, `"high"`, string(echoed.Extra["reasoning_effort"]))
}

func TestClaudeRequestWithoutExtraDropsUndeclaredFields(t *testing.T) {
	body := []byte(`{"model":"claude-3-7-sonnet","max_tokens":64,` +
		`"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	var req ClaudeRequest
	require.NoError(t, kitutil.Unmarshal(body, &req))
	require.Contains(t, req.Extra, "reasoning_effort")

	// Dropping Extra is how the gatekeeper channel setting removes the fields, so
	// the encoder must not resurrect them.
	req.DropUndeclaredFields()
	encoded, err := kitutil.Marshal(req)
	require.NoError(t, err)

	assert.NotContains(t, string(encoded), "reasoning_effort")
	assert.JSONEq(t, `{"model":"claude-3-7-sonnet","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		string(encoded))
}

// TestDropUndeclaredFieldsIsNilSafe covers the delegating channels, which hand a
// possibly nil envelope to the shared helper.
func TestDropUndeclaredFieldsIsNilSafe(t *testing.T) {
	var req *ClaudeRequest
	require.NotPanics(t, req.DropUndeclaredFields)
}

func TestClaudeRequestExtraCannotShadowDeclaredFields(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-3-7-sonnet",
		Extra: map[string]json.RawMessage{"model": json.RawMessage(`"shadow"`)},
	}

	encoded, err := kitutil.Marshal(req)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"claude-3-7-sonnet"}`, string(encoded))
	assert.NotContains(t, string(encoded), "shadow")
}

func TestClaudeRequestEmptyExtraEmitsOnlyTheDeclaredEnvelope(t *testing.T) {
	// The no-extra path is the common one and must not pay for the merge.
	encoded, err := kitutil.Marshal(ClaudeRequest{Model: "claude-3-7-sonnet"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"claude-3-7-sonnet"}`, string(encoded))
}
