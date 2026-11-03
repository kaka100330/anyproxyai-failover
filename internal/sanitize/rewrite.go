package sanitize

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// reasoningFieldOrder lists the field names OpenAI-compatible servers use for
// chain of thought, in priority order.
var reasoningFieldOrder = []string{"reasoning_content", "reasoning", "reasoning_text"}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func reasoningFrom(msg map[string]any) (text string, field string) {
	for _, f := range reasoningFieldOrder {
		if s, ok := msg[f].(string); ok && s != "" {
			return s, f
		}
	}
	return "", reasoningFieldOrder[0]
}

func notesJoined(notes []string) string {
	sort.Strings(notes)
	return strings.Join(notes, ",")
}

// ApplyToBody sanitizes a non-streaming OpenAI chat completion body.
//
// It returns the rewritten body, the notes that describe what was repaired and
// whether the response is poisoned (reasoning was the only content, so the
// client must not treat it as an answer). Bodies that need no repair are
// returned untouched, byte for byte.
func ApplyToBody(body []byte) (out []byte, notes []string, poison bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, nil, false
	}
	choices, ok := root["choices"].([]any)
	if !ok || len(choices) == 0 {
		return body, nil, false
	}

	changed := false
	allNotes := map[string]bool{}

	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		reasoning, field := reasoningFrom(msg)
		content := asString(msg["content"])
		tools, _ := msg["tool_calls"].([]any)

		g := NewGuard()
		g.Push(reasoning, content, tools)
		g.Finish()
		n := g.Notes()
		if len(n) == 0 {
			continue
		}
		for _, note := range n {
			allNotes[note] = true
		}
		changed = true

		msg["content"] = g.EmittedContent()
		if r := g.EmittedReasoning(); r != "" {
			msg[field] = r
		} else if reasoning != "" {
			// The whole reasoning field was delimiter residue. Clear it
			// instead of leaving the original dirty text in the body.
			msg[field] = ""
		}
		toolCalls := g.ToolCalls()
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCallsJSON(toolCalls)
			choice["finish_reason"] = "tool_calls"
		}
		if g.Retryable() && len(toolCalls) == 0 {
			poison = true
		}
	}

	if !changed {
		return body, nil, false
	}

	repairedCount.Add(1)
	notes = make([]string, 0, len(allNotes))
	for k := range allNotes {
		notes = append(notes, k)
	}
	sort.Strings(notes)

	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, notes, poison
	}
	return rewritten, notes, poison
}

func toolCallsJSON(calls []ToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	return out
}

// SSERewriter turns an upstream OpenAI SSE stream into a sanitized one.
//
// It is an io.Reader so the proxy can keep streaming: bytes are produced as
// soon as the guard is sure they are not part of a marker.
type SSERewriter struct {
	src   *bufio.Reader
	out   bytes.Buffer
	guard *Guard
	done  bool

	base           map[string]any
	reasoningField string
	finishSent     bool
	poison         bool
	dropReasoning  bool
	silent         bool
}

// SetDropReasoning suppresses reasoning frames (used when retrying a stream
// whose reasoning the client has already received).
func (s *SSERewriter) SetDropReasoning(v bool) { s.dropReasoning = v }

// SetSilent stops the rewriter from writing its own error frame, leaving the
// decision to the caller.
func (s *SSERewriter) SetSilent(v bool) { s.silent = v }

// NewSSERewriter wraps an upstream SSE body.
func NewSSERewriter(r io.Reader) *SSERewriter {
	return &SSERewriter{
		src:            bufio.NewReaderSize(r, 64*1024),
		guard:          NewGuard(),
		base:           map[string]any{"object": "chat.completion.chunk"},
		reasoningField: reasoningFieldOrder[0],
	}
}

// Poisoned reports whether the stream ended in a state that must not be shown
// to the client as a finished answer.
func (s *SSERewriter) Poisoned() bool { return s.poison }

// Notes returns the repair notes collected for this stream.
func (s *SSERewriter) Notes() []string { return s.guard.Notes() }

// EmittedContent reports the answer text already written to the client.  The
// caller uses it to decide whether a rejected stream may be retried: once
// content has been forwarded, a retry would duplicate the partial answer.
func (s *SSERewriter) EmittedContent() string { return s.guard.EmittedContent() }

// Read implements io.Reader.
func (s *SSERewriter) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && !s.done {
		if err := s.step(); err != nil {
			if err == io.EOF {
				s.done = true
				break
			}
			return 0, err
		}
	}
	if s.out.Len() == 0 {
		return 0, io.EOF
	}
	return s.out.Read(p)
}

