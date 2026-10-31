package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

var testReasoning = strings.Repeat("用户想让我检查端口是否监听。先看 netstat，再用 PowerShell 的 Get-NetTCPConnection。", 6)

func textOf(events []Event, kind EventKind) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == kind {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func streamOf(chunks []map[string]any) ([]Event, *Guard) {
	g := NewGuard()
	var events []Event
	for _, c := range chunks {
		events = append(events, g.Push(asString(c["reasoning_content"]), asString(c["content"]), nil)...)
	}
	events = append(events, g.Finish()...)
	return events, g
}

// splitRunes cuts s into chunks of n runes, so the pieces stay valid UTF-8.
func splitRunes(s string, n int) []string {
	r := []rune(s)
	out := make([]string, 0, len(r)/n+1)
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func chunk(content string) map[string]any { return map[string]any{"content": content} }

func rchunk(reasoning string) map[string]any { return map[string]any{"reasoning_content": reasoning} }

func TestDuplicateContentIsDropped(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":              "assistant",
				"content":           testReasoning,
				"reasoning_content": testReasoning,
			},
			"finish_reason": "stop",
		}},
	})
	out, notes, poison := ApplyToBody(body)
	if !poison {
		t.Fatal("a copy of the reasoning must be reported as poison")
	}
	if !strings.Contains(strings.Join(notes, ","), NoteDuplicate) {
		t.Fatalf("notes = %v, want %s", notes, NoteDuplicate)
	}

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	msg := root["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "" {
		t.Fatalf("content = %q, want empty", msg["content"])
	}
	if msg["reasoning_content"] != testReasoning {
		t.Fatal("reasoning must be preserved")
	}
}

func TestThinkingTagMovesToReasoning(t *testing.T) {
	g := NewGuard()
	g.Push("", "<thinking>先看端口。</thinking>端口 5642 在听。", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "端口 5642 在听。" {
		t.Fatalf("content = %q", got)
	}
	if got := g.EmittedReasoning(); got != "先看端口。" {
		t.Fatalf("reasoning = %q", got)
	}
}

func TestUnterminatedThinkingTag(t *testing.T) {
	g := NewGuard()
	g.Push("", "<thinkingNow update the conversion calls and the converter.", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
	if got := g.EmittedReasoning(); got != "Now update the conversion calls and the converter." {
		t.Fatalf("reasoning = %q", got)
	}
}

func TestNormalAnswerUntouched(t *testing.T) {
	g := NewGuard()
	g.Push("先确认端口。", "端口 5642 正在监听，进程是 anyproxyai。", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "端口 5642 正在监听，进程是 anyproxyai。" {
		t.Fatalf("content = %q", got)
	}
	if got := g.EmittedReasoning(); got != "先确认端口。" {
		t.Fatalf("reasoning = %q", got)
	}
	if notes := g.Notes(); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
}

const dsmlCall = "\n<｜｜DSML｜｜ tool_calls>\n<｜｜DSML｜｜ invoke name=\"ops_bash\">\n" +
	"<｜｜DSML｜｜ parameter name=\"command\">netstat -ano | findstr 5642</｜｜DSML｜｜ parameter>\n" +
	"</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"

func TestDsmlInContentBecomesToolCall(t *testing.T) {
	g := NewGuard()
	g.Push("", "好的，我来执行。"+dsmlCall, nil)
	g.Finish()
	if got := g.EmittedContent(); got != "好的，我来执行。\n" {
		t.Fatalf("content = %q", got)
	}
	calls := g.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "ops_bash" {
		t.Fatalf("calls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments not JSON: %v", err)
	}
	if args["command"] != "netstat -ano | findstr 5642" {
		t.Fatalf("arguments = %v", args)
	}
}

func TestDsmlInReasoningRecoveredAndDuplicateDropped(t *testing.T) {
	r := testReasoning + dsmlCall
	g := NewGuard()
	g.Push(r, r, nil)
	g.Finish()
	if got := g.EmittedContent(); got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
	if len(g.ToolCalls()) != 1 {
		t.Fatalf("calls = %+v", g.ToolCalls())
	}
	if strings.Contains(g.EmittedReasoning(), "DSML") {
		t.Fatal("reasoning must not contain the DSML block")
	}
	if !strings.HasPrefix(g.EmittedReasoning(), testReasoning[:40]) {
		t.Fatal("reasoning prefix lost")
	}
	if !strings.Contains(strings.Join(g.Notes(), ","), NoteDsmlRecovered) {
		t.Fatalf("notes = %v", g.Notes())
	}
}

func TestStreamedDuplicateNeverEmitted(t *testing.T) {
	var chunks []map[string]any
	for _, part := range splitRunes(testReasoning, 17) {
		chunks = append(chunks, rchunk(part))
	}
	chunks = append(chunks, chunk(testReasoning))

	events, g := streamOf(chunks)
	if got := textOf(events, EventContent); got != "" {
		t.Fatalf("content leaked: %q", got)
	}
	if !g.Retryable() {
		t.Fatalf("notes = %v, want retryable", g.Notes())
	}
}

func TestStreamedNormalAnswerIntact(t *testing.T) {
	answer := "端口 5642 正在监听。进程 PID 是 2244，路径是 anyproxyai.exe。"
	var chunks []map[string]any
	for _, part := range splitRunes(testReasoning, 7) {
		chunks = append(chunks, rchunk(part))
	}
	for _, part := range splitRunes(answer, 5) {
		chunks = append(chunks, chunk(part))
	}
	events, g := streamOf(chunks)
	if got := textOf(events, EventContent); got != answer {
		t.Fatalf("content = %q, want %q", got, answer)
	}
	if got := textOf(events, EventReasoning); got != testReasoning {
		t.Fatalf("reasoning = %q", got)
	}
	if notes := g.Notes(); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
}

func TestStreamedThinkingTagDiverted(t *testing.T) {
	events, _ := streamOf([]map[string]any{
		chunk("<thin"), chunk("king>推理一"), chunk("段</thinking>"), chunk("结论在这里。"),
	})
	if got := textOf(events, EventContent); got != "结论在这里。" {
		t.Fatalf("content = %q", got)
	}
	if got := textOf(events, EventReasoning); got != "推理一段" {
		t.Fatalf("reasoning = %q", got)
	}
}

func TestPartialMarkerAtTailNotLeaked(t *testing.T) {
	g := NewGuard()
	g.Push("", "准备调用<｜｜DSML｜｜ invoke", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "准备调用" {
		t.Fatalf("content = %q", got)
	}
}

func TestParseDsmlMultipleInvokes(t *testing.T) {
	body := "<｜｜DSML｜｜ tool_calls>\n<｜｜DSML｜｜ invoke name=\"a\"><｜｜DSML｜｜ parameter name=\"x\">1</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>\n" +
		"<｜｜DSML｜｜ invoke name=\"b\"><｜｜DSML｜｜ parameter name=\"y\">{\"k\":2}</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	calls, ok := parseDsml(body)
	if !ok || len(calls) != 2 {
		t.Fatalf("calls = %+v ok=%v", calls, ok)
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(calls[1].Arguments), &b); err != nil {
		t.Fatal(err)
	}
	if a["x"] != float64(1) {
		t.Fatalf("a = %v", a)
	}
	if m, _ := b["y"].(map[string]any); m["k"] != float64(2) {
		t.Fatalf("b = %v", b)
	}
}
