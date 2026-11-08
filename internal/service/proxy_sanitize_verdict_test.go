package service

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
	"openai-router-go/internal/sanitize"
)

// newSanitizeProxyOpts builds a proxy with the two sanitizer switches set
// explicitly, so the tests can cover the "restore as a last resort" path.
func newSanitizeProxyOpts(t *testing.T, thinking, restore bool) (*RouteService, *ProxyService) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "verdict.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rs := NewRouteService(db)
	return rs, NewProxyService(rs, &config.Config{
		LocalAPIKey:              "sk-test",
		SanitizeThinking:         thinking,
		SanitizeDuplicateRestore: restore,
	})
}

// duplicateSSE is the upstream defect that actually broke the session: the
// endpoint streams its chain of thought and then replays it verbatim as the
// assistant answer.
func duplicateSSE(text string) string {
	return "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"" + text + "\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + text + "\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// reasoningOnlySSE is the second defect: the endpoint spends the whole
// completion budget on reasoning and never starts the answer.
func reasoningOnlySSE(text string) string {
	return "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"" + text + "\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
}

// 判定顺序就是这次事故的根子：断流不能被贴上"思维链当正文"的标签，
// 只有 DSML 残渣的响应也不能说成"思考烧光预算"。
func TestClassifySanitizeFailureOrder(t *testing.T) {
	broken := streamOutcome{Terminated: false}
	cases := []struct {
		name       string
		notes      []string
		content    string
		reasoning  string
		outcome    streamOutcome
		wantErr    error
		wantSubmit bool
	}{
		{"partial content wins over everything", []string{sanitize.NoteDuplicate}, "hi", "hi", broken, errSanitizePartial, false},
		{"truncated stream beats duplicate", []string{sanitize.NoteDuplicate, sanitize.NoteEmptyAfter}, "", "hi", broken, ErrStreamTruncated, false},
		{"duplicate", []string{sanitize.NoteDuplicate, sanitize.NoteEmptyAfter}, "", "hi", streamOutcome{Terminated: true}, errThinkAsContent, false},
		{"dsml unparsed", []string{sanitize.NoteDsmlUnparsed}, "", "", streamOutcome{Terminated: true}, errDsmlUnparsed, false},
		{"orphan dsml tail is empty, not thinking", []string{sanitize.NoteOrphanDsml, sanitize.NoteEmptyAfter}, "", "</｜｜DSML｜｜ invoke>", streamOutcome{Terminated: true}, errEmptyUpstream, false},
		{"reasoning only", []string{sanitize.NoteEmptyAfter}, "", "thinking...", streamOutcome{Terminated: true}, errTruncatedThinking, false},
		{"nothing at all", []string{sanitize.NoteEmptyAfter}, "", "", streamOutcome{Terminated: true}, errEmptyUpstream, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySanitizeFailure(tc.notes, tc.content, tc.reasoning, tc.outcome)
			if !errors.Is(got, tc.wantErr) {
				t.Fatalf("classified as %v, want %v", got, tc.wantErr)
			}
		})
	}
}

// 只出推理、正文一个字没有：必须报 TRUNCATED_THINKING，不能含糊成 think-as-content。
func TestProxyStreamReasoningOnlyIsTruncatedThinking(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(reasoningOnlySSE("let me think about the plan")))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("thinking", "model-thinking", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-thinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, errTruncatedThinking) {
		t.Fatalf("err = %v, want errTruncatedThinking", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "TRUNCATED_THINKING") {
		t.Fatalf("client should see TRUNCATED_THINKING, got:\n%s", out)
	}
	if strings.Contains(out, "THINK_AS_CONTENT") {
		t.Fatalf("a reasoning-only stream must not be reported as think-as-content, got:\n%s", out)
	}
}

