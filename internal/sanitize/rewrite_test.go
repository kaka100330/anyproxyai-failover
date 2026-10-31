package sanitize

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func sseFrame(t *testing.T, delta map[string]any, finish any) string {
	t.Helper()
	chunk := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   "deepseek-v4.1-flash",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return "data: " + string(b) + "\n\n"
}

func buildSSE(t *testing.T, deltas []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, d := range deltas {
		b.WriteString(sseFrame(t, d, nil))
	}
	b.WriteString(sseFrame(t, map[string]any{}, "stop"))
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// collectContent extracts assistant content from a sanitized SSE stream.
func collectContent(t *testing.T, stream string) (content, reasoning string, errCode string) {
	t.Helper()
	var cb, rb strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad json in stream: %v (%s)", err, payload)
		}
		if e, ok := chunk["error"].(map[string]any); ok {
			errCode, _ = e["code"].(string)
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			cb.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			rb.WriteString(s)
		}
	}
	return cb.String(), rb.String(), errCode
}

func TestSSERewriterPassesCleanAnswer(t *testing.T) {
	deltas := []map[string]any{
		{"role": "assistant"},
		{"reasoning_content": "先确认端口占用。"},
		{"content": "端口 5642 正在监听。"},
		{"content": "进程是 anyproxyai。"},
	}
	out, err := io.ReadAll(NewSSERewriter(strings.NewReader(buildSSE(t, deltas))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content, reasoning, code := collectContent(t, string(out))
	if code != "" {
		t.Fatalf("unexpected error frame: %s", code)
	}
	if content != "端口 5642 正在监听。进程是 anyproxyai。" {
		t.Fatalf("content = %q", content)
	}
	if reasoning != "先确认端口占用。" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Fatal("stream must end with [DONE]")
	}
}

func TestSSERewriterRejectsReasoningAsContent(t *testing.T) {
	deltas := []map[string]any{{"role": "assistant"}}
	for _, part := range splitRunes(testReasoning, 40) {
		deltas = append(deltas, map[string]any{"reasoning_content": part})
	}
	// The upstream then re-sends the whole reasoning as one content delta.
	deltas = append(deltas, map[string]any{"content": testReasoning})

	out, err := io.ReadAll(NewSSERewriter(strings.NewReader(buildSSE(t, deltas))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content, _, code := collectContent(t, string(out))
	if code != "THINK_AS_CONTENT" {
		t.Fatalf("error code = %q, want THINK_AS_CONTENT", code)
	}
	if content != "" {
		t.Fatalf("content leaked to client: %q", content)
	}
	if strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Fatal("a poisoned stream must not be closed with finish_reason=stop")
	}
}

func TestSSERewriterDivertsThinkingTag(t *testing.T) {
	deltas := []map[string]any{
		{"content": "<thin"},
		{"content": "king>推理一"},
		{"content": "段</thinking>"},
		{"content": "结论在这里。"},
	}
	out, err := io.ReadAll(NewSSERewriter(strings.NewReader(buildSSE(t, deltas))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content, reasoning, code := collectContent(t, string(out))
	if code != "" {
		t.Fatalf("unexpected error frame: %s", code)
	}
	if content != "结论在这里。" {
		t.Fatalf("content = %q", content)
	}
	if reasoning != "推理一段" {
		t.Fatalf("reasoning = %q", reasoning)
	}
}

func TestSSERewriterKeepsUsageChunk(t *testing.T) {
	var b strings.Builder
	b.WriteString(sseFrame(t, map[string]any{"content": "好的。"}, nil))
	b.WriteString(sseFrame(t, map[string]any{}, "stop"))
	b.WriteString(`data: {"id":"x","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	out, err := io.ReadAll(NewSSERewriter(strings.NewReader(b.String())))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), `"total_tokens":5`) {
		t.Fatal("usage chunk must be forwarded")
	}
}
