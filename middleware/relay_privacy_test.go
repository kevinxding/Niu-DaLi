package middleware_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/relay/channel/minimax"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/QuantumNous/new-api/relay/channel/sub2api"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func privacyRouter(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(common.RequestIdKey, "local-request")
		c.Header(common.RequestIdKey, "local-request")
		c.Header("Access-Control-Allow-Origin", "https://customer.example")
	})
	engine.POST("/test", middleware.RouteTag("relay"), handler)
	return engine
}

// withErrorRedaction runs a test under the privacy policy, where upstream error
// text is replaced by the template message. The gateway default is pass-through
// (common.RelayErrorPassthrough), so every test that asserts redaction has to
// ask for it explicitly.
func withErrorRedaction(t *testing.T) {
	t.Helper()
	previous := common.RelayErrorPassthrough
	common.RelayErrorPassthrough = false
	t.Cleanup(func() { common.RelayErrorPassthrough = previous })
}

// withErrorPassthrough pins the gateway default for tests that assert that
// upstream diagnostics reach the client untouched.
func withErrorPassthrough(t *testing.T) {
	t.Helper()
	previous := common.RelayErrorPassthrough
	common.RelayErrorPassthrough = true
	t.Cleanup(func() { common.RelayErrorPassthrough = previous })
}

// TestRelayErrorPassthroughForwardsUpstreamDiagnostics pins the default policy:
// a rejected request must reach the client with the upstream's own status and
// reason, because that reason is what the client needs to fix its call. Gateway
// provenance (channel, upstream host, cost) still never leaves the gateway.
func TestRelayErrorPassthroughForwardsUpstreamDiagnostics(t *testing.T) {
	withErrorPassthrough(t)

	const upstream = `{"error":{"type":"invalid_request_error","message":"claude-opus-5-5 requires adaptive thinking; omit thinking or use thinking.type=adaptive"},"channel_name":"private-provider","upstream_url":"https://private.invalid","system_fingerprint":"private-fingerprint"}`
	engine := privacyRouter(func(c *gin.Context) {
		c.Set("channel_id", 28)
		c.Data(http.StatusBadRequest, "application/json", []byte(upstream))
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))

	assert.Equal(t, http.StatusBadRequest, recorder.Code, "the upstream status must not be rewritten")
	assert.Contains(t, recorder.Body.String(), "requires adaptive thinking", "the upstream reason must reach the client")
	assert.Contains(t, recorder.Body.String(), "invalid_request_error", "the upstream error type must be preserved")
	for _, leaked := range []string{"private-provider", "private.invalid", "private-fingerprint"} {
		assert.NotContains(t, recorder.Body.String(), leaked, "gateway provenance must stay private")
	}
}

// TestRelayErrorPassthroughKeepsNonJSONErrorBodies covers the case that used to
// be flattened to a template: an HTML or plain-text error from the upstream is
// returned as-is, still under its own status code.
func TestRelayErrorPassthroughKeepsNonJSONErrorBodies(t *testing.T) {
	withErrorPassthrough(t)

	const body = "<html>rate limited by upstream</html>"
	engine := privacyRouter(func(c *gin.Context) {
		c.Set("channel_id", 28)
		c.Data(http.StatusTooManyRequests, "text/html", []byte(body))
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))

	assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.Equal(t, body, recorder.Body.String())
}

// TestRelayErrorPassthroughHeaderOverride covers the per-request switch:
// X-Error-Passthrough is the positive spelling, X-Error-No the negative one the
// operators asked for, and an unparsable value must fall back to the global
// setting rather than silently enabling redaction.
func TestRelayErrorPassthroughHeaderOverride(t *testing.T) {
	const upstream = `{"error":{"type":"invalid_request_error","message":"upstream said no"}}`

	for _, tc := range []struct {
		name          string
		global        bool
		header, value string
		wantUpstream  bool
	}{
		{"global passthrough, no header", true, "", "", true},
		{"global redaction, no header", false, "", "", false},
		{"explicit passthrough off", true, common.RelayErrorPassthroughHeader, "false", false},
		{"explicit passthrough on", false, common.RelayErrorPassthroughHeader, "true", true},
		{"passthrough on wins over header in redaction mode", false, common.RelayErrorPassthroughHeader, "1", true},
		{"legacy X-Error-No true hides", true, common.RelayErrorPassthroughNoHeader, "true", false},
		{"legacy X-Error-No false passes through", false, common.RelayErrorPassthroughNoHeader, "false", true},
		{"unparsable value keeps the global setting", true, common.RelayErrorPassthroughHeader, "maybe", true},
		{"unparsable legacy value keeps the global setting", false, common.RelayErrorPassthroughHeader, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := common.RelayErrorPassthrough
			common.RelayErrorPassthrough = tc.global
			t.Cleanup(func() { common.RelayErrorPassthrough = previous })

			engine := privacyRouter(func(c *gin.Context) {
				c.Set("channel_id", 28)
				c.Data(http.StatusBadRequest, "application/json", []byte(upstream))
			})
			request := httptest.NewRequest(http.MethodPost, "/test", nil)
			if tc.header != "" && tc.value != "" {
				request.Header.Set(tc.header, tc.value)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			if tc.wantUpstream {
				assert.Contains(t, recorder.Body.String(), "upstream said no")
			} else {
				assert.NotContains(t, recorder.Body.String(), "upstream said no")
				assert.Contains(t, recorder.Body.String(), "local-request")
			}
		})
	}
}

