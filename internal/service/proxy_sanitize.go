package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"openai-router-go/internal/sanitize"

	log "github.com/sirupsen/logrus"
)

// errThinkAsContent reports that the upstream answered with its reasoning in
// the assistant content field (and nothing else). Such a response must never be
// presented to the client as a finished answer: the agent loop would treat it
// as the model's final message and silently end the turn.
var errThinkAsContent = errors.New("upstream returned its reasoning as assistant content (think-as-content)")

// errEmptyUpstream reports that the response contained no usable assistant
// output after sanitizing.  In the original incident the upstream emitted only
// residual DSML closing tags; calling that "think-as-content" was misleading
// and also hid the fact that the endpoint was unhealthy.
var errEmptyUpstream = errors.New("upstream returned no usable content after sanitizing")

// errSanitizePartial reports that the response was rejected after some content
// had already been forwarded.  Retrying another endpoint would duplicate the
// partial answer, so the caller must fail the request instead.
var errSanitizePartial = errors.New("upstream response was rejected after partial content was sent")

// errTruncatedThinking reports that the upstream spent the whole completion
// budget on reasoning (or had its stream cut mid-thinking) and never started
// the answer. Older builds reported this as think-as-content, which sent every
// investigation after the wrong defect: the body really does contain reasoning
// only, but the cause is a truncated/unhealthy endpoint, not a relay writing
// the chain of thought into content. It is retryable on another endpoint.
var errTruncatedThinking = errors.New("upstream streamed only reasoning and never produced an answer (truncated thinking)")

// errDsmlUnparsed reports that the body held a DSML tool-call block that could
// not be reconstructed and no other usable content. It is a "textual tool call
// we could not recover" verdict, not a "reasoning leaked into content" one.
var errDsmlUnparsed = errors.New("upstream returned an unparsable DSML tool-call block and no usable content")

// sanitizeThinkingEnabled reports whether the response sanitizer is switched on.
// Missing config values default to false so tests and library users see the
// historical behaviour unless they opt in; LoadConfig sets it to true.
func (s *ProxyService) sanitizeThinkingEnabled() bool {
	return s != nil && s.config != nil && s.config.SanitizeThinking
}

// sanitizeDuplicateRestoreEnabled reports whether a poisoned response may be
// handed over as a last resort instead of failing the client's turn outright.
func (s *ProxyService) sanitizeDuplicateRestoreEnabled() bool {
	return s != nil && s.config != nil && s.config.SanitizeDuplicateRestore
}

// sanitizeRequestHistory removes reasoning replay and leaked chain-of-thought
// from the assistant messages before the request is sent upstream. This is the
// other half of the fix: sanitizing only the response leaves the model staring
// at its own malformed output on every turn, so it keeps copying the tags.
func (s *ProxyService) sanitizeRequestHistory(reqData map[string]interface{}) {
	if !s.sanitizeThinkingEnabled() || reqData == nil {
		return
	}
	changed, notes := sanitize.CleanAssistantHistory(reqData)
	if changed == 0 {
		return
	}
	log.Warnf("[Sanitize] cleaned request history: %d assistant message(s), notes=%s",
		changed, strings.Join(notes, ","))
}

// sanitizeNonStreamBody repairs a completed (non-streaming) OpenAI response.
// It returns an error describing the concrete defect when the body was nothing
// but a copy of the reasoning, so the caller can retry instead of forwarding it.
func (s *ProxyService) sanitizeNonStreamBody(body []byte) ([]byte, error) {
	out, notes, poison := sanitize.ApplyToBody(body)
	if len(notes) > 0 {
		log.Warnf("[Sanitize] repaired upstream response: %s", strings.Join(notes, ","))
	}
	if poison {
		failure := classifySanitizeFailure(notes, "", "", streamOutcome{Terminated: true})
		if failure == nil {
			failure = errThinkAsContent
		}
		log.Warnf("[Sanitize] rejected response: %v (notes=%s)", failure, strings.Join(notes, ","))
		return nil, failure
	}
	return out, nil
}

