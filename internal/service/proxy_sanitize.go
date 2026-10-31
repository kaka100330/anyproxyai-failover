package service

import (
	"bytes"
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

// sanitizeThinkingEnabled reports whether the response sanitizer is switched on.
// Missing config values default to false so tests and library users see the
// historical behaviour unless they opt in; LoadConfig sets it to true.
func (s *ProxyService) sanitizeThinkingEnabled() bool {
	return s != nil && s.config != nil && s.config.SanitizeThinking
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
				promptTokens, completionTokens := s.extractTokensFromStreamResponse(raw.String())
				totalTokens := promptTokens + completionTokens
				if rewriter.Poisoned() {
					s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, false, errThinkAsContent.Error())
					log.Warnf("[Sanitize] rejected stream (%d bytes forwarded): %s", bytesWritten, strings.Join(rewriter.Notes(), ","))
					return errThinkAsContent
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
	payload := `{"error":{"message":"upstream returned its reasoning as assistant content (think-as-content); response rejected by anyproxyai sanitizer","type":"upstream_error","code":"THINK_AS_CONTENT"}}`
	io.WriteString(writer, "data: "+payload+"\n\ndata: [DONE]\n\n")
	flusher.Flush()
}
