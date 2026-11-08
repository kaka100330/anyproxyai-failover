// Package sanitize repairs OpenAI-compatible responses whose reasoning leaks
// into the assistant content.
//
// Three defects are handled, all observed in the wild (hyhawang "国模自动路由",
// plus assorted OpenAI-compatible relays that re-wrap native DeepSeek output):
//
//  1. content wrapped in raw <thinking>...</thinking> tags  -> move to reasoning
//  2. content that is a byte-for-byte copy of the reasoning   -> drop content
//  3. DeepSeek-native tool calls leaked as <｜｜DSML｜｜ invoke ...> text
//     -> parse back into real tool_calls
//
// Defect 2 is the dangerous one for agent clients: the response carries no
// tool_calls and finishes with finish_reason=stop, so the client treats a copy
// of the chain of thought as the final answer and silently ends the turn.
// The guard therefore reports such a message as "poisoned" so the caller can
// fail loudly instead of handing fabricated content to the client.
//
// The same Guard drives both the streaming and the non-streaming path so the
// two cannot drift apart.
package sanitize

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// tailHold is the number of trailing bytes kept back while streaming: a DSML
// marker (or a <thinking prefix) may straddle two chunks, so the last few
// bytes cannot be emitted until we know they are not part of a marker.
const tailHold = 32

// dupMin is the minimum content length before "content copies reasoning" is
// considered a defect. Short answers legitimately repeat a short thought.
const dupMin = 24

// Notes reported when a defect was found.
const (
	NoteDuplicate        = "content_duplicates_reasoning"
	NoteEmptyAfter       = "empty_after_sanitize"
	NoteDsmlRecovered    = "dsml_recovered"
	NoteDsmlUnparsed     = "dsml_unparsed"
	NoteOrphanDsml       = "dsml_orphan_tail_dropped"
	NoteThinkingTagTaken = "thinking_tag_extracted"
	NoteChannelCleaned   = "channel_delimiters_cleaned"
	// NoteDuplicateRestored is the last-resort verdict: the content channel was
	// a copy of the reasoning, but every endpoint had already failed, so the
	// withheld text was handed to the client instead of an empty error. It is
	// never set by the guard itself — the caller flips it via
	// RestoreWithheldContent so the failure path stays the default.
	NoteDuplicateRestored = "duplicate_content_restored"
)

var markers = []string{"<｜｜DSML｜｜", "<|DSML|", "<｜DSML｜>", "</｜｜DSML｜｜", "</|DSML|", "</｜DSML｜>"}

// ToolCall is a reconstructed OpenAI tool call.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// EventKind identifies what a guard event carries.
type EventKind string

const (
	EventReasoning EventKind = "reasoning"
	EventContent   EventKind = "content"
	EventTools     EventKind = "tools"
)

// Event is one unit of sanitized output, in stream order.
type Event struct {
	Kind  EventKind
	Text  string
	Calls []ToolCall
	// Passthrough means the calls came from the upstream tool_calls delta and
	// must be forwarded untouched.
	Passthrough bool
}

// guardCallSeq keeps reconstructed tool-call ids unique inside one process.
var guardCallSeq int64

// findMarker returns the earliest DSML marker index in text[from:to), or -1.
func findMarker(text string, from, to int) int {
	if to > len(text) {
		to = len(text)
	}
	best := -1
	for _, m := range markers {
		if i := strings.Index(text[from:], m); i >= 0 {
			i += from
			if i < to && (best < 0 || i < best) {
				best = i
			}
		}
	}
	return best
}

// normalizeDsml rewrites every marker spelling to the canonical tag.
func normalizeDsml(text string) string {
	// Closing tags first: "</｜｜DSML｜｜" must not be rewritten into
	// "</<|DSML|" by the generic rule below.
	text = strings.ReplaceAll(text, "</｜｜DSML｜｜", "</|DSML|")
	text = strings.ReplaceAll(text, "</｜DSML｜>", "</|DSML|")
	text = strings.ReplaceAll(text, "｜｜DSML｜｜", "<|DSML|")
	text = strings.ReplaceAll(text, "｜DSML｜>", "<|DSML|")
	return text
}

func coerce(raw string) any {
	v := strings.TrimSpace(raw)
	switch v {
	case "":
		return ""
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	var num json.Number
	if err := json.Unmarshal([]byte(v), &num); err == nil {
		if n, err2 := num.Float64(); err2 == nil {
			return n
		}
	}
	if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
		var anyVal any
		if err := json.Unmarshal([]byte(v), &anyVal); err == nil {
			return anyVal
		}
	}
	return v
}