// classifySanitizeFailure turns the sanitizer notes plus what already reached
// the client into the one verdict that describes what actually happened.
//
// Ordering matters. A stream that died without a completion marker is a
// truncation whatever residue the guard saw, and a rejection that only ever
// forwarded reasoning is an exhausted thinking budget — neither of them is
// "reasoning written into content", which is what the old code always claimed.
func classifySanitizeFailure(notes []string, emittedContent, emittedReasoning string, outcome streamOutcome) error {
	switch {
	case emittedContent != "":
		return errSanitizePartial
	case hasNote(notes, sanitize.NoteDuplicateRestored):
		return nil
	case outcome.Failed():
		return truncationError(outcome, false)
	case hasNote(notes, sanitize.NoteDuplicate):
		return errThinkAsContent
	case hasNote(notes, sanitize.NoteDsmlUnparsed):
		return errDsmlUnparsed
	case hasNote(notes, sanitize.NoteOrphanDsml):
		// 只有 DSML 分隔符残渣的"推理"不是思考，谈不上思考烧光预算。
		return errEmptyUpstream
	case emittedReasoning != "":
		return errTruncatedThinking
	default:
		return errEmptyUpstream
	}
}

// streamDirectSanitized is streamDirect with the SSE stream rewritten through
// the sanitizer. dropReasoning suppresses reasoning frames, which is what a
// retry wants: the client already received them from the first attempt.
//
// allowRestore is the last-chance switch: when no other endpoint is left and
// nothing but reasoning has been forwarded, a response whose content was
// withheld as a duplicate of the reasoning is handed over anyway (see
// ProxyService.sanitizeDuplicateRestoreEnabled). Failing loudly is still the
// default — the restore only happens once failover is exhausted.
//
// The returned error names the concrete defect (see classifySanitizeFailure).
func (s *ProxyService) streamDirectSanitized(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64, dropReasoning, allowRestore bool) error {
	var raw bytes.Buffer
	src := io.TeeReader(reader, &raw)

	rewriter := sanitize.NewSSERewriter(src)
	rewriter.SetDropReasoning(dropReasoning)
	// The caller decides how a rejected stream is reported, so the rewriter
	// stays silent on poison; it still refuses to emit a fake finish.
	rewriter.SetSilent(true)
	// A body that ends without [DONE]/finish_reason is a truncation, not an
	// answer: never let the rewriter close it with a synthetic finish frame.
	rewriter.SetRequireCompletion(true)

	buf := make([]byte, 4096)
	var bytesWritten int64
	for {
		n, err := rewriter.Read(buf)
		if n > 0 {
			bytesWritten += int64(n)
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				log.Errorf("[Sanitize] Failed to write to client: %v", writeErr)
				s.routeService.LogRequest(model, routeID, 0, 0, 0, false, writeErr.Error())
				return writeErr
			}
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				rawBody := raw.String()
				promptTokens, completionTokens := s.extractTokensFromStreamResponse(rawBody)
				totalTokens := promptTokens + completionTokens
				outcome := inspectUpstreamStream(rawBody)
				if rewriter.Poisoned() {
					notes := rewriter.Notes()
					// Last resort: failover is exhausted, the client has no
					// answer bytes, and all we hold is the duplicate text.
					if allowRestore && s.sanitizeDuplicateRestoreEnabled() &&
						rewriter.EmittedContent() == "" && hasNote(notes, sanitize.NoteDuplicate) {
						if events := rewriter.RestoreWithheldContent(); len(events) > 0 {
							writeRestoredEvents(writer, flusher, rewriter.Meta(), events)
							log.Warnf("[Sanitize] no endpoint produced an answer; restored duplicated content as a last resort (notes=%s)",
								strings.Join(rewriter.Notes(), ","))
							s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, true, "")
							return nil
						}
					}
					failure := classifySanitizeFailure(notes, rewriter.EmittedContent(), rewriter.EmittedReasoning(), outcome)
					if failure == nil {
						failure = errThinkAsContent
					}
					s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, false,
						sanitizeLogReason(failure, notes))
					log.Warnf("[Sanitize] rejected stream (%d bytes forwarded): %v (notes=%s, finish_ok=%v)",
						bytesWritten, failure, strings.Join(notes, ","), !outcome.Failed())
					return failure
				}
				// 上游没有给出任何结束标记就断流：判定为该端点失败，
				// 由调用方冷却端点，客户端收到明确的截断错误而不是"成功但答案不完整"。
				if msg := rewriter.UpstreamError(); msg != "" && outcome.UpstreamErr == "" {
					outcome.UpstreamErr = msg
				}
				if outcome.Failed() {
					// Anything already on the wire (content, tool deltas) makes a
					// retry unsafe. A stream that carried *only* reasoning is
					// poisoned above and is handled by the failover branch.
					failure := truncationError(outcome, bytesWritten > 0)
					s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, false, failure.Error())
					log.Errorf("[Sanitize] %v (%d bytes forwarded)", failure, bytesWritten)
					return failure
				}
				log.Debugf("[Sanitize] stream completed, %d bytes forwarded", bytesWritten)
				s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, true, "")
				return nil
			}
			log.Errorf("[Sanitize] stream error: %v", err)
			s.routeService.LogRequest(model, routeID, 0, 0, 0, false, err.Error())
			return err
		}
	}
}