func (s *SSERewriter) step() error {
	line, err := s.src.ReadString('\n')
	// Process whatever we got, even on EOF (last line may lack a newline).
	if line == "" && err != nil {
		if err == io.EOF && !s.finishSent {
			s.finishSent = true
			s.emitEvents(s.guard.Finish())
			if s.guard.Retryable() {
				s.poison = true
				if !s.silent && s.guard.EmittedContent() == "" {
					s.writeError()
				}
				s.done = true
				return nil
			}
			s.writeFinish("stop", nil)
		}
		return err
	}

	if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "data:") {
		s.out.WriteString(line)
		if err != nil && err != io.EOF {
			return err
		}
		return nil
	}

	payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " \t"), "data:"))
	if payload == "" {
		s.out.WriteString(line)
		return nil
	}
	if payload == "[DONE]" {
		if !s.finishSent {
			s.finishSent = true
			s.emitEvents(s.guard.Finish())
			if s.guard.Retryable() {
				s.poison = true
				if !s.silent && s.guard.EmittedContent() == "" {
					s.writeError()
				}
				s.done = true
				return nil
			}
			s.writeFinish("stop", nil)
		}
		s.out.WriteString("data: [DONE]\n\n")
		s.done = true
		return nil
	}

	var chunk map[string]any
	if jsonErr := json.Unmarshal([]byte(payload), &chunk); jsonErr != nil {
		s.out.WriteString(line)
		return nil
	}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint"} {
		if v, ok := chunk[k]; ok {
			s.base[k] = v
		}
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		// Usage-only chunk: forward untouched.
		s.out.WriteString(line)
		return nil
	}
	if len(choices) > 1 {
		// n>1 is outside what the guard models; pass through.
		s.out.WriteString(line)
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		s.out.WriteString(line)
		return nil
	}

	reasoning := ""
	for _, f := range reasoningFieldOrder {
		if v, ok := delta[f].(string); ok && v != "" {
			reasoning = v
			s.reasoningField = f
			break
		}
	}
	content := asString(delta["content"])
	tools, _ := delta["tool_calls"].([]any)
	finish := asString(choice["finish_reason"])

	if reasoning == "" && content == "" && len(tools) == 0 && finish == "" {
		// role-only (or empty) delta: keep the frame as-is.
		s.out.WriteString(line)
		return nil
	}

	if reasoning != "" || content != "" || len(tools) > 0 {
		s.emitEvents(s.guard.Push(reasoning, content, tools))
		if len(tools) > 0 {
			// Upstream already produced real tool calls; forward them verbatim.
			s.out.WriteString(line)
		}
	}

	if finish != "" {
		fr := finish
		s.finishSent = true
		s.emitEvents(s.guard.Finish())
		if s.guard.Retryable() {
			s.poison = true
			rejectedCount.Add(1)
			if !s.silent && s.guard.EmittedContent() == "" {
				s.writeError()
			}
			s.done = true
			return nil
		}
		var usage map[string]any
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		if len(s.guard.ToolCalls()) > 0 {
			fr = "tool_calls"
		}
		s.writeFinish(fr, usage)
	}
	return nil
}

func (s *SSERewriter) emitEvents(events []Event) {
	for _, ev := range events {
		switch ev.Kind {
		case EventReasoning:
			if s.dropReasoning {
				continue
			}
			if ev.Text != "" {
				s.writeChunk(map[string]any{s.reasoningField: ev.Text}, nil, nil)
			}
		case EventContent:
			if ev.Text != "" {
				s.writeChunk(map[string]any{"content": ev.Text}, nil, nil)
			}
		case EventTools:
			if ev.Passthrough {
				continue
			}
			calls := make([]any, 0, len(ev.Calls))
			for i, c := range ev.Calls {
				calls = append(calls, map[string]any{
					"index": i,
					"id":    c.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      c.Name,
						"arguments": c.Arguments,
					},
				})
			}
			s.writeChunk(map[string]any{"tool_calls": calls}, nil, nil)
		}
	}
}

func (s *SSERewriter) writeChunk(delta map[string]any, finish any, usage map[string]any) {
	chunk := make(map[string]any, len(s.base)+2)
	for k, v := range s.base {
		chunk[k] = v
	}
	chunk["choices"] = []any{map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finish,
	}}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	s.out.WriteString("data: ")
	s.out.Write(b)
	s.out.WriteString("\n\n")
}

func (s *SSERewriter) writeFinish(reason string, usage map[string]any) {
	s.writeChunk(map[string]any{}, reason, usage)
}

func (s *SSERewriter) writeError() {
	// Loud failure: the client must retry or surface an error, never present a
	// copy of the reasoning as the final answer.
	payload := map[string]any{"error": map[string]any{
		"message": "upstream returned its reasoning as assistant content (think-as-content); response rejected by anyproxyai sanitizer",
		"type":    "upstream_error",
		"code":    "THINK_AS_CONTENT",
	}}
	b, _ := json.Marshal(payload)
	s.out.WriteString("data: ")
	s.out.Write(b)
	s.out.WriteString("\n\ndata: [DONE]\n\n")
}
