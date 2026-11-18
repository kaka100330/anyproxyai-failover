package sanitize

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// nativeToolChunk builds an upstream chunk that carries a real tool_calls delta
// with the given slot index.
func nativeToolChunk(index int, id, name, args string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    float64(index),
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}}},
			"finish_reason": nil,
		}},
	}
}

func toolCallsSSE(native []map[string]any, content string) string {
	var b strings.Builder
	for _, c := range native {
		raw, _ := json.Marshal(c)
		b.WriteString("data: " + string(raw) + "\n\n")
	}
	for _, piece := range splitRunes(content, 12) {
		raw, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"content": piece},
				"finish_reason": nil,
			}},
		})
		b.WriteString("data: " + string(raw) + "\n\n")
	}
	fin, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	})
	b.WriteString("data: " + string(fin) + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// toolCallIndices returns the index of every tool_calls delta the client sees.
func toolCallIndices(out string) []int {
	var idx []int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || strings.Contains(line, "[DONE]") {
			continue
		}
		var ch map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ch) != nil {
			continue
		}
		choices, _ := ch["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, c := range calls {
			if m, ok := c.(map[string]any); ok {
				if v, ok := m["index"].(float64); ok {
					idx = append(idx, int(v))
				}
			}
		}
	}
	return idx
}

// A model that streams its call natively and then repeats the same call as DSML
// text must not have that text forwarded as a second call: the client
// concatenates arguments per index, which turns two objects into one
// unparsable string ("{...}{...}").
func TestTextualCopyOfNativeToolCallIsSuppressed(t *testing.T) {
	body := toolCallsSSE(
		[]map[string]any{nativeToolChunk(0, "call_a", "ops_bash", `{"command":"ver"}`)},
		dsmlCall,
	)
	rw := NewSSERewriter(strings.NewReader(body))
	out, _ := io.ReadAll(rw)

	if idx := toolCallIndices(string(out)); len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("client must see exactly one tool call at index 0, got %v\n%s", idx, string(out))
	}
	if !strings.Contains(strings.Join(rw.Notes(), ","), NoteDsmlTextSuppressed) {
		t.Fatalf("the suppressed copy must be recorded in the notes, got %v", rw.Notes())
	}
	if rw.Poisoned() {
		t.Fatal("suppressing a duplicate textual call is a repair, not a poisoned stream")
	}
}

// When the textual block carries more calls than the native stream did, the
// surplus calls are kept but placed after the native slots instead of reusing
// index 0.
func TestRecoveredToolCallGetsFreeIndex(t *testing.T) {
	body := toolCallsSSE(
		[]map[string]any{nativeToolChunk(0, "call_a", "ops_bash", `{"command":"ver"}`)},
		dsmlCall+dsmlCall,
	)
	rw := NewSSERewriter(strings.NewReader(body))
	out, _ := io.ReadAll(rw)

	idx := toolCallIndices(string(out))
	seen := map[int]bool{}
	for _, i := range idx {
		seen[i] = true
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("native call keeps index 0 and the recovered call takes index 1, got %v", idx)
	}
	if len(idx) != 2 {
		t.Fatalf("expected exactly two tool calls, got %v", idx)
	}
}