// TestRelayErrorPassthroughKeepsSSEErrorFrames checks the streaming path, where
// the sanitizer works frame by frame.
func TestRelayErrorPassthroughKeepsSSEErrorFrames(t *testing.T) {
	withErrorPassthrough(t)

	const frame = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"upstream said no\"}}\n\n"
	engine := privacyRouter(func(c *gin.Context) {
		c.Set("channel_id", 28)
		c.Header("Content-Type", "text/event-stream")
		_, err := c.Writer.WriteString(frame)
		require.NoError(t, err)
		c.Writer.Flush()
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))

	assert.Contains(t, recorder.Body.String(), "upstream said no")
	assert.Contains(t, recorder.Body.String(), "event: error")

	previous := common.RelayErrorPassthrough
	common.RelayErrorPassthrough = false
	t.Cleanup(func() { common.RelayErrorPassthrough = previous })
	engine = privacyRouter(func(c *gin.Context) {
		c.Set("channel_id", 28)
		c.Header("Content-Type", "text/event-stream")
		_, err := c.Writer.WriteString(frame)
		require.NoError(t, err)
		c.Writer.Flush()
	})
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))
	assert.NotContains(t, recorder.Body.String(), "upstream said no")
	assert.Contains(t, recorder.Body.String(), "local-request")
}

