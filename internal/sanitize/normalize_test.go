package sanitize

import (
	"strings"
	"testing"
)

func TestCleanReasoningStripsDelimitersAndNestedQuotes(t *testing.T) {
	in := " > > 先确认端口。</thinking>\n> 再看进程。\n<acp tokens=\"2\" type=\"text\">m01017</acp><control>wait</control>继续。"
	got := CleanReasoning(in)
	if strings.Contains(got, "</thinking>") || strings.Contains(got, "<acp") || strings.Contains(got, "<control>") {
		t.Fatalf("reasoning still contains transport noise: %q", got)
	}
	if !strings.Contains(got, "先确认端口。") || !strings.Contains(got, "再看进程。") || !strings.Contains(got, "继续。") {
		t.Fatalf("reasoning text was lost: %q", got)
	}
	if strings.HasPrefix(got, ">") {
		t.Fatalf("leading quote nesting was not removed: %q", got)
	}
}

func TestSplitAnalysisContentMovesCoTToReasoning(t *testing.T) {
	content, reasoning := CleanContent("<analysis>\n用户想确认端口。\n</analysis>\n\n端口 5642 正在监听。")
	if content != "端口 5642 正在监听。" {
		t.Fatalf("content = %q", content)
	}
	if reasoning != "用户想确认端口。" {
		t.Fatalf("reasoning = %q", reasoning)
	}
}

func TestUnterminatedAnalysisIsAllReasoning(t *testing.T) {
	content, reasoning := CleanContent("<analysis>\n我按时间顺序梳理一下这次对话。")
	if content != "" {
		t.Fatalf("content = %q, want empty", content)
	}
	if reasoning != "我按时间顺序梳理一下这次对话。" {
		t.Fatalf("reasoning = %q", reasoning)
	}
}

func TestStrayClosingTagIsResidue(t *testing.T) {
	content, reasoning := CleanContent("</thinking>")
	if content != "" || reasoning != "" {
		t.Fatalf("content=%q reasoning=%q, want both empty", content, reasoning)
	}
}

func TestOrphanDsmlTail(t *testing.T) {
	tail := "</｜｜DSML｜｜ parameter>\n</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	if !IsOrphanDsmlTail(tail) {
		t.Fatalf("orphan DSML tail was not recognised: %q", tail)
	}
	asciiTail := "</|DSML| parameter>\n</|DSML| invoke>\n</|DSML| calls>"
	if !IsOrphanDsmlTail(asciiTail) {
		t.Fatalf("ASCII orphan DSML tail was not recognised: %q", asciiTail)
	}
	full := "<｜｜DSML｜｜ tool_calls>\n<｜｜DSML｜｜ invoke name=\"ops_bash\">\n" +
		"<｜｜DSML｜｜ parameter name=\"command\">dir</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>"
	if IsOrphanDsmlTail(full) {
		t.Fatalf("a real DSML call must not be treated as residue")
	}
	prose, ok := DsmlResidue("</｜｜DSML｜｜ calls>\n端口 5642 正常。")
	if !ok || prose != "端口 5642 正常。" {
		t.Fatalf("prose=%q ok=%v", prose, ok)
	}
	if _, ok := DsmlResidue(full); ok {
		t.Fatalf("a real DSML invoke must not be treated as residue")
	}
}

func TestCleanAssistantHistoryDropsReasoningReplay(t *testing.T) {
	req := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{
				"role":              "assistant",
				"content":           "</thinking>",
				"reasoning_content": "> > 想一下</thinking>",
			},
			map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "调用工具",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_time", "arguments": "{}"},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "12:00"},
		},
	}
	changed, notes := CleanAssistantHistory(req)
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages kept = %d, want 3 (empty assistant turn dropped)", len(msgs))
	}
	for _, raw := range msgs {
		msg := raw.(map[string]any)
		if _, ok := msg["reasoning_content"]; ok {
			t.Fatalf("reasoning_content was replayed: %#v", msg)
		}
	}
	if len(notes) == 0 {
		t.Fatal("expected history notes")
	}
}

func TestStreamedSingularThinkTagIsDiverted(t *testing.T) {
	g := NewGuard()
	g.Push("", "<thi", nil)
	g.Push("", "nk>已找到确认按钮元素。</think>", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
	if got := g.EmittedReasoning(); got != "已找到确认按钮元素。" {
		t.Fatalf("reasoning = %q", got)
	}
}

func TestSingleChunkThinkTagSplitsAnswer(t *testing.T) {
	g := NewGuard()
	g.Push("", "<think>先看端口。</think>端口 5642 正常。", nil)
	g.Finish()
	if got := g.EmittedContent(); got != "端口 5642 正常。" {
		t.Fatalf("content = %q", got)
	}
	if got := g.EmittedReasoning(); got != "先看端口。" {
		t.Fatalf("reasoning = %q", got)
	}
}