// parseDsml parses DeepSeek-native tool call blocks. ok=false means a block was
// seen but no call could be reconstructed.
func parseDsml(text string) (calls []ToolCall, ok bool) {
	t := normalizeDsml(text)
	invokeRe := mustRe(`<\|DSML\|\s*invoke\s+name="([^"]*)"\s*>([\s\S]*?)(</\|DSML\|\s*invoke>|$)`)
	paramRe := mustRe(`<\|DSML\|\s*parameter\s+name="([^"]*)"\s*>([\s\S]*?)(</\|DSML\|\s*parameter>|$)`)

	for _, m := range invokeRe.FindAllStringSubmatch(t, -1) {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		args := map[string]any{}
		for _, p := range paramRe.FindAllStringSubmatch(m[2], -1) {
			args[strings.TrimSpace(p[1])] = coerce(p[2])
		}
		payload, err := json.Marshal(args)
		if err != nil {
			payload = []byte("{}")
		}
		calls = append(calls, ToolCall{Name: name, Arguments: string(payload)})
	}
	return calls, len(calls) > 0
}

func newCallID() string {
	n := atomic.AddInt64(&guardCallSeq, 1)
	return fmt.Sprintf("call_guard_%s_%d", strings.ToLower(strconvBase36(time.Now().UnixNano())), n)
}

func strconvBase36(v int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if v == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%36]
		v /= 36
	}
	return string(buf[i:])
}

// Guard is the incremental state machine for one assistant message.
type Guard struct {
	notes map[string]bool

	reasoning   string
	reasonEmit  int
	content     string
	contentEmit int
	pending     string
	mode        string // unknown | text | thinking
	dup         bool

	dsmlSource string // "" | "content" | "reasoning"
	dsmlBuf    string

	upstreamTools bool
	toolCalls     []ToolCall

	emittedContent   string
	emittedReasoning string
	finished         bool
}

// NewGuard returns an empty guard.
func NewGuard() *Guard {
	return &Guard{notes: map[string]bool{}, mode: "unknown"}
}

// Notes returns the notes collected so far.
func (g *Guard) Notes() []string {
	out := make([]string, 0, len(g.notes))
	for _, k := range []string{NoteThinkingTagTaken, NoteChannelCleaned, NoteDuplicate, NoteDsmlRecovered, NoteDsmlUnparsed, NoteOrphanDsml, NoteEmptyAfter, NoteDuplicateRestored} {
		if g.notes[k] {
			out = append(out, k)
		}
	}
	return out
}

// Retryable reports whether the response was a defect that the caller should
// treat as failed rather than as a valid answer.
func (g *Guard) Retryable() bool {
	return g.notes[NoteDuplicate] || g.notes[NoteEmptyAfter] || g.notes[NoteDsmlUnparsed]
}

// RestoreWithheldContent flushes the content channel that was suppressed
// because it duplicated the reasoning.
//
// It is the last-resort recovery for a poisoned response: once every endpoint
// of the group has failed there is nothing left to fail over to, and an error
// frame only ends the client's turn with an empty answer. Handing over the
// withheld text at least keeps the turn alive, and the
// duplicate_content_restored note makes the compromise visible in the logs.
//
// It returns nil when there is nothing withheld (content was already emitted,
// or the poison came from something other than duplication).
func (g *Guard) RestoreWithheldContent() []Event {
	if !g.notes[NoteDuplicate] || g.emittedContent != "" {
		return nil
	}
	if g.dsmlSource != "" {
		// A tool-call block took over the buffer; replaying it as text would
		// spill DSML markers into the answer.
		return nil
	}
	g.dup = false
	var out []Event
	g.flushContent(&out, true)
	if g.emittedContent == "" {
		return nil
	}
	g.notes[NoteDuplicateRestored] = true
	return out
}

// EmittedContent is the sanitized answer text handed to the client.
func (g *Guard) EmittedContent() string { return g.emittedContent }

// EmittedReasoning is the sanitized reasoning text.
func (g *Guard) EmittedReasoning() string { return g.emittedReasoning }

// ToolCalls returns the reconstructed calls, nil when none were found.
func (g *Guard) ToolCalls() []ToolCall { return g.toolCalls }