func TestRelayPrivacyAcrossProtocols(t *testing.T) {
	withErrorRedaction(t)
	for _, tc := range []struct{ name, body string }{
		{"chat", `{"id":"completion-id","model":"customer-model","choices":[{"message":{"content":"usage_source private-worker"}}],"system_fingerprint":"private-fingerprint","usage":{"total_tokens":10,"usage_source":"private-worker","usage_semantic":"estimated","cost":0.4,"billing_usage":{"source":"private-provider"}}}`},
		{"responses", `{"type":"response.completed","response":{"id":"response-id","system_fingerprint":"private-fingerprint","usage":{"input_tokens":10,"usage_source":"private-worker"}},"output":[{"text":"usage_source private-worker"}]}`},
		{"claude", `{"type":"message_start","message":{"id":"message-id","usage":{"input_tokens":10,"usage_source":"private-worker"}},"content":[{"text":"usage_source private-worker"}]}`},
		{"gemini-array", `[{"provider":"private-provider","candidates":[{"content":{"parts":[{"text":"usage_source private-worker"}]}}],"usageMetadata":{"totalTokenCount":10}}]`},
		{"task", `{"id":"public-task-id","status":"failed","fail_reason":"private-failure","channel_name":"private-provider","upstream_url":"https://private.invalid","data":{"text":"usage_source private-worker"}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var original string
				engine := privacyRouter(func(c *gin.Context) {
					c.Header("X-Railway-Edge", "private-platform")
					c.Writer.Header()["x-lowercase-provider"] = []string{"private-lowercase"}
					c.Header("X-Hikari-Trace", "private-trace")
					c.Header("X-New-Api-Version", "private-version")
					c.Header("X-Unknown-Provider", "private-unknown")
					c.Header("Server", "private-server")
					c.Header("Via", "private-proxy")
					c.Header("Set-Cookie", "private-cookie")
					c.Header("Access-Control-Allow-Origin", "https://private.invalid")
					c.Header(common.RequestIdKey, "private-request")
					c.Header("Retry-After", "3")
					original = tc.body
					if stream {
						c.Header("Content-Type", "text/event-stream")
						_, err := c.Writer.WriteString(": keepalive\n\nevent: response\ndata: " + tc.body[:len(tc.body)/2])
						require.NoError(t, err)
						c.Writer.Flush()
						_, err = c.Writer.WriteString(tc.body[len(tc.body)/2:] + "\n\ndata: [DONE]\n\n")
						require.NoError(t, err)
						c.Writer.Flush()
					} else {
						c.Header("Content-Type", "application/json")
						c.Header("Content-Length", fmt.Sprint(len(tc.body)))
						_, err := c.Writer.WriteString(tc.body[:len(tc.body)/2])
						require.NoError(t, err)
						c.Writer.Flush()
						_, err = c.Writer.WriteString(tc.body[len(tc.body)/2:])
						require.NoError(t, err)
					}
				})
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))
				assert.Equal(t, http.StatusOK, recorder.Code)
				assert.Equal(t, tc.body, original)
				for _, field := range []string{`"usage_source"`, `"usage_semantic"`, `"billing_usage"`, `"cost"`, `"system_fingerprint"`, `"provider"`, `"channel_name"`, `"upstream_url"`} {
					assert.NotContains(t, recorder.Body.String(), field)
				}
				assert.Contains(t, recorder.Body.String(), "usage_source private-worker", "generated content must be untouched")
				assert.NotContains(t, recorder.Body.String(), "private-failure")
				for _, name := range []string{"X-Railway-Edge", "X-Hikari-Trace", "X-New-Api-Version", "X-Unknown-Provider", "Server", "Via", "Set-Cookie"} {
					assert.Empty(t, recorder.Header().Get(name))
				}
				assert.NotContains(t, recorder.Header(), "x-lowercase-provider")
				assert.Equal(t, "local-request", recorder.Header().Get(common.RequestIdKey))
				assert.Equal(t, "https://customer.example", recorder.Header().Get("Access-Control-Allow-Origin"))
				assert.Equal(t, "3", recorder.Header().Get("Retry-After"))
				if stream {
					assert.Contains(t, recorder.Body.String(), "event: response")
					assert.Contains(t, recorder.Body.String(), "data: [DONE]")
				} else {
					assert.Equal(t, fmt.Sprint(recorder.Body.Len()), recorder.Header().Get("Content-Length"))
				}
			})
		}
	}
}

func TestKimiCacheUsageThroughRelayPrivacy(t *testing.T) {
	service.InitHttpClient()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 10
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })

	for _, tc := range []struct {
		name      string
		model     string
		stream    bool
		malformed bool
	}{
		{name: "highspeed JSON", model: "kimi-k3.1-pro-highspeed"},
		{name: "pro SSE", model: "kimi-k3.1-pro", stream: true},
		{name: "invalid values JSON", model: "kimi-k3.1-pro-highspeed", malformed: true},
		{name: "invalid values SSE", model: "kimi-k3.1-pro", stream: true, malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cacheOptions = `{"mode":"implicit","ttl":"1h"}`
			const usage = `"usage":{"prompt_tokens":2300,"completion_tokens":1,"total_tokens":2301,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2048}}`

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				var request struct {
					CacheOptions common.RawMessage `json:"prompt_cache_options"`
				}
				if assert.NoError(t, common.DecodeJson(r.Body, &request)) {
					assert.JSONEq(t, cacheOptions, string(request.CacheOptions))
				}

				w.Header().Set("Server", "private-provider")
				w.Header().Set("Via", "private-proxy")
				w.Header().Set("X-Request-Id", "private-request")
				w.Header().Set("X-Client-Request-Id", "private-client-request")
				w.Header().Set("Set-Cookie", "private-cookie")
				w.Header().Set("Msh-Usage-Account", "private-account")
				w.Header().Set(common.KimiCacheWriteTokens5mHeader, "0")
				w.Header().Set(common.KimiCacheWriteTokens1hHeader, "2048")
				if tc.malformed {
					w.Header().Set(common.KimiCacheWriteTokens5mHeader, "https://private.invalid")
					w.Header().Add(common.KimiCacheWriteTokens1hHeader, "2049")
				}

				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"completion-id\",\"model\":%q,\"provider\":\"private-provider\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"completion-id\",\"model\":%q,\"choices\":[],%s}\n\ndata: [DONE]\n\n", tc.model, tc.model, usage)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"completion-id","model":%q,"provider":"private-provider","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],%s}`, tc.model, usage)
			}))
			t.Cleanup(upstream.Close)

			engine := privacyRouter(func(c *gin.Context) {
				defer common.CleanupBodyStorage(c)
				const mode = relayconstant.RelayModeChatCompletions
				req, err := helper.GetAndValidateTextRequest(c, mode)
				require.NoError(t, err)
				info := &relaycommon.RelayInfo{
					ChannelMeta: &relaycommon.ChannelMeta{
						ChannelType:       constant.ChannelTypeSub2API,
						ChannelBaseUrl:    upstream.URL,
						UpstreamModelName: req.Model,
					},
					OriginModelName:    req.Model,
					RequestURLPath:     "/v1/chat/completions",
					RelayFormat:        types.RelayFormatOpenAI,
					RelayMode:          mode,
					IsStream:           tc.stream,
					ShouldIncludeUsage: true,
				}
				common.SetContextKey(c, constant.ContextKeyOriginalModel, req.Model)
				adaptor := &sub2api.Adaptor{}
				adaptor.Init(info)
				converted, err := adaptor.ConvertOpenAIRequest(c, info, req)
				require.NoError(t, err)
				body, err := common.Marshal(converted)
				require.NoError(t, err)
				resp, err := adaptor.DoRequest(c, info, bytes.NewReader(body))
				require.NoError(t, err)
				_, relayErr := adaptor.DoResponse(c, resp.(*http.Response), info)
				require.Nil(t, relayErr)
			})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(fmt.Sprintf(
				`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"OK"}],"prompt_cache_options":%s}`, tc.model, tc.stream, cacheOptions,
			)))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			result := recorder.Result()
			defer result.Body.Close()
			require.Equal(t, http.StatusOK, result.StatusCode)
			if tc.malformed {
				assert.Empty(t, result.Header.Values(common.KimiCacheWriteTokens5mHeader))
				assert.Empty(t, result.Header.Values(common.KimiCacheWriteTokens1hHeader))
			} else {
				assert.Equal(t, []string{"0"}, result.Header.Values(common.KimiCacheWriteTokens5mHeader))
				assert.Equal(t, []string{"2048"}, result.Header.Values(common.KimiCacheWriteTokens1hHeader))
			}
			for _, header := range []string{"Server", "Via", "X-Request-Id", "X-Client-Request-Id", "Set-Cookie", "Msh-Usage-Account"} {
				assert.Empty(t, result.Header.Values(header), header)
			}
			assert.Equal(t, "local-request", result.Header.Get(common.RequestIdKey))
			assert.NotContains(t, recorder.Body.String(), "private-provider")
			assert.Contains(t, recorder.Body.String(), `"ephemeral_1h_input_tokens":2048`)
			if tc.stream {
				assert.Contains(t, recorder.Body.String(), "data: [DONE]")
			}
		})
	}
}

