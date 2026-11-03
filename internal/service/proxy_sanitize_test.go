package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
)

const poisonedReasoning = "I should call the tool right now, then continue with the plan."

func newSanitizeProxy(t *testing.T, enabled bool) (*RouteService, *ProxyService) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sanitize.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rs := NewRouteService(db)
	return rs, NewProxyService(rs, &config.Config{LocalAPIKey: "sk-test", SanitizeThinking: enabled})
}

// 上游第一次把思维链当正文返回（且没有 tool_calls），第二次返回正常答案。
func poisonedThenCleanUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Write([]byte(`{"id":"p1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"` + poisonedReasoning + `","content":"` + poisonedReasoning + `"},"finish_reason":"stop"}]}`))
			return
		}
		w.Write([]byte(`{"id":"p2","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestProxyRequestRepairsThinkAsContent(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)
	srv, calls := poisonedThenCleanUpstream(t)

	if err := rs.AddRoute("poison", "model-poison", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	body, status, err := ps.ProxyRequest(
		[]byte(`{"model":"model-poison","messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(body))
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response: %s", string(body))
	}
	if got := resp.Choices[0].Message.Content; got != "done" {
		t.Fatalf("client must receive the repaired answer, got %q", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("poisoned response must be retried exactly once, upstream calls=%d", calls.Load())
	}
}

// 关闭净化时必须完全保持旧行为：不重试、不改写。
func TestProxyRequestSanitizeDisabledKeepsLegacyBehaviour(t *testing.T) {
	rs, ps := newSanitizeProxy(t, false)
	srv, calls := poisonedThenCleanUpstream(t)

	if err := rs.AddRoute("poison", "model-poison", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	body, status, err := ps.ProxyRequest(
		[]byte(`{"model":"model-poison","messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	if calls.Load() != 1 {
		t.Fatalf("sanitize disabled must not retry, upstream calls=%d", calls.Load())
	}
	if !strings.Contains(string(body), poisonedReasoning) {
		t.Fatalf("sanitize disabled must pass the body through untouched, got %s", string(body))
	}
}

// 流式：第一轮只有"正文=思维链"，必须重试并让客户端拿到干净答案。
func TestProxyStreamRepairsThinkAsContent(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
			w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
			w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
			w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Write([]byte("data: {\"id\":\"s2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("poison", "model-poison", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-poison","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, poisonedReasoning) {
		t.Fatalf("client must never receive the reasoning copy as content, got:\n%s", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("client must receive the repaired answer, got:\n%s", out)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) == false {
		t.Fatalf("stream must still terminate with finish_reason=stop, got:\n%s", out)
	}
	if calls.Load() != 2 {
		t.Fatalf("poisoned stream must be retried exactly once, upstream calls=%d", calls.Load())
	}
}

// 两条路径都持续中毒时必须响亮失败，不能把思维链当答案发出去。
func TestProxyStreamAllAttemptsPoisonedReturnsError(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("poison", "model-poison", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-poison","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if err == nil {
		t.Fatal("expected a loud failure when every attempt is poisoned")
	}
	out := rec.Body.String()
	if !strings.Contains(out, "THINK_AS_CONTENT") {
		t.Fatalf("client should be told the response was rejected, got:\n%s", out)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("a poisoned stream must never be closed as a normal answer, got:\n%s", out)
	}
}

// 单条端点被 sanitizer 判定为不可用时，必须换到下一条端点，而不是在同一条上反复重试。
func TestProxyStreamSanitizerFailoverToNextEndpoint(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	var badCalls, goodCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + poisonedReasoning + "\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(bad.Close)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"s2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"s2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(good.Close)

	groupID, err := rs.AddFailoverGroup("sanitize-group", "model-sanitize-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	addEndpoint(t, rs, groupID, "bad", bad.URL)
	addEndpoint(t, rs, groupID, "good", good.URL)

	rec := httptest.NewRecorder()
	err = ps.ProxyStreamRequest(
		[]byte(`{"model":"model-sanitize-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if err != nil {
		t.Fatalf("stream request should fail over and succeed: %v", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, poisonedReasoning) {
		t.Fatalf("poisoned content leaked: %s", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("second endpoint answer missing: %s", out)
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("bad=%d good=%d, want one call each", badCalls.Load(), goodCalls.Load())
	}
}

// 复现原会话最后一次中断：上游只返回 DSML 闭合残片。必须报 EMPTY_UPSTREAM，
// 不能再用 THINK_AS_CONTENT 把「端点坏了」误报成「模型把思考当正文」。
func TestProxyStreamOrphanDsmlTailReturnsEmptyUpstream(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	chunks := []string{"  ", "</｜｜DSML｜", "｜ para", "me", "te", "r>\n</｜｜DSML", "｜｜ ", "i", "nvoke>\n</｜｜DSML｜｜ calls>"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			payload, _ := json.Marshal(map[string]any{
				"id": "s1", "object": "chat.completion.chunk", "created": 1, "model": "m",
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"reasoning_content": c}, "finish_reason": nil,
				}},
			})
			w.Write([]byte("data: " + string(payload) + "\n\n"))
		}
		w.Write([]byte("data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("orphan", "model-orphan", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-orphan","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, errEmptyUpstream) {
		t.Fatalf("err = %v, want errEmptyUpstream", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, "THINK_AS_CONTENT") {
		t.Fatalf("orphan DSML tail must not be reported as think-as-content: %s", out)
	}
	if !strings.Contains(out, "EMPTY_UPSTREAM") {
		t.Fatalf("client should see EMPTY_UPSTREAM, got: %s", out)
	}
}

// 请求侧修复：上游不应再看到客户端回放的 reasoning_content 或正文里的 <analysis>。
func TestProxyRequestCleansAssistantHistory(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ok","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("clean-history", "model-clean-history", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	body := `{"model":"model-clean-history","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"<analysis>内部推理</analysis>最终答复","reasoning_content":"> > 内部推理</thinking>"},
		{"role":"user","content":"continue"}
	]}`
	if _, _, err := ps.ProxyRequest([]byte(body), map[string]string{"Content-Type": "application/json"}); err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	sent, _ := gotBody.Load().(string)
	if strings.Contains(sent, "reasoning_content") {
		t.Fatalf("upstream request still contains reasoning_content: %s", sent)
	}
	if strings.Contains(sent, "<analysis>") || strings.Contains(sent, "</thinking>") {
		t.Fatalf("upstream request still contains thinking delimiters: %s", sent)
	}
	if !strings.Contains(sent, "最终答复") {
		t.Fatalf("normal answer text was lost: %s", sent)
	}
}

// 非流式路径同样要在 sanitizer 判定失败时换端点。
func TestProxyRequestSanitizerFailoverToNextEndpoint(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	var badCalls, goodCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"p1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"` + poisonedReasoning + `","content":"` + poisonedReasoning + `"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"p2","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(good.Close)

	groupID, err := rs.AddFailoverGroup("sanitize-request-group", "model-sanitize-request-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	addEndpoint(t, rs, groupID, "bad", bad.URL)
	addEndpoint(t, rs, groupID, "good", good.URL)

	body, status, err := ps.ProxyRequest(
		[]byte(`{"model":"model-sanitize-request-group","messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		t.Fatalf("request should fail over and succeed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(body), "done") {
		t.Fatalf("second endpoint answer missing: %s", body)
	}
	if badCalls.Load() != 1 || goodCalls.Load() != 1 {
		t.Fatalf("bad=%d good=%d, want one call each", badCalls.Load(), goodCalls.Load())
	}
}

// 流式已经下发了部分正文后才发现畸形 DSML：不能重试（会重复正文），必须
// 回一个 SANITIZE_PARTIAL 错误帧。
func TestProxyStreamPartialContentReturnsSanitizePartial(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{"部分答案。", "<｜｜DSML｜｜ invoke>"} {
			payload, _ := json.Marshal(map[string]any{
				"id": "s1", "object": "chat.completion.chunk", "created": 1, "model": "m",
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": c}, "finish_reason": nil,
				}},
			})
			w.Write([]byte("data: " + string(payload) + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("partial", "model-partial", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-partial","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, errSanitizePartial) {
		t.Fatalf("err = %v, want errSanitizePartial", err)
	}
	if !strings.Contains(rec.Body.String(), "SANITIZE_PARTIAL") {
		t.Fatalf("client should see SANITIZE_PARTIAL, got: %s", rec.Body.String())
	}
}