// HasOutput reports whether anything other than reasoning was emitted.
func (g *Guard) HasOutput() bool {
	return g.emittedContent != "" || len(g.toolCalls) > 0 || g.upstreamTools
}

// Push feeds one streamed delta into the guard.
func (g *Guard) Push(reasoning, content string, toolCalls []any) []Event {
	var out []Event
	if reasoning != "" {
		g.pushReasoning(reasoning, &out)
	}
	if content != "" {
		g.pushContent(content, &out)
	}
	if len(toolCalls) > 0 {
		g.upstreamTools = true
		out = append(out, Event{Kind: EventTools, Passthrough: true})
	}
	return out
}

func (g *Guard) emitReasoning(end int, out *[]Event) {
	if end <= g.reasonEmit {
		return
	}
	end = safeCut(g.reasoning, end)
	end = runeBoundary(g.reasoning, end)
	if end <= g.reasonEmit {
		return
	}
	raw := g.reasoning[g.reasonEmit:end]
	g.reasonEmit = end
	cleaned := cleanReasoningChunk(raw)
	text := cleaned
	if end == len(g.reasoning) {
		text = strings.TrimSpace(text)
	}
	if cleaned != raw {
		g.notes[NoteChannelCleaned] = true
	}
	if text == "" {
		return
	}
	g.emittedReasoning += text
	*out = append(*out, Event{Kind: EventReasoning, Text: text})
}

func (g *Guard) pushReasoning(text string, out *[]Event) {
	g.reasoning += text
	if g.dsmlSource == "reasoning" {
		g.dsmlBuf += text
		return
	}
	if g.mode == "thinking" {
		if closeIdx := indexCloseThink(g.reasoning); closeIdx >= 0 && closeIdx >= g.reasonEmit {
			inner := g.reasoning[closeIdx:]
			m := closeThinkRe.FindString(inner)
			g.emitReasoning(closeIdx, out)
			after := g.reasoning[closeIdx+len(m):]
			g.reasoning = g.reasoning[:closeIdx]
			g.reasonEmit = len(g.reasoning)
			g.mode = "text"
			if after != "" {
				g.pushContent(after, out)
			}
			return
		}
	}
	if mk := findMarker(g.reasoning, g.reasonEmit, len(g.reasoning)); mk >= 0 {
		g.emitReasoning(mk, out)
		g.dsmlSource = "reasoning"
		g.dsmlBuf = g.reasoning[mk:]
		g.reasonEmit = len(g.reasoning)
		return
	}
	g.emitReasoning(len(g.reasoning)-tailHold, out)
}

func (g *Guard) pushContent(text string, out *[]Event) {
	if g.mode == "unknown" {
		g.pending += text
		t := strings.TrimLeft(g.pending, " \t\r\n")
		lower := strings.ToLower(t)
		// Hold a prefix that may still become <thinking>, <think> or
		// <analysis>.  The old code only knew <thinking>, so a stream that
		// started with "<think>" was emitted as body text before the tag could
		// be recognised.
		if len(t) < len("<analysis") {
			for _, tag := range []string{"<thinking", "<think", "<analysis"} {
				if strings.HasPrefix(tag, lower) {
					return
				}
			}
		}
		if loc := openThinkAtStartRe.FindStringIndex(t); loc != nil {
			rest := t[loc[1]:]
			g.pending = ""
			g.mode = "thinking"
			g.notes[NoteThinkingTagTaken] = true
			if rest != "" {
				g.pushReasoning(rest, out)
			}
			return
		}
		g.mode = "text"
		p := g.pending
		g.pending = ""
		if p != "" {
			g.appendContent(p, out)
		}
		return
	}
	if g.mode == "thinking" {
		g.pushReasoning(text, out)
		return
	}
	g.appendContent(text, out)
}

func (g *Guard) appendContent(chunk string, out *[]Event) {
	g.content += chunk
	if g.dsmlSource == "content" {
		g.dsmlBuf += chunk
		return
	}
	if !g.dup && g.reasoning != "" && len(g.content) >= dupMin && strings.HasPrefix(g.reasoning, g.content) {
		g.dup = true
	}
	if g.dup {
		if !strings.HasPrefix(g.reasoning, g.content) {
			g.dup = false
			g.notes[NoteDuplicate] = true
			g.flushContent(out, true)
		}
		return
	}
	g.flushContent(out, false)
}