func TestKimiErrorsKeepUpstreamDiagnosticsPrivate(t *testing.T) {
	withErrorPassthrough(t)
	const failure = `{"error":{"type":"rate_limit_error","message":"private-provider https://private.invalid","code":"rate_limit_exceeded"}}`

	for _, tc := range []struct {
		model  string
		status int
		stream bool
	}{
		{model: "kimi-k3.1-pro-highspeed", status: http.StatusBadRequest},
		{model: "kimi-k3.1-pro", stream: true},
	} {
		t.Run(tc.model, func(t *testing.T) {
			engine := privacyRouter(func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
				common.SetContextKey(c, constant.ContextKeyChannelId, 1)
				if tc.stream {
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString("event: error\ndata: " + failure + "\n\n")
					require.NoError(t, err)
					c.Writer.Flush()
					return
				}
				c.Data(tc.status, "application/json", []byte(failure))
			})
			request := httptest.NewRequest(http.MethodPost, "/test", nil)
			request.Header.Set(common.RelayErrorPassthroughHeader, "true")
			request.Header.Set(common.RelayErrorPassthroughNoHeader, "false")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if tc.stream {
				assert.Equal(t, http.StatusOK, recorder.Code)
				assert.Contains(t, recorder.Body.String(), "event: error")
			} else {
				assert.Equal(t, tc.status, recorder.Code)
			}
			assert.Contains(t, recorder.Body.String(), "rate_limit_exceeded")
			assert.Contains(t, recorder.Body.String(), "local-request")
			assert.NotContains(t, recorder.Body.String(), "private-provider")
			assert.NotContains(t, recorder.Body.String(), "private.invalid")
		})
	}
}

func TestKimiChatRequestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const highspeed = "kimi-k3.1-pro-highspeed"
	for _, tc := range []struct {
		name       string
		model      string
		parameters string
		messages   string
		field      string
		valid      bool
	}{
		{name: "omitted", model: highspeed, valid: true},
		{name: "default thinking accepts 1", model: highspeed, parameters: `,"temperature":1,"top_p":0.95`, valid: true},
		{name: "default thinking rejects 0.7", model: highspeed, parameters: `,"temperature":0.7`, field: "temperature"},
		{name: "thinking enabled rejects 0.6", model: highspeed, parameters: `,"thinking":{"type":"enabled"},"temperature":0.6`, field: "temperature"},
		{name: "thinking disabled accepts 0.6", model: highspeed, parameters: `,"thinking":{"type":"disabled"},"temperature":0.6,"top_p":0.95`, valid: true},
		{name: "thinking disabled rejects 1", model: highspeed, parameters: `,"thinking":{"type":"disabled"},"temperature":1`, field: "temperature"},
		{name: "effort none selects non-thinking sampling", model: highspeed, parameters: `,"thinking":{"type":"enabled"},"reasoning_effort":"none","temperature":0.6`, valid: true},
		{name: "temperature zero in a streaming request", model: highspeed, parameters: `,"stream":true,"thinking":{"type":"disabled"},"temperature":0`, field: "temperature"},
		{name: "top_p 0.5", model: highspeed, parameters: `,"top_p":0.5`, field: "top_p"},
		{name: "thinking disabled rejects top_p 1", model: highspeed, parameters: `,"thinking":{"type":"disabled"},"top_p":1`, field: "top_p"},
		{name: "standard model keeps provider validation", model: "kimi-k3.1-pro", parameters: `,"temperature":0.7,"top_p":0.5`, valid: true},
		{name: "named tool with thinking", model: highspeed, parameters: `,"thinking":{"type":"enabled"},"tool_choice":{"type":"function","function":{"name":"lookup"}}`, field: "tool_choice"},
		{name: "named tool with effort", model: "kimi-k3.1-pro", parameters: `,"reasoning_effort":"high","tool_choice":{"type":"function","function":{"name":"lookup"}}`, field: "tool_choice"},
		{name: "named tool with thinking off", model: highspeed, parameters: `,"thinking":{"type":"disabled"},"tool_choice":{"type":"function","function":{"name":"lookup"}}`, valid: true},
		{name: "automatic tool with high effort preserved", model: highspeed, parameters: `,"thinking":{"type":"enabled","effort":"high"},"tool_choice":"auto"`, valid: true},
		{name: "explicit cache mode", model: "kimi-k3.1-pro", parameters: `,"prompt_cache_options":{"mode":"explicit"}`, field: "prompt_cache_options.mode"},
		{name: "implicit cache mode", model: highspeed, parameters: `,"prompt_cache_options":{"mode":"implicit","ttl":"1h"}`, valid: true},
		{name: "unsupported cache ttl", model: highspeed, parameters: `,"prompt_cache_options":{"ttl":"2h"}`, field: "prompt_cache_options.ttl"},
		{name: "developer role", model: highspeed, messages: `[{"role":"developer","content":"hi"}]`, field: "messages[0].role"},
		{name: "bare base64", model: highspeed, messages: `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"aGVsbG8="}}]}]`, field: "messages[0].content"},
		{name: "image data URL", model: "kimi-k3.1-pro", messages: `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]`, valid: true},
		{name: "other model developer role", model: "gpt-4o", messages: `[{"role":"developer","content":"hi"}]`, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/v1/chat/completions"
			messages := tc.messages
			if messages == "" {
				messages = `[{"role":"user","content":"hi"}]`
			}
			body := fmt.Sprintf(`{"model":%q,"messages":%s%s}`, tc.model, messages, tc.parameters)
			engine := gin.New()
			engine.POST(path, middleware.RouteTag("relay"), func(c *gin.Context) {
				defer common.CleanupBodyStorage(c)
				common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
				common.SetContextKey(c, constant.ContextKeyChannelId, 1)
				c.Set(common.RequestIdKey, "gateway-request")
				if !tc.valid {
					controller.Relay(c, types.RelayFormatOpenAI)
					return
				}
				parsed, err := helper.GetAndValidateTextRequest(c, relayconstant.RelayModeChatCompletions)
				require.NoError(t, err)
				forwarded, err := common.Marshal(parsed)
				require.NoError(t, err)
				assert.JSONEq(t, body, string(forwarded))
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(common.RelayErrorPassthroughHeader, "true")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if tc.valid {
				assert.Equal(t, http.StatusNoContent, recorder.Code)
				return
			}
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			var response struct {
				Error *types.OpenAIError `json:"error"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.NotNil(t, response.Error)
			assert.Equal(t, "invalid_request_error", response.Error.Type)
			assert.Equal(t, "invalid_request", response.Error.Code)
			assert.Equal(t, tc.field, response.Error.Param)
			assert.NotEqual(t, common.PublicRelayFailureMessage("gateway-request"), response.Error.Message)
			assert.NotContains(t, response.Error.Message, "aGVsbG8=")
		})
	}
}

func TestKimiMessagesCacheTTLAndUsage(t *testing.T) {
	service.InitHttpClient()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 10
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	const model = "kimi-k3.1-pro"
	for _, tc := range []struct {
		name         string
		stream       bool
		cacheControl string
		wantTTL      string
		count        string
		wantSplit    bool
	}{
		{name: "1h JSON", wantTTL: "1h", count: "2048", wantSplit: true},
		{name: "1h SSE", stream: true, wantTTL: "1h", count: "2048", wantSplit: true},
		{name: "conflicting upstream counts", wantTTL: "1h", count: "2047"},
		{name: "explicit cache control wins", cacheControl: `,"cache_control":{"type":"ephemeral","ttl":"5m"}`, wantTTL: "5m", count: "2048", wantSplit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const usage = `"usage":{"input_tokens":12,"output_tokens":1,"cache_creation_input_tokens":2048,"cache_read_input_tokens":0}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/messages", r.URL.Path)
				var request struct {
					CacheControl struct {
						Type string `json:"type"`
						TTL  string `json:"ttl"`
					} `json:"cache_control"`
					TTL *string `json:"ttl"`
				}
				if assert.NoError(t, common.DecodeJson(r.Body, &request)) {
					assert.Equal(t, tc.wantTTL, request.CacheControl.TTL)
					assert.Equal(t, "ephemeral", request.CacheControl.Type)
					assert.Nil(t, request.TTL)
				}
				w.Header().Set("X-Provider", "private-provider")
				w.Header().Set(common.KimiCacheWriteTokens5mHeader, "0")
				w.Header().Set(common.KimiCacheWriteTokens1hHeader, tc.count)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":%q,%s}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},%s}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", model, usage, usage)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"type":"message","model":%q,"content":[{"type":"text","text":"OK"}],%s}`, model, usage)
			}))
			t.Cleanup(upstream.Close)

			engine := privacyRouter(func(c *gin.Context) {
				defer common.CleanupBodyStorage(c)
				req, err := helper.GetAndValidateClaudeRequest(c)
				require.NoError(t, err)
				info := &relaycommon.RelayInfo{
					ChannelMeta: &relaycommon.ChannelMeta{
						ChannelType:       constant.ChannelTypeSub2API,
						ChannelBaseUrl:    upstream.URL,
						UpstreamModelName: req.Model,
					},
					OriginModelName: req.Model,
					RequestURLPath:  "/v1/messages",
					RelayFormat:     types.RelayFormatClaude,
					IsStream:        tc.stream,
				}
				adaptor := &sub2api.Adaptor{}
				adaptor.Init(info)
				converted, err := adaptor.ConvertClaudeRequest(c, info, req)
				require.NoError(t, err)
				body, err := common.Marshal(converted)
				require.NoError(t, err)
				response, err := adaptor.DoRequest(c, info, bytes.NewReader(body))
				require.NoError(t, err)
				actual, relayErr := adaptor.DoResponse(c, response.(*http.Response), info)
				require.Nil(t, relayErr)
				reported := actual.(*dto.Usage)
				if tc.wantSplit {
					assert.Equal(t, 2048, reported.ClaudeCacheCreation1hTokens)
				} else {
					assert.Zero(t, reported.ClaudeCacheCreation1hTokens)
				}
				assert.Equal(t, 2048, reported.PromptTokensDetails.CachedCreationTokens)
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(fmt.Sprintf(
				`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":32,"ttl":"1h","stream":%t%s}`, model, tc.stream, tc.cacheControl,
			)))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)
			response := recorder.Result()
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "0", response.Header.Get(common.KimiCacheWriteTokens5mHeader))
			assert.Equal(t, tc.count, response.Header.Get(common.KimiCacheWriteTokens1hHeader))
			assert.Empty(t, response.Header.Get("X-Provider"))
			if tc.wantSplit {
				assert.Contains(t, recorder.Body.String(), `"ephemeral_1h_input_tokens":2048`)
			} else {
				assert.NotContains(t, recorder.Body.String(), `"ephemeral_1h_input_tokens"`)
			}
			if tc.stream {
				assert.Contains(t, recorder.Body.String(), "event: message_stop")
			}
		})
	}
}

