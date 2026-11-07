package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openai-router-go/internal/sanitize"
)

// 中转站把错误对象塞进流里再断开：客户端不能只收到一句
// "Upstream request failed"，必须收到明确的 STREAM_TRUNCATED，并且原始
// 上游消息要保留下来供排查。
func TestUpstreamErrorFrameBecomesTruncation(t *testing.T) {
	rs, ps := newSanitizeProxy(t, true)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(truncatedStream))
		w.Write([]byte(`data: {"error":{"message":"Upstream request failed","type":"upstream_error"}}` + "\n\n"))
	}))
	t.Cleanup(broken.Close)

	if err := rs.AddRoute("errorframe", "model-errorframe", broken.URL, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add route: %v", err)
	}

	rec := httptest.NewRecorder()
	err := ps.ProxyStreamRequest(
		[]byte(`{"model":"model-errorframe","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		map[string]string{"Content-Type": "application/json"},
		rec, rec,
	)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
	if !IsStreamReported(err) {
		t.Fatal("the error frame was written by the service layer and must be marked as reported")
	}

	out := rec.Body.String()
	if strings.Count(out, `"error"`) != 1 {
		t.Fatalf("client must receive exactly one error frame, got:\n%s", out)
	}
	if !strings.Contains(out, "STREAM_TRUNCATED") {
		t.Fatalf("missing STREAM_TRUNCATED code:\n%s", out)
	}
	if !strings.Contains(out, "Upstream request failed") {
		t.Fatalf("the upstream message must be preserved as context:\n%s", out)
	}
}

// rewriter 在 requireCompletion 模式下必须吞掉上游错误帧并记录原因，
// 否则客户端会看到裸的 "Upstream request failed"。
func TestSSERewriterSwallowsUpstreamErrorWhenRequired(t *testing.T) {
	body := truncatedStream + `data: {"error":{"message":"Upstream request failed","type":"upstream_error"}}` + "\n\n"

	r := sanitize.NewSSERewriter(strings.NewReader(body))
	r.SetSilent(true)
	r.SetRequireCompletion(true)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(out), "Upstream request failed") {
		t.Fatalf("upstream error frame must not be forwarded verbatim:\n%s", string(out))
	}
	if r.UpstreamError() != "Upstream request failed" {
		t.Fatalf("upstream error not recorded: %q", r.UpstreamError())
	}
}