func (g *Guard) flushContent(out *[]Event, all bool) {
	limit := len(g.content) - tailHold
	if all {
		limit = len(g.content)
	}
	if limit <= g.contentEmit {
		return
	}
	if !all {
		limit = safeCut(g.content, limit)
	}
	mk := findMarker(g.content, g.contentEmit, limit)
	end := limit
	if mk >= 0 {
		end = mk
	}
	if end > g.contentEmit {
		end = runeBoundary(g.content, end)
	}
	if end > g.contentEmit {
		raw := g.content[g.contentEmit:end]
		cleaned := cleanContentChunk(raw)
		text := cleaned
		g.contentEmit = end
		if all {
			text = strings.TrimSpace(text)
		}
		if cleaned != raw {
			g.notes[NoteChannelCleaned] = true
		}
		if text != "" {
			g.emittedContent += text
			*out = append(*out, Event{Kind: EventContent, Text: text})
		}
	}
	if mk >= 0 {
		g.dsmlSource = "content"
		g.dsmlBuf = g.content[mk:]
		g.contentEmit = len(g.content)
	}
}

// Finish flushes everything still held back and reconstructs tool calls.
func (g *Guard) Finish() []Event {
	var out []Event
	if g.finished {
		return out
	}
	g.finished = true

	if g.mode == "unknown" && g.pending != "" {
		// Feed the held prefix through the normal content path so a
		// single-chunk "<think>...</think>answer" is split correctly at EOF.
		pending := g.pending
		g.pending = ""
		g.pushContent(pending, &out)
		if g.pending != "" {
			// The stream ended in the middle of a possible opening tag; it
			// cannot become a wrapper any more, so treat it as text.
			g.mode = "text"
			rest := g.pending
			g.pending = ""
			g.appendContent(rest, &out)
		}
	}
	if g.dsmlSource != "reasoning" {
		g.emitReasoning(len(g.reasoning), &out)
	}

	for {
		switch {
		case g.dsmlSource != "":
			g.finishDsml(&out)
		case g.dup && strings.HasPrefix(g.reasoning, g.content):
			// The whole content was a copy of the reasoning: nothing was emitted.
			g.notes[NoteDuplicate] = true
			g.dup = false
		default:
			g.flushContent(&out, true)
			// flushContent may discover a DSML marker that was held back in
			// the tail.  It has to be parsed here as well, otherwise a marker
			// arriving in the last 32 bytes would silently become content.
			if g.dsmlSource == "" {
				goto finished
			}
			continue
		}
		break
	}
finished:

	if g.emittedContent == "" && len(g.toolCalls) == 0 && !g.upstreamTools {
		g.notes[NoteEmptyAfter] = true
	}
	return out
}

// finishDsml resolves the DSML buffer collected from either channel.  Closing
// residue is stripped (and any prose around it kept); only a real but
// malformed invoke remains a hard failure.
func (g *Guard) finishDsml(out *[]Event) {
	calls, ok := parseDsml(g.dsmlBuf)
	switch {
	case ok:
		for i := range calls {
			calls[i].ID = newCallID()
		}
		g.toolCalls = calls
		*out = append(*out, Event{Kind: EventTools, Calls: calls})
		g.notes[NoteDsmlRecovered] = true
		g.dsmlSource = ""
		g.dsmlBuf = ""
	case IsOrphanDsmlTail(g.dsmlBuf):
		// Only closing tags arrived (the model collapsed into emitting
		// </parameter></invoke></calls>).  There is no call to recover,
		// but this must not be reported as an unparsable tool call: doing
		// so turned a completely empty response into a hard
		// THINK_AS_CONTENT failure.
		g.notes[NoteOrphanDsml] = true
		g.dsmlSource = ""
		g.dsmlBuf = ""
		g.flushContent(out, true)
	default:
		if prose, residue := DsmlResidue(g.dsmlBuf); residue {
			g.notes[NoteOrphanDsml] = true
			g.dsmlSource = ""
			g.dsmlBuf = ""
			if prose != "" {
				g.content += prose
			}
			g.flushContent(out, true)
			return
		}
		g.notes[NoteDsmlUnparsed] = true
		g.dsmlSource = ""
		g.dsmlBuf = ""
	}
}

// runeBoundary moves i back to the start of a UTF-8 sequence.
func runeBoundary(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