// 思考烧光预算的端点后面还有好端点：换过去，客户端拿到完整答案。
func TestTruncatedThinkingFailsOverToNextEndpoint(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, false)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(reasoningOnlySSE("thinking forever")))
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"g\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"g\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(good.Close)

	groupID, err := rs.AddFailoverGroup("thinking-group", "model-thinking-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	badID := addEndpoint(t, rs, groupID, "bad", bad.URL)
	addEndpoint(t, rs, groupID, "good", good.URL)

	rec := httptest.NewRecorder()
	if err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-thinking-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	); err != nil {
		t.Fatalf("stream request should be served by the second endpoint, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "done") {
		t.Fatalf("client should receive the answer from the healthy endpoint, got:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("stream must end with finish_reason=stop, got:\n%s", out)
	}
	group, err := rs.GetFailoverGroupByID(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	status := ps.FailoverStatus(groupID, 600, group.Strategy)
	if !coolingOf(status, badID) {
		t.Fatal("the endpoint that burned its budget on reasoning must be cooled down")
	}
}

// 整组端点都只会复制思维链：宁可把重复内容交出去，也不要中断整个回合。
func TestDuplicateRestoreIsLastResort(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(duplicateSSE(poisonedReasoning)))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("dup", "model-dup", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	if err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-dup","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	); err != nil {
		t.Fatalf("with restore enabled the turn must survive, got %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, poisonedReasoning) {
		t.Fatalf("the withheld content should be restored as the answer, got:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("restored response must be a complete stream, got:\n%s", out)
	}
	if strings.Contains(out, "error") {
		t.Fatalf("restored response must not carry an error frame, got:\n%s", out)
	}

	var success bool
	if err := rs.db.QueryRow(`SELECT success FROM request_logs ORDER BY id DESC LIMIT 1`).Scan(&success); err != nil {
		t.Fatalf("read request log: %v", err)
	}
	if !success {
		t.Fatal("a restored response is a served request and must be logged as success")
	}
}

// 关掉兜底开关就必须保持"响亮失败"的老行为。
func TestDuplicateRestoreDisabledKeepsLoudFailure(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(duplicateSSE(poisonedReasoning)))
	}))
	t.Cleanup(srv.Close)

	if err := rs.AddRoute("dup", "model-dup", srv.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-dup","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, errThinkAsContent) {
		t.Fatalf("err = %v, want errThinkAsContent", err)
	}
	if strings.Contains(rec.Body.String(), poisonedReasoning) {
		t.Fatalf("the reasoning copy must never reach the client, got:\n%s", rec.Body.String())
	}
}

// 上一轮的净化器判定不能串到这一轮的传输故障上：客户端看到的错误必须
// 描述本次真正发生的事，而不是一个已经过去了的 think-as-content。
func TestStaleSanitizeVerdictDoesNotLeakIntoNextRound(t *testing.T) {
	rs, ps := newSanitizeProxyOpts(t, true, false)

	dup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(duplicateSSE(poisonedReasoning)))
	}))
	t.Cleanup(dup.Close)

	// 第二个端点连响应都不给就断开：这是一个纯粹的传输故障。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, err := hijacker.Hijack()
			if err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					tcp.SetLinger(0)
				}
				conn.Close()
				return
			}
		}
		hj, _, _ := w.(http.Hijacker).Hijack()
		hj.Close()
	}))
	t.Cleanup(dead.Close)

	groupID, err := rs.AddFailoverGroup("stale-group", "model-stale-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	addEndpoint(t, rs, groupID, "dup", dup.URL)
	addEndpoint(t, rs, groupID, "dead", dead.URL)

	rec := httptest.NewRecorder()
	err = ps.ProxyStreamRequest(
		[]byte(`{"model":"model-stale-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if err == nil {
		t.Fatalf("both endpoints failed, expected an error")
	}
	if errors.Is(err, errThinkAsContent) {
		t.Fatalf("the transport failure was reported as the previous round's think-as-content: %v", err)
	}
	if strings.Contains(rec.Body.String(), "THINK_AS_CONTENT") {
		t.Fatalf("client must not be told think-as-content when this round only hit a dead endpoint:\n%s", rec.Body.String())
	}
}
