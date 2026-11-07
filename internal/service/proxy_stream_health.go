package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
)

// ErrStreamTruncated reports that the upstream closed the SSE body without any
// completion marker: no [DONE], no finish_reason, no message_stop.
//
// This is the failure mode behind "Upstream request failed" in the client: the
// relay station cuts a long single stream (a long reasoning block immediately
// followed by a multi-KB tool-call argument) in the middle. Before this check
// the gateway logged such a request as a success with 0/0 tokens, so the panel
// stayed green while the client kept dying.
var ErrStreamTruncated = errors.New("upstream stream ended without a completion marker ([DONE]/finish_reason)")

// StreamTruncatedError carries whether any byte already reached the client.
// A truncation before the first byte may still be retried on another endpoint;
// after that, a retry would duplicate the partial answer.
type StreamTruncatedError struct {
	UpstreamErr string
	Forwarded   bool
}

func (e *StreamTruncatedError) Error() string {
	if e.UpstreamErr != "" {
		return fmt.Sprintf("%v (upstream error: %s)", ErrStreamTruncated, e.UpstreamErr)
	}
	return ErrStreamTruncated.Error()
}

// Unwrap lets errors.Is(err, ErrStreamTruncated) work.
func (e *StreamTruncatedError) Unwrap() error { return ErrStreamTruncated }

// streamOutcome summarises how an upstream SSE body ended.
type streamOutcome struct {
	// Terminated is true when the upstream signalled a normal end of stream.
	Terminated bool
	// UpstreamErr holds the error message an upstream pushed inside the stream
	// (for example {"error":{"message":"Upstream request failed"}}).
	UpstreamErr string
	// Claude is true when the body used the Anthropic event vocabulary.
	Claude bool
}

// Failed reports whether the body is unusable: either it ended without a
// completion marker, or the upstream pushed an error object into it.
func (o streamOutcome) Failed() bool { return !o.Terminated || o.UpstreamErr != "" }

// inspectUpstreamStream scans a raw SSE body for the end-of-stream markers of
// the three formats this proxy speaks (OpenAI, Claude, Gemini) and for an
// upstream error object.
func inspectUpstreamStream(raw string) streamOutcome {
	var out streamOutcome
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ":") || strings.HasPrefix(trimmed, "event:") {
			continue
		}
		payload := trimmed
		if strings.HasPrefix(payload, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			out.Terminated = true
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}

		if e, ok := chunk["error"].(map[string]any); ok {
			if msg, _ := e["message"].(string); msg != "" {
				out.UpstreamErr = msg
			} else {
				out.UpstreamErr = payload
			}
			continue
		}

		if kind, ok := chunk["type"].(string); ok {
			switch kind {
			case "message_start", "message_delta", "content_block_start", "content_block_delta":
				out.Claude = true
			case "message_stop":
				out.Terminated = true
			case "error":
				out.UpstreamErr = payload
			}
			if kind == "message_delta" {
				if delta, ok := chunk["delta"].(map[string]any); ok {
					if stop, _ := delta["stop_reason"].(string); stop != "" {
						out.Terminated = true
					}
				}
			}
		}

		if choices, ok := chunk["choices"].([]any); ok {
			for _, c := range choices {
				choice, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
					out.Terminated = true
				}
			}
		}

		if candidates, ok := chunk["candidates"].([]any); ok {
			for _, c := range candidates {
				candidate, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if reason, ok := candidate["finishReason"].(string); ok && reason != "" {
					out.Terminated = true
				}
			}
		}

		// A usage chunk carrying completion tokens is also proof that the
		// upstream finished generating, even when it omits finish_reason.
		if usage, ok := chunk["usage"].(map[string]any); ok {
			if ct, ok := usage["completion_tokens"].(float64); ok && ct > 0 {
				out.Terminated = true
			}
		}
	}
	return out
}

// truncationError builds the error returned for a stream that ended early.
func truncationError(out streamOutcome, forwarded bool) *StreamTruncatedError {
	return &StreamTruncatedError{UpstreamErr: out.UpstreamErr, Forwarded: forwarded}
}

// coolDownStreamEndpoint marks the endpoint that served a broken stream as
// failed so the next request starts from another candidate.
func (s *ProxyService) coolDownStreamEndpoint(res *routeResolution, routeID int64, failure error) {
	if res == nil || res.Group == nil || routeID == 0 {
		return
	}
	s.failover.MarkFailed(res.Group.ID, routeID)
	log.Warnf("[Stream] endpoint %d in group %q cooled down after a broken stream: %v",
		routeID, res.Group.Name, failure)
}

// StreamReportedError marks an error whose SSE error frame has already been
// written to the client. The HTTP layer must not write a second frame: a
// duplicate error object after [DONE] confuses strict SSE clients.
type StreamReportedError struct{ Err error }

func (e *StreamReportedError) Error() string { return e.Err.Error() }

// Unwrap keeps errors.Is/errors.As working on the wrapped cause.
func (e *StreamReportedError) Unwrap() error { return e.Err }

// IsStreamReported reports whether the error frame already reached the client.
func IsStreamReported(err error) bool {
	var reported *StreamReportedError
	return errors.As(err, &reported)
}

// writeStreamTruncatedError tells an SSE client that the answer was cut short.
// The dedicated code lets the client distinguish "the relay dropped the stream"
// from a sanitizer verdict, instead of showing a bare "Upstream request failed".
func writeStreamTruncatedError(writer io.Writer, flusher http.Flusher, err error) {
	message := ErrStreamTruncated.Error()
	var upstream string
	var truncated *StreamTruncatedError
	if errors.As(err, &truncated) && truncated.UpstreamErr != "" {
		upstream = truncated.UpstreamErr
	}
	body := map[string]any{
		"message": message,
		"type":    "upstream_error",
		"code":    "STREAM_TRUNCATED",
	}
	if upstream != "" {
		body["upstream_message"] = upstream
	}
	payload, _ := json.Marshal(map[string]any{"error": body})
	io.WriteString(writer, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