// sanitizeLogReason keeps the request log self-explanatory: the verdict plus the
// sanitizer notes that produced it, so a later investigation does not have to
// reproduce the whole stream.
func sanitizeLogReason(failure error, notes []string) string {
	if len(notes) == 0 {
		return failure.Error()
	}
	return failure.Error() + " [notes: " + strings.Join(notes, ",") + "]"
}

// writeRestoredEvents emits the withheld content as a normal answer and closes
// the stream with finish_reason=stop.
func writeRestoredEvents(writer io.Writer, flusher http.Flusher, meta map[string]any, events []sanitize.Event) {
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind == sanitize.EventContent {
			text.WriteString(ev.Text)
		}
	}
	if text.Len() == 0 {
		return
	}
	content := map[string]any{
		"id":      meta["id"],
		"object":  "chat.completion.chunk",
		"created": meta["created"],
		"model":   meta["model"],
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": text.String()},
			"finish_reason": nil,
		}},
	}
	if b, err := json.Marshal(content); err == nil {
		io.WriteString(writer, "data: "+string(b)+"\n\n")
	}
	finish := map[string]any{
		"id":      meta["id"],
		"object":  "chat.completion.chunk",
		"created": meta["created"],
		"model":   meta["model"],
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	if b, err := json.Marshal(finish); err == nil {
		io.WriteString(writer, "data: "+string(b)+"\n\n")
	}
	io.WriteString(writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeThinkAsContentError tells an SSE client that the answer is unusable.
func writeThinkAsContentError(writer io.Writer, flusher http.Flusher) {
	writeSanitizeError(writer, flusher, errThinkAsContent, nil)
}

// writeSanitizeError reports the concrete sanitizer verdict instead of always
// claiming "think-as-content". EMPTY_UPSTREAM tells the client that the endpoint
// returned only delimiter residue, TRUNCATED_THINKING that the endpoint burned
// its budget on reasoning, DSML_UNPARSED that a textual tool call could not be
// recovered — all health signals for the endpoint, not the same defect.
func writeSanitizeError(writer io.Writer, flusher http.Flusher, err error, notes []string) {
	code := "THINK_AS_CONTENT"
	message := errThinkAsContent.Error()
	switch {
	case errors.Is(err, errEmptyUpstream):
		code = "EMPTY_UPSTREAM"
		message = errEmptyUpstream.Error()
	case errors.Is(err, errSanitizePartial):
		code = "SANITIZE_PARTIAL"
		message = errSanitizePartial.Error()
	case errors.Is(err, errTruncatedThinking):
		code = "TRUNCATED_THINKING"
		message = errTruncatedThinking.Error()
	case errors.Is(err, errDsmlUnparsed):
		code = "DSML_UNPARSED"
		message = errDsmlUnparsed.Error()
	case errors.Is(err, ErrStreamTruncated):
		code = "STREAM_TRUNCATED"
		message = err.Error()
	}
	body := map[string]any{
		"message": message + "; response rejected by anyproxyai sanitizer",
		"type":    "upstream_error",
		"code":    code,
	}
	if len(notes) > 0 {
		body["notes"] = notes
	}
	payload, _ := json.Marshal(map[string]any{"error": body})
	io.WriteString(writer, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
	flusher.Flush()
}

// writeAnyStreamFailure picks the frame that matches the failure. A stream that
// was cut off keeps its own STREAM_TRUNCATED code; every other verdict is a
// sanitizer rejection with its own code (see writeSanitizeError).
func writeAnyStreamFailure(writer io.Writer, flusher http.Flusher, err error) {
	if errors.Is(err, ErrStreamTruncated) {
		writeStreamTruncatedError(writer, flusher, err)
		return
	}
	writeSanitizeError(writer, flusher, err, nil)
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if note == want {
			return true
		}
	}
	return false
}
