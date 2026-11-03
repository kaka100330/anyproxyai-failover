package sanitize

import (
	"regexp"
	"strings"
)

// The sanitizer has to cope with several different spellings of the same
// defect.  The original implementation only knew the literal <thinking> tag,
// which is why real sessions leaked <think>, <analysis> and bare closing tags
// straight into the assistant body.
var (
	// openThinkAtStartRe matches a reasoning wrapper only at the beginning of
	// the content field.  A wrapper in the middle of a normal answer is much
	// more likely to be quoted text and must not be moved.
	openThinkAtStartRe = regexp.MustCompile(`(?i)^\s*<(thinking|think|analysis)(?:\s[^>]*)?>?`)

	// closeThinkRe matches every known closing delimiter, including the
	// singular <think> form emitted by several DeepSeek relays.
	closeThinkRe = regexp.MustCompile(`(?i)</(thinking|think|analysis)\s*>`)

	// anyOpenThinkRe is used to remove an opening delimiter that ended up in
	// the reasoning channel or in the middle of an answer.
	anyOpenThinkRe = regexp.MustCompile(`(?i)<(thinking|think|analysis)\b[^>]*>`)

	// acpBlockRe / controlBlockRe remove DSH transport markers that some
	// upstreams echo back into the model output.  They are never user-visible
	// content and they make the model repeat the pattern on the next turn.
	acpBlockRe     = regexp.MustCompile(`(?is)<acp\b[^>]*>.*?</acp>`)
	controlBlockRe = regexp.MustCompile(`(?is)<control>.*?</control>`)

	// lineQuotePrefixRe strips Markdown blockquote markers at the beginning of
	// every line.  A reasoning message that starts with " > > " is rendered by
	// the client as nested quotes; the model then copies the markers into the
	// next turn and the nesting grows.  Reasoning is internal, so removing a
	// leading quote marker there is safe.
	lineQuotePrefixRe = regexp.MustCompile(`(?m)^[ \t]*(?:>[ \t]*)+`)

	// orphanDsmlTailRe matches a DSML envelope that contains only closing
	// tags (optionally with the opening tool_calls marker).  parseDsml needs a
	// real <invoke name="...">; an envelope made only of closers is residue,
	// not a tool call, and must not poison the stream.
	orphanDsmlTagRe = regexp.MustCompile(`(?i)</?\|DSML\|(?:\s+(?:tool_calls|parameter|invoke|calls))?\s*>`)

	// dsmlAnyTagRe / dsmlInvokeRe decide whether a failed DSML block is
	// residue (safe to strip and keep the prose) or a genuine but malformed
	// tool call (must be rejected so the router can fail over).
	dsmlAnyTagRe = regexp.MustCompile(`(?i)</?\|DSML\|[^>]*>`)
	dsmlInvokeRe = regexp.MustCompile(`(?i)<\|DSML\|\s*invoke\b`)
)

// NormalizeDsml rewrites every known DSML marker spelling to the canonical
// form.  It is exported so request-history cleaning uses exactly the same
// rules as response parsing.
func NormalizeDsml(text string) string { return normalizeDsml(text) }

// safeCut backs an emission boundary out of a partially received tag.  A
// delta can end in the middle of "</thinking>" or "<|DSML| parameter>";
// emitting that half would defeat the regex cleanup, so keep it for the next
// round (or for Finish) instead.
func safeCut(s string, end int) int {
	if end <= 0 || end >= len(s) {
		return end
	}
	if i := strings.LastIndexByte(s[:end], '<'); i >= 0 && end-i <= 32 {
		if !strings.Contains(s[i:end], ">") {
			return i
		}
	}
	return end
}

// CleanReasoning removes channel delimiters and transport noise from a
// reasoning block.  It deliberately does not try to interpret the text: the
// goal is to keep the model's own reasoning, not to turn it into an answer.
func CleanReasoning(text string) string {
	return strings.TrimSpace(cleanReasoningChunk(text))
}

