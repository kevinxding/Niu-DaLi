package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// A bound on one JSON response or unfinished SSE frame, never the entire stream.
const maxPrivateRelayBuffer = 256 << 20

// relayPrivacyWriter is installed for every relay route, including task routes.
// This covers adaptors which use c.JSON/c.Data or write directly, not just the
// shared response helpers. Binary media remains streaming and byte-for-byte intact.
type relayPrivacyWriter struct {
	gin.ResponseWriter
	ctx            *gin.Context
	localHeaders   http.Header
	pending        bytes.Buffer
	mode           string
	failed         bool
	lateMediaError bool
	jsonWritten    bool

	passthrough    bool
	passthroughSet bool
}

// errorPassthrough reports whether upstream errors reach the client unchanged.
// It is resolved once per response, so flipping the option mid-response cannot
// produce a half-redacted body.
func (w *relayPrivacyWriter) errorPassthrough() bool {
	if !w.passthroughSet {
		w.passthrough = common.RelayErrorPassthroughFor(w.ctx.GetHeader)
		w.passthroughSet = true
	}
	return w.passthrough
}

// redactErrors is the inverse of the pass-through switch, named for the
// sanitizer it feeds.
func (w *relayPrivacyWriter) redactErrors() bool { return !w.errorPassthrough() }