func TestRelayPrivacyKeepsImageAccountingAndBinaryMedia(t *testing.T) {
	const body = `{"data":[{"b64_json":"original-image"}],"usage":{"input_tokens":19,"output_tokens":1056,"total_tokens":1075,"usage_source":"private-worker","usage_semantic":"estimated"}}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			engine := privacyRouter(func(c *gin.Context) {
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}, common.RequestIdKey: []string{"upstream-request"}}, Body: io.NopCloser(strings.NewReader(body))}
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: stream}
				if stream {
					usage, err := openai.OpenaiImageStreamHandler(c, info, response)
					require.Nil(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, "private-worker", usage.UsageSource)
					assert.Equal(t, 1056, usage.CompletionTokens)
				} else {
					usage, err := openai.OpenaiImageHandler(c, info, response)
					require.Nil(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, "private-worker", usage.UsageSource)
					assert.Equal(t, 1056, usage.CompletionTokens)
					assert.Equal(t, "upstream-request", c.GetString(common.UpstreamRequestIdKey))
				}
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
			assert.NotContains(t, recorder.Body.String(), "private-worker")
			assert.NotContains(t, recorder.Body.String(), "usage_semantic")
			assert.Contains(t, recorder.Body.String(), "original-image")
			assert.Contains(t, recorder.Body.String(), `"output_tokens":1056`)
		})
	}
	engine := privacyRouter(func(c *gin.Context) { c.Data(200, "audio/mpeg", []byte("audio\x00\xffprivate-worker")) })
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
	assert.Equal(t, []byte("audio\x00\xffprivate-worker"), recorder.Body.Bytes())
	for _, name := range []string{"Content-Type", "Retry-After", "X-Reasoning-Included", "X-Codex-Turn-State"} {
		assert.True(t, service.ShouldCopyUpstreamHeader(nil, name, []string{"value"}))
	}
	assert.False(t, service.ShouldCopyUpstreamHeader(nil, "X-New-Api-Version", []string{"value"}))
	assert.False(t, service.ShouldCopyUpstreamHeader(nil, "Content-Type", nil))
}

func TestRelayPrivacyHidesErrorDiagnostics(t *testing.T) {
	withErrorRedaction(t)
	for _, tc := range []struct {
		name              string
		status            int
		contentType, body string
		wantJSON          string
	}{
		{"json", 502, "application/json", `{"error":{"message":"Post https://private.invalid: secret","code":"private-provider","metadata":{"key":"secret"}},"debug":"private-worker"}`, ""},
		{"html", 429, "text/html", "<html>private-provider secret</html>", ""},
		{"sse", 200, "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"response_not_found\",\"message\":\"private-provider secret\"}}\n\n", ""},
		{"malformed", 200, "application/json", "{broken private-provider secret", ""},
		{"task-error", 400, "application/json", `{"code":"fail_to_fetch_task","message":"private-provider secret","data":{"debug":"private-worker secret"}}`, `{"error":{"type":"api_error","code":"upstream_error","message":"The model request failed. Contact support with the request ID. (request id: local-request)"}}`},
		{"standard-error", 400, "application/json", `{"error":{"type":"invalid_request_error","code":"invalid_input","message":"private-provider secret"},"debug":"private-worker"}`, `{"error":{"type":"invalid_request_error","code":"invalid_input","message":"The model request failed. Contact support with the request ID. (request id: local-request)"}}`},
		{"empty-error", 400, "application/json", `{"error":{},"debug":"private-worker secret"}`, `{"error":{"type":"api_error","code":"upstream_error","message":"The model request failed. Contact support with the request ID. (request id: local-request)"}}`},
		{"null-error", 400, "application/json", `{"error":null,"message":"private-worker secret"}`, `{"error":{"type":"api_error","code":"upstream_error","message":"The model request failed. Contact support with the request ID. (request id: local-request)"}}`},
		{"empty-string-error", 400, "application/json", `{"error":"","message":"private-worker secret"}`, `{"error":{"type":"api_error","code":"upstream_error","message":"The model request failed. Contact support with the request ID. (request id: local-request)"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := privacyRouter(func(c *gin.Context) {
				c.Set("channel_id", 38)
				c.Data(tc.status, tc.contentType, []byte(tc.body))
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
			assert.NotContains(t, recorder.Body.String(), "private-")
			assert.NotContains(t, recorder.Body.String(), "secret")
			assert.Contains(t, recorder.Body.String(), "local-request")
			if tc.wantJSON != "" {
				assert.JSONEq(t, tc.wantJSON, recorder.Body.String())
			}
			if tc.contentType == "text/event-stream" {
				assert.Contains(t, recorder.Body.String(), "response_not_found")
			} else if tc.status >= 400 {
				assert.Equal(t, tc.status, recorder.Code)
			} else {
				assert.Equal(t, 502, recorder.Code)
			}
		})
	}
	// WebSocket callers use the same sanitizer on a client-only copy.
	original := []byte(`{"type":"response.done","response":{"id":"resume-id","usage":{"total_tokens":7,"usage_source":"private-worker"}}}`)
	clean := common.PublicRelayJSON(original, "request", false)
	assert.NotContains(t, string(clean), "private-worker")
	assert.Contains(t, string(clean), "resume-id")
	assert.Contains(t, string(original), "private-worker")
	multiline := []byte("event: message\r\ndata: {\"usage\":\r\ndata: {\"total_tokens\":7,\"usage_source\":\"private-worker\"}}\r\n\r\n")
	result := common.PublicRelaySSE(multiline, "request", true)
	assert.NotContains(t, string(result), "private-worker")
	assert.Contains(t, string(result), "event: message")
	assert.Contains(t, string(result), `"total_tokens":7`)
}

func TestRelayPrivacyProxiesAudioWithoutDisclosingTheDownloadHost(t *testing.T) {
	fetch := system_setting.GetFetchSetting()
	previous := *fetch
	t.Cleanup(func() { *fetch = previous; service.InitHttpClient() })
	fetch.AllowPrivateIp = true
	fetch.AllowedPorts = []string{"1-65535"}
	service.InitHttpClient()
	audio := []byte("audio\x00\xff")
	downloads := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/audio", http.StatusFound)
			return
		}
		w.Header().Set("X-Private-Upstream", "hidden")
		_, _ = w.Write(audio)
	}))
	defer upstream.Close()
	engine := privacyRouter(func(c *gin.Context) {
		body, err := common.Marshal(map[string]any{"data": map[string]any{"audio": upstream.URL + "/redirect"}, "extra_info": map[string]any{"usage_characters": 5}})
		require.NoError(t, err)
		response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, RelayMode: relayconstant.RelayModeAudioSpeech}
		_, apiErr := (&minimax.Adaptor{}).DoResponse(c, response, info)
		if !fetch.AllowPrivateIp {
			require.NotNil(t, apiErr)
			c.JSON(502, gin.H{"error": apiErr.ToOpenAIError()})
			return
		}
		require.Nil(t, apiErr)
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
	assert.Equal(t, 200, recorder.Code)
	assert.Equal(t, audio, recorder.Body.Bytes())
	assert.Equal(t, 2, downloads)
	assert.Empty(t, recorder.Header().Get("Location"))
	assert.Empty(t, recorder.Header().Get("X-Private-Upstream"))
	fetch.AllowPrivateIp = false
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
	assert.Equal(t, 502, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), upstream.URL)
	assert.Equal(t, 2, downloads, "private addresses must be blocked before fetching")
}