// cleanReasoningChunk is the streaming-safe variant of CleanReasoning.  It
// must not trim leading/trailing spaces: a delta may end in the middle of a
// word ("hello " + "world"), and trimming each chunk would join the words.
func cleanReasoningChunk(text string) string {
	text = acpBlockRe.ReplaceAllString(text, "")
	text = controlBlockRe.ReplaceAllString(text, "")
	text = closeThinkRe.ReplaceAllString(text, "")
	text = anyOpenThinkRe.ReplaceAllString(text, "")
	text = lineQuotePrefixRe.ReplaceAllString(text, "")
	return text
}

// cleanContentChunk removes delimiters from a content delta without trimming
// the edges, for the same reason as cleanReasoningChunk.
func cleanContentChunk(text string) string {
	text = acpBlockRe.ReplaceAllString(text, "")
	text = controlBlockRe.ReplaceAllString(text, "")
	text = closeThinkRe.ReplaceAllString(text, "")
	text = anyOpenThinkRe.ReplaceAllString(text, "")
	return text
}

// SplitThinkingContent separates a content block that is wrapped in a
// reasoning delimiter.  It returns the answer part and the reasoning part.
//
// Only a delimiter at the very start of content is treated as a wrapper.  If
// the closing delimiter is absent, the whole remainder is reasoning: an
// unterminated <analysis> block is exactly the long chain-of-thought leak seen
// in the wild.
func SplitThinkingContent(text string) (content string, reasoning string) {
	loc := openThinkAtStartRe.FindStringIndex(text)
	if loc == nil {
		// A stray closing tag without an opening tag is residue.  Drop it, but
		// keep any prose around it.
		if closeThinkRe.MatchString(text) {
			return strings.TrimSpace(closeThinkRe.ReplaceAllString(text, "")), ""
		}
		return text, ""
	}

	rest := text[loc[1]:]
	if closeLoc := closeThinkRe.FindStringIndex(rest); closeLoc != nil {
		return strings.TrimSpace(rest[closeLoc[1]:]), strings.TrimSpace(rest[:closeLoc[0]])
	}
	return "", strings.TrimSpace(rest)
}

// CleanContent removes delimiters and transport noise from a normal answer.
// The second return value is reasoning that was wrapped inside the content
// field; callers should move it to the reasoning channel.
func CleanContent(text string) (content string, reasoning string) {
	content, reasoning = SplitThinkingContent(text)
	content = acpBlockRe.ReplaceAllString(content, "")
	content = controlBlockRe.ReplaceAllString(content, "")
	content = closeThinkRe.ReplaceAllString(content, "")
	content = anyOpenThinkRe.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)
	if IsOrphanDsmlTail(content) {
		content = ""
	}
	return content, reasoning
}

// IsOrphanDsmlTail reports whether text consists only of DSML envelope
// closers (and whitespace).  Such a fragment cannot be turned into a tool
// call; treating it as an unparsable call was the cause of the fatal
// THINK_AS_CONTENT error in the original incident.
func IsOrphanDsmlTail(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.Contains(trimmed, "DSML") {
		return false
	}
	normalized := NormalizeDsml(trimmed)
	rest := orphanDsmlTagRe.ReplaceAllString(normalized, "")
	rest = strings.TrimSpace(rest)
	return rest == ""
}

// DsmlResidue strips a failed DSML envelope that contains no <invoke> at all
// and returns the prose that surrounded it.  A block that does contain an
// invoke is a real (if malformed) tool call, not residue, and is reported as
// unparsable so the caller can fail over instead of silently turning tool
// arguments into an answer.
func DsmlResidue(text string) (prose string, ok bool) {
	normalized := NormalizeDsml(text)
	if !strings.Contains(normalized, "DSML") || dsmlInvokeRe.MatchString(normalized) {
		return "", false
	}
	prose = dsmlAnyTagRe.ReplaceAllString(normalized, "")
	return strings.TrimSpace(prose), true
}

