package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 中转站在长流中途掐断：没有 [DONE]，没有 finish_reason，只有半截 tool_call。
const truncatedStream = "data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"先看端口。\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"s1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"ops_bash\",\"arguments\":\"{\\\"command\\\":\\\"ss\"}}]},\"finish_reason\":null}]}\n\n"

func TestInspectUpstreamStreamCompletionMarkers(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		terminated bool
		upstream   string
	}{
		{"openai done", truncatedStream + "data: {\"id\":\"s1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true, ""},
		{"openai finish only", truncatedStream + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", true, ""},
		{"openai usage only", truncatedStream + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n\n", true, ""},
		{"claude message_stop", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, ""},
		{"gemini finishReason", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}\n\n", true, ""},
		{"truncated", truncatedStream, false, ""},
		{"upstream error frame", truncatedStream + "data: {\"error\":{\"message\":\"Upstream request failed\",\"type\":\"upstream_error\"}}\n\n", false, "Upstream request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := inspectUpstreamStream(tc.body)
			if out.Terminated != tc.terminated {
				t.Fatalf("terminated = %v, want %v", out.Terminated, tc.terminated)
			}
			if out.UpstreamErr != tc.upstream {
				t.Fatalf("upstream error = %q, want %q", out.UpstreamErr, tc.upstream)
			}
		})
	}
}

// 断流必须被判为失败：既不能记成功，也不能把半截答案当成完整答案。
func TestProxyStreamTruncatedIsReportedAndCoolsEndpoint(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(truncatedStream))
		// 直接断开，不发 finish_reason，不发 [DONE]
	}))
	t.Cleanup(broken.Close)

	if err := rs.AddRoute("broken", "model-truncated", broken.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-truncated","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "STREAM_TRUNCATED") {
		t.Fatalf("client should be told the stream was truncated, got:\n%s", out)
	}
	if strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("a truncated stream must never be closed as a normal answer, got:\n%s", out)
	}

	// 请求日志必须记失败，面板不能再显示绿色。
	var success bool
	var errMsg string
	if err := rs.db.QueryRow(`SELECT success, COALESCE(error_message, '') FROM request_logs ORDER BY id DESC LIMIT 1`).Scan(&success, &errMsg); err != nil {
		t.Fatalf("read request log: %v", err)
	}
	if success {
		t.Fatal("a truncated stream must be logged as a failure, not as a healthy request")
	}
	if !strings.Contains(errMsg, "completion marker") {
		t.Fatalf("error message should explain the truncation, got %q", errMsg)
	}
}

// 分组里第一条端点断流：端点进入冷却，下一条请求直接从第二条端点开始。
func TestTruncatedStreamCoolsEndpointInGroup(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(truncatedStream))
	}))
	t.Cleanup(broken.Close)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(good.Close)

	groupID, err := rs.AddFailoverGroup("truncate-group", "model-truncate-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	badID := addEndpoint(t, rs, groupID, "broken", broken.URL)
	addEndpoint(t, rs, groupID, "good", good.URL)

	group, err := rs.GetFailoverGroupByID(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}

	rec := httptest.NewRecorder()
	err = ps.ProxyStreamRequest(
		[]byte(`{"model":"model-truncate-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("first request should report truncation, got %v", err)
	}

	status := ps.FailoverStatus(groupID, 600, group.Strategy)
	if !coolingOf(status, badID) {
		t.Fatal("the endpoint that truncated the stream must be put in cooldown")
	}

	res, err := ps.resolveGroupTargets(group)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].ID == badID {
		t.Fatalf("next request must skip the cooling endpoint, got %v", ids(res.Targets))
	}
}

// 端点连响应体都没发就断开（0 字节转发）：此时换端点重试是安全的。
func TestTruncatedBeforeAnyByteFailsOver(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	var emptyCalls atomic.Int32
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// 立即结束，什么都不写
	}))
	t.Cleanup(empty.Close)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"served\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(good.Close)

	groupID, err := rs.AddFailoverGroup("empty-group", "model-empty-group", 600, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	addEndpoint(t, rs, groupID, "empty", empty.URL)
	addEndpoint(t, rs, groupID, "good", good.URL)

	rec := httptest.NewRecorder()
	err = ps.ProxyStreamRequest(
		[]byte(`{"model":"model-empty-group","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if err != nil {
		t.Fatalf("stream should fail over to the healthy endpoint: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "served") {
		t.Fatalf("healthy endpoint answer missing: %s", rec.Body.String())
	}
	if emptyCalls.Load() != 1 {
		t.Fatalf("empty endpoint calls = %d, want 1", emptyCalls.Load())
	}
}