func TestRelayPrivacyLeavesLocalValidationAndPricingIntact(t *testing.T) {
	engine := privacyRouter(func(c *gin.Context) {
		c.JSON(400, gin.H{"error": gin.H{"type": "new_api_error", "code": "invalid_request", "message": "n must be between 1 and 128"}})
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
	assert.Contains(t, recorder.Body.String(), "n must be between 1 and 128")
	engine = gin.New()
	engine.GET("/api/pricing", middleware.RouteTag("api"), func(c *gin.Context) {
		c.JSON(200, gin.H{"billing_expr": "customer-price-expression", "group_ratio": 1})
	})
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/pricing", nil))
	assert.JSONEq(t, `{"billing_expr":"customer-price-expression","group_ratio":1}`, recorder.Body.String())
	for _, frame := range []string{"event: error\ndata: private-worker secret\n\n", "event: error\ndata: {\"message\":\"private-worker secret\"}\n\n"} {
		clean := common.PublicRelaySSE([]byte(frame), "local-request", true)
		assert.NotContains(t, string(clean), "private-worker")
		assert.NotContains(t, string(clean), "secret")
		assert.Contains(t, string(clean), "local-request")
	}
	engine = privacyRouter(func(c *gin.Context) {
		c.Header("Content-Type", "audio/mpeg")
		_, err := c.Writer.Write([]byte("partial-audio"))
		require.NoError(t, err)
		c.Writer.Flush()
		c.JSON(502, gin.H{"error": gin.H{"message": "https://private.invalid"}})
	})
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest("POST", "/test", nil))
	assert.Equal(t, "partial-audio", recorder.Body.String())
}