// CleanAssistantHistory removes reasoning replay and leaked chain-of-thought
// from an OpenAI request's assistant messages.  It is called before the
// request is forwarded upstream.
//
// Two things are intentionally aggressive here:
//
//  1. reasoning_content / reasoning / reasoning_text is deleted.  OpenAI's
//     chat protocol has no standard slot for replaying chain-of-thought, and
//     DeepSeek documents that reasoning_content should not be sent back.  The
//     original incident was a feedback loop: the client replayed malformed
//     reasoning, the model copied its own closing tags, and the next response
//     was worse.
//  2. an assistant message whose content is only a reasoning wrapper (or only
//     closing tags) is removed if it has no tool calls.  Keeping an empty or
//     poisoned assistant turn in context is worse than dropping it.
//
// The function mutates req in place and returns the number of messages
// changed plus a small set of notes for logging.
func CleanAssistantHistory(req map[string]any) (changed int, notes []string) {
	if req == nil {
		return 0, nil
	}
	rawMessages, ok := req["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return 0, nil
	}

	out := make([]any, 0, len(rawMessages))
	droppedReasoning := 0
	droppedContent := 0
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			out = append(out, raw)
			continue
		}

		messageChanged := false
		for _, field := range reasoningFieldOrder {
			if _, exists := msg[field]; exists {
				delete(msg, field)
				messageChanged = true
				droppedReasoning++
			}
		}

		content, moved, contentChanged := cleanMessageContent(msg["content"])
		if moved != "" {
			// The reasoning wrapper has been removed; do not put it back into
			// reasoning_content either.  The goal is to stop replay entirely.
			messageChanged = true
		}
		if contentChanged {
			msg["content"] = content
			messageChanged = true
		}

		hasTools := false
		if tools, ok := msg["tool_calls"].([]any); ok && len(tools) > 0 {
			hasTools = true
		}
		if isEmptyContent(content) && !hasTools {
			// A pure reasoning/tool-residue turn carries no usable context.
			droppedContent++
			changed++
			continue
		}
		if messageChanged {
			changed++
		}
		out = append(out, msg)
	}

	if changed > 0 {
		req["messages"] = out
	}
	if droppedReasoning > 0 {
		notes = append(notes, "history_reasoning_dropped")
	}
	if droppedContent > 0 {
		notes = append(notes, "history_empty_assistant_dropped")
	}
	return changed, notes
}

func isEmptyContent(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(value) == ""
	case []any:
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			if typ, _ := part["type"].(string); typ == "text" {
				if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
					return false
				}
				continue
			}
			// A non-text part (for example an image) is still content.
			return false
		}
		return true
	default:
		return false
	}
}

// cleanMessageContent handles both the string form and the multimodal array
// form used by some OpenAI clients.  It returns the replacement value, the
// reasoning that was wrapped in the content field, and whether the value
// changed.
func cleanMessageContent(v any) (content any, reasoning string, changed bool) {
	switch value := v.(type) {
	case string:
		cleaned, moved := CleanContent(value)
		return cleaned, moved, cleaned != value
	case []any:
		parts := make([]any, len(value))
		copy(parts, value)
		var moved []string
		changed := false
		for i, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := part["type"].(string); typ != "text" {
				continue
			}
			text, _ := part["text"].(string)
			cleaned, reasoningPart := CleanContent(text)
			if reasoningPart != "" {
				moved = append(moved, reasoningPart)
			}
			if cleaned != text {
				// Copy the part before changing it: the caller owns the
				// request map, but mutating a caller's nested map in place is
				// a surprising side effect for a sanitizer.
				partCopy := make(map[string]any, len(part))
				for k, v := range part {
					partCopy[k] = v
				}
				partCopy["text"] = cleaned
				parts[i] = partCopy
				changed = true
			}
		}
		return parts, strings.Join(moved, "\n"), changed
	default:
		return v, "", false
	}
}
