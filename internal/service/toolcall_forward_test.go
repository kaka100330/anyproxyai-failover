package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func toolCallDelta(index int, id, name, args string) string {
	chunk := map[string]any{
		"id": "up", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    index,
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}}},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	return "data: " + string(raw) + "\n\n"
}

func reasoningDelta(text string) string {
	chunk := map[string]any{
		"id": "up", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"reasoning_content": text},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	return "data: " + string(raw) + "\n\n"
}

func contentDelta(text string) string {
	chunk := map[string]any{
		"id": "up", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": text},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	return "data: " + string(raw) + "\n\n"
}

func finishDelta(reason string) string {
	chunk := map[string]any{
		"id": "up", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	raw, _ := json.Marshal(chunk)
	return "data: " + string(raw) + "\n\n"
}

// collectToolCalls returns the arguments the client accumulates per tool-call
// index. Two answers landing on one index is exactly the corruption a user sees
// as "Unexpected non-whitespace character after JSON".
func collectToolCalls(body string) map[int]string {
	args := map[int]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			idx := int(call["index"].(float64))
			args[idx] += fmt.Sprint(fn["arguments"])
		}
	}
	return args
}

// duplicatedReasoning is replayed verbatim as content by the bad endpoint, so
// the guard withholds it as a copy of the reasoning and the sanitizer reaches a
// "retryable" verdict with nothing but the tool call on the wire.
var duplicatedReasoning = strings.Repeat("先确认端口是否在监听，再看上游是否正常返回。", 3)

// An endpoint that delivers a tool call and then gets a retryable sanitizer
// verdict must not be failed over: the replacement endpoint would publish its
// own call on index 0 and the client would concatenate both argument strings.
func TestForwardedToolCallIsNotDuplicatedByFailover(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, false)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(toolCallDelta(0, "call_first", "ops_bash", `{"command":"ver","description":"first"}`)))
		w.Write([]byte(reasoningDelta(duplicatedReasoning)))
		w.Write([]byte(contentDelta(duplicatedReasoning)))
		w.Write([]byte(finishDelta("tool_calls")))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(first.Close)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(toolCallDelta(0, "call_second", "ops_bash", `{"command":"hostname","description":"second"}`)))
		w.Write([]byte(finishDelta("tool_calls")))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(second.Close)

	groupID, err := rs.AddFailoverGroup("tool-group", "model-tool-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	addEndpoint(t, rs, groupID, "first", first.URL)
	addEndpoint(t, rs, groupID, "second", second.URL)

	rec := httptest.NewRecorder()
	if err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-tool-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	); err != nil {
		t.Fatalf("the delivered tool call must survive, got %v", err)
	}

	args := collectToolCalls(rec.Body.String())
	if len(args) != 1 {
		t.Fatalf("client must see exactly one tool call, got %v", args)
	}
	if got := args[0]; got != `{"command":"ver","description":"first"}` {
		t.Fatalf("index 0 must carry only the first endpoint's arguments, got %q", got)
	}
	if strings.Contains(rec.Body.String(), "hostname") {
		t.Fatalf("the second endpoint must never be appended to a delivered call:\n%s", rec.Body.String())
	}
}