func (w *relayPrivacyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *relayPrivacyWriter) prepare() {
	if w.mode != "" {
		return
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	switch {
	case w.Status() >= 400 && !strings.Contains(contentType, "json"):
		w.mode = "error"
	case strings.Contains(contentType, "text/event-stream"):
		w.mode = "sse"
	case strings.Contains(contentType, "json"):
		w.mode = "json"
	default:
		w.mode = "binary"
	}
	for name := range w.Header() {
		if common.RelayProtocolHeader(name) || name == "Content-Length" || name == "Connection" || name == "Transfer-Encoding" {
			continue
		}
		if values, ok := w.localHeaders[name]; ok {
			w.Header()[name] = values
		} else {
			delete(w.Header(), name)
		}
	}
	// The local request ID is the only infrastructure trace exposed to clients.
	if id := w.ctx.GetString(common.RequestIdKey); id != "" {
		w.Header().Set(common.RequestIdKey, id)
	}
	if w.mode != "binary" {
		w.Header().Del("Content-Length")
	}
}

func (w *relayPrivacyWriter) WriteHeader(status int) {
	if w.mode == "binary" && w.ResponseWriter.Written() && status >= 400 {
		w.lateMediaError = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *relayPrivacyWriter) WriteHeaderNow() {
	w.prepare()
	if w.mode == "binary" || w.mode == "sse" {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *relayPrivacyWriter) Write(data []byte) (int, error) {
	w.prepare()
	if w.failed {
		return 0, errors.New("relay response privacy buffer exceeded")
	}
	if w.mode == "binary" {
		// A late error must not append JSON diagnostics to an audio/video stream
		// whose headers and payload have already been sent.
		if w.lateMediaError || (w.ResponseWriter.Written() && strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "json")) {
			return 0, errors.New("cannot append JSON to an active media response")
		}
		return w.ResponseWriter.Write(data)
	}
	// Most adaptors already hold a complete JSON document. Filter and write
	// that document directly, avoiding another multi-megabyte image buffer.
	if w.mode == "json" && w.pending.Len() == 0 && !w.jsonWritten && gjson.ValidBytes(data) {
		err := w.writeJSON(data)
		return len(data), err
	}
	if w.jsonWritten {
		return 0, errors.New("JSON response already completed")
	}
	if w.pending.Len()+len(data) > maxPrivateRelayBuffer {
		w.pending.Reset()
		w.failed = true
		return 0, errors.New("relay response privacy buffer exceeded")
	}
	_, _ = w.pending.Write(data)
	if w.mode == "sse" {
		for {
			buffer := w.pending.Bytes()
			end, delimiter := bytes.Index(buffer, []byte("\n\n")), 2
			if crlf := bytes.Index(buffer, []byte("\r\n\r\n")); crlf >= 0 && (end < 0 || crlf < end) {
				end, delimiter = crlf, 4
			}
			if end < 0 {
				break
			}
			frame := w.pending.Next(end + delimiter)
			if _, err := w.ResponseWriter.Write(common.PublicRelaySSE(frame, w.ctx.GetString(common.RequestIdKey), w.redactErrors())); err != nil {
				return 0, err
			}
		}
	}
	return len(data), nil
}

func (w *relayPrivacyWriter) WriteString(data string) (int, error) { return w.Write([]byte(data)) }
func (w *relayPrivacyWriter) Written() bool                        { return w.pending.Len() > 0 || w.ResponseWriter.Written() }

func (w *relayPrivacyWriter) Flush() {
	w.prepare()
	if w.mode == "json" || w.mode == "error" {
		return
	}
	w.ResponseWriter.Flush()
}

func (w *relayPrivacyWriter) finish() {
	w.prepare()
	if w.mode == "binary" {
		return
	}
	if w.mode == "sse" {
		// Upstreams sometimes omit the last blank line. Still filter the tail before
		// flushing; never flush an incomplete frame in the middle of a stream.
		if w.pending.Len() > 0 && !w.failed {
			_, _ = w.ResponseWriter.Write(common.PublicRelaySSE(w.pending.Bytes(), w.ctx.GetString(common.RequestIdKey), w.redactErrors()))
		}
		return
	}
	if w.jsonWritten {
		return
	}
	if err := w.writeJSON(w.pending.Bytes()); err != nil {
		common.SysError("failed to write public relay response: " + err.Error())
	}
}

func (w *relayPrivacyWriter) writeJSON(body []byte) error {
	w.jsonWritten = true
	passthrough := w.errorPassthrough()

	switch {
	case w.failed:
		// The privacy buffer overflowed, so the upstream body is gone. Only a
		// template is possible. This is a gateway fault rather than an upstream
		// rejection, so it stays opaque even under pass-through.
		body = w.failureTemplate()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Content-Encoding")
		if w.Status() < 400 {
			w.ResponseWriter.WriteHeader(http.StatusBadGateway)
		}
	case (w.mode == "error" || (len(body) > 0 && !gjson.ValidBytes(body))) && !passthrough:
		// A non-JSON or malformed upstream error under the privacy policy.
		body = w.failureTemplate()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Content-Encoding")
		if w.Status() < 400 {
			w.ResponseWriter.WriteHeader(http.StatusBadGateway)
		}
	case passthrough:
		// Hand the upstream document back with gateway provenance removed. The
		// upstream status and error text are already on the wire untouched.
		body = common.PassthroughRelayJSON(body)
	default:
		redactErrors := w.Status() < 400 || w.Status() >= 500 || w.ctx.GetInt("channel_id") > 0
		if w.Status() >= 400 && redactErrors {
			// Do not forward arbitrary sibling diagnostics from upstream errors.
			upstreamError := gjson.GetBytes(body, "error")
			originalError := upstreamError.Raw
			if !upstreamError.Exists() || upstreamError.Type == gjson.Null || originalError == `""` || originalError == "{}" {
				publicError, err := common.Marshal(gin.H{"type": "api_error", "code": "upstream_error", "message": common.PublicRelayFailureMessage(w.ctx.GetString(common.RequestIdKey))})
				if err != nil {
					return err
				}
				originalError = string(publicError)
			}
			if gjson.GetBytes(body, "type").String() == "error" {
				body = []byte(`{"type":"error","error":` + originalError + `}`)
			} else {
				body = []byte(`{"error":` + originalError + `}`)
			}
		}
		body = common.PublicRelayJSON(body, w.ctx.GetString(common.RequestIdKey), redactErrors)
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, err := w.ResponseWriter.Write(body)
	return err
}

// failureTemplate is the operator-visible placeholder used whenever the real
// upstream document cannot or must not be returned.
func (w *relayPrivacyWriter) failureTemplate() []byte {
	body, _ := common.Marshal(gin.H{"error": gin.H{"type": "api_error", "code": "upstream_error", "message": common.MessageWithRequestId("The model request failed. Contact support with the request ID.", w.ctx.GetString(common.RequestIdKey))}})
	return body
}
