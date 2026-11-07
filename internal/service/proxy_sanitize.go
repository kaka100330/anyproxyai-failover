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

// sanitizeThinkingEnabled reports whether the response sanitizer is switched on.
// Missing config values default to false so tests and library users see the
// historical behaviour unless they opt in; LoadConfig sets it to true.
func (s *ProxyService) sanitizeThinkingEnabled() bool {
	return s != nil && s.config != nil && s.config.SanitizeThinking
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
// It returns errThinkAsContent when the body was nothing but a copy of the
// reasoning, so the caller can retry instead of forwarding it.
func (s *ProxyService) sanitizeNonStreamBody(body []byte) ([]byte, error) {
	out, notes, poison := sanitize.ApplyToBody(body)
	if len(notes) > 0 {
		log.Warnf("[Sanitize] repaired upstream response: %s", strings.Join(notes, ","))
	}
	if poison {
		log.Warnf("[Sanitize] rejected response: upstream returned reasoning as assistant content")
		return nil, errThinkAsContent
	}
	return out, nil
}

// streamDirectSanitized is streamDirect with the SSE stream rewritten through
// the sanitizer. dropReasoning suppresses reasoning frames, which is what a
// retry wants: the client already received them from the first attempt.
//
// The returned error is errThinkAsContent when the upstream produced only a
// copy of its reasoning. Nothing is written to the client in that case beyond
// the reasoning frames themselves.
func (s *ProxyService) streamDirectSanitized(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64, dropReasoning bool) error {
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
				if rewriter.Poisoned() {
					failure := errThinkAsContent
					switch {
					case rewriter.EmittedContent() != "":
						failure = errSanitizePartial
					case hasNote(rewriter.Notes(), sanitize.NoteEmptyAfter) &&
						!hasNote(rewriter.Notes(), sanitize.NoteDuplicate) &&
						!hasNote(rewriter.Notes(), sanitize.NoteDsmlUnparsed):
						failure = errEmptyUpstream
					}
					s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, false, failure.Error())
					log.Warnf("[Sanitize] rejected stream (%d bytes forwarded): %s", bytesWritten, strings.Join(rewriter.Notes(), ","))
					return failure
				}
				// 上游没有给出任何结束标记就断流：判定为该端点失败，
				// 由调用方冷却端点，客户端收到明确的截断错误而不是"成功但答案不完整"。
				outcome := inspectUpstreamStream(rawBody)
				if msg := rewriter.UpstreamError(); msg != "" && outcome.UpstreamErr == "" {
					outcome.UpstreamErr = msg
				}
				if outcome.Failed() {
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

// writeThinkAsContentError tells an SSE client that the answer is unusable.
func writeThinkAsContentError(writer io.Writer, flusher http.Flusher) {
	writeSanitizeError(writer, flusher, errThinkAsContent)
}

// writeSanitizeError reports the concrete sanitizer verdict instead of always
// claiming "think-as-content". EMPTY_UPSTREAM in particular tells the client
// that the endpoint returned only delimiter residue, which is a health signal.
func writeSanitizeError(writer io.Writer, flusher http.Flusher, err error) {
	code := "THINK_AS_CONTENT"
	message := errThinkAsContent.Error()
	switch {
	case errors.Is(err, errEmptyUpstream):
		code = "EMPTY_UPSTREAM"
		message = errEmptyUpstream.Error()
	case errors.Is(err, errSanitizePartial):
		code = "SANITIZE_PARTIAL"
		message = errSanitizePartial.Error()
	}
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message + "; response rejected by anyproxyai sanitizer",
			"type":    "upstream_error",
			"code":    code,
		},
	})
	io.WriteString(writer, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
	flusher.Flush()
}

func hasNote(notes []string, want string) bool {
	for _, note := range notes {
		if note == want {
			return true
		}
	}
	return false
}
