package service

import (
	"encoding/json"
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
