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

func TestDsmlTailWithProseKeepsProse(t *testing.T) {
	g := NewGuard()
	g.Push("</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ calls>\n端口 5642 正常。", "", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "端口 5642 正常。" {
		t.Fatalf("content = %q", got)
	}
	if g.Retryable() {
		t.Fatalf("residue must not poison the stream: %v", g.Notes())
	}
	if !strings.Contains(strings.Join(g.Notes(), ","), NoteOrphanDsml) {
		t.Fatalf("notes = %v", g.Notes())
	}
}

// 线上真实形态：上游先把一大段思考流出来，正文开头复述了思考的前几个词，
// 随后继续往下写真正的答案。复述之后的分叉恰好证明它不是副本——旧代码在
// 这里判成 content_duplicates_reasoning，把已经发出去的正文又贴上一个失败
// 标签，客户端于是先收到答案、再收到 SANITIZE_PARTIAL。
func TestStreamedPrefixEchoThenAnswerIsNotADuplicate(t *testing.T) {
	reasoning := "用户让我确认 5642 端口是不是在监听。先看 netstat，再用 PowerShell 的 " +
		"Get-NetTCPConnection 交叉验证，然后把结果和 PID 一起报给他。"
	prefix := strings.TrimRight(string([]rune(reasoning)[:24]), " ")
	answer := "5642 端口确实在监听，进程 PID 是 6352，路径 D:\\anyproxyai\\anyproxyai-windows-amd64.exe。"

	var chunks []map[string]any
	for _, part := range splitRunes(reasoning, 9) {
		chunks = append(chunks, rchunk(part))
	}
	// 正文先复述思考开头，再分叉成真实答案。
	for _, part := range splitRunes(prefix+" 结论是：", 5) {
		chunks = append(chunks, chunk(part))
	}
	for _, part := range splitRunes(answer, 6) {
		chunks = append(chunks, chunk(part))
	}

	events, g := streamOf(chunks)
	want := prefix + " 结论是：" + answer
	if got := textOf(events, EventContent); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	notes := strings.Join(g.Notes(), ",")
	if strings.Contains(notes, NoteDuplicate) {
		t.Fatalf("an answer that only echoed the reasoning prefix must not be a duplicate copy: %v", g.Notes())
	}
	if !strings.Contains(notes, NotePrefixEcho) {
		t.Fatalf("notes = %v, want %s", g.Notes(), NotePrefixEcho)
	}
	if g.Retryable() {
		t.Fatalf("an echoed prefix is not a defect, the response must not be retried: %v", g.Notes())
	}
}

// 前缀复述只用掉一次怀疑额度：后面正文再怎么和思考撞前缀，也不该被重新
// 当成副本，否则同一段回答会被反复扣住。
func TestStreamedEchoDoesNotRearmDuplicateCheck(t *testing.T) {
	reasoning := strings.Repeat("先检查端口，再检查路由表，最后核对 failover 组。", 3)
	prefix := string([]rune(reasoning)[:30])

	var chunks []map[string]any
	for _, part := range splitRunes(reasoning, 11) {
		chunks = append(chunks, rchunk(part))
	}
	for _, part := range splitRunes(prefix+" 答案是 5642。", 4) {
		chunks = append(chunks, chunk(part))
	}
	// 第二段正文再次以思考的前缀开头，但依然继续往下写。
	for _, part := range splitRunes(prefix+" 补充：PID 6352。", 4) {
		chunks = append(chunks, chunk(part))
	}

	events, g := streamOf(chunks)
	want := prefix + " 答案是 5642。" + prefix + " 补充：PID 6352。"
	if got := textOf(events, EventContent); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if notes := strings.Join(g.Notes(), ","); strings.Contains(notes, NoteDuplicate) {
		t.Fatalf("notes = %v, the echoed prefix must not be re-judged as a copy", g.Notes())
	}
	if g.Retryable() {
		t.Fatalf("notes = %v, want a plain answer", g.Notes())
	}
}

// 反面：正文从头到尾都是思考的副本，仍然必须扣住并允许换端点，不能因为
// 放宽"前缀复述"而把真正的 think-as-content 放过去。
func TestStreamedFullCopyStillWithheld(t *testing.T) {
	var chunks []map[string]any
	for _, part := range splitRunes(testReasoning, 13) {
		chunks = append(chunks, rchunk(part))
	}
	chunks = append(chunks, chunk(testReasoning))

	events, g := streamOf(chunks)
	if got := textOf(events, EventContent); got != "" {
		t.Fatalf("content leaked: %q", got)
	}
	if !strings.Contains(strings.Join(g.Notes(), ","), NoteDuplicate) {
		t.Fatalf("notes = %v, want %s", g.Notes(), NoteDuplicate)
	}
	if !g.Retryable() {
		t.Fatalf("notes = %v, want retryable", g.Notes())
	}
}
