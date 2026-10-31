package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
)

func newTestProxy(t *testing.T) (*RouteService, *ProxyService) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rs := NewRouteService(db)
	return rs, NewProxyService(rs, &config.Config{LocalAPIKey: "sk-test"})
}

// 返回指定状态码的上游假服务
func fakeUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func addEndpoint(t *testing.T, rs *RouteService, groupID int64, name, url string) int64 {
	t.Helper()
	if err := rs.AddFailoverEndpoint(groupID, name, url, "sk-upstream", "openai"); err != nil {
		t.Fatalf("add endpoint %s: %v", name, err)
	}
	eps, err := rs.GetFailoverEndpoints(groupID, true)
	if err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	for _, ep := range eps {
		if ep.Name == name {
			return ep.ID
		}
	}
	t.Fatalf("endpoint %s not found after insert", name)
	return 0
}

// 冷却计时随请求滑动：好端点接管后，坏端点要等到分组真正静默 N 秒才恢复
func TestGroupCooldownResetsOnEveryRequest(t *testing.T) {
	rs, ps := newTestProxy(t)

	bad := fakeUpstream(t, http.StatusInternalServerError)
	good := fakeUpstream(t, http.StatusOK)

	const cooldownSeconds = 2
	groupID, err := rs.AddFailoverGroup("主力组", "model-x", cooldownSeconds, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	badID := addEndpoint(t, rs, groupID, "bad", bad.URL)
	goodID := addEndpoint(t, rs, groupID, "good", good.URL)

	group, err := rs.GetFailoverGroupByID(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}

	attempt := func(target *database.ModelRoute) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, target.APIUrl, strings.NewReader(`{}`))
		return ps.httpClient.Do(req)
	}

	// 第一次请求：首选坏端点，失败后顺延到好端点
	res, err := ps.resolveGroupTargets(group)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Targets) != 2 || res.Targets[0].ID != badID {
		t.Fatalf("first resolve should start at the highest priority endpoint, got %v", ids(res.Targets))
	}
	resp, served, err := ps.doWithFailover(res, attempt)
	if err != nil {
		t.Fatalf("failover request: %v", err)
	}
	resp.Body.Close()
	if served == nil || served.ID != goodID {
		t.Fatalf("request should be served by the healthy endpoint, got %+v", served)
	}

	// 坏端点进入冷却，后续请求直接从好端点开始
	deadline := time.Now().Add(time.Duration(cooldownSeconds)*time.Second - 400*time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		res, err := ps.resolveGroupTargets(group)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(res.Targets) != 1 || res.Targets[0].ID != goodID {
			t.Fatalf("cooling endpoint must stay skipped while traffic continues, got %v", ids(res.Targets))
		}
		status := ps.FailoverStatus(groupID, cooldownSeconds, group.Strategy)
		if !coolingOf(status, badID) {
			t.Fatal("UI status must report the endpoint as cooling while the group keeps receiving requests")
		}
		// 计时器被每次请求重置：剩余时间应始终接近 N 秒
		if remain := remainOf(status, badID); remain < cooldownSeconds-1 {
			t.Fatalf("cooldown timer was not reset by requests, remain=%ds", remain)
		}
	}

	// 分组静默超过 N 秒后，坏端点恢复为可用
	time.Sleep(time.Duration(cooldownSeconds)*time.Second + 200*time.Millisecond)
	res, err = ps.resolveGroupTargets(group)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Targets) != 2 || res.Targets[0].ID != badID {
		t.Fatalf("after idle cooldown the priority head must be picked again, got %v", ids(res.Targets))
	}
}

func TestGroupStrategyOrdering(t *testing.T) {
	rs, ps := newTestProxy(t)

	groupID, err := rs.AddFailoverGroup("轮询组", "model-rr", 600, StrategyPolling)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	a := addEndpoint(t, rs, groupID, "a", "http://127.0.0.1:1/v1")
	b := addEndpoint(t, rs, groupID, "b", "http://127.0.0.1:2/v1")

	group, err := rs.GetFailoverGroupByID(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}

	var firsts []int64
	for i := 0; i < 4; i++ {
		res, err := ps.resolveGroupTargets(group)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(res.Targets) != 2 {
			t.Fatalf("expected 2 candidates, got %v", ids(res.Targets))
		}
		firsts = append(firsts, res.Targets[0].ID)
	}
	want := []int64{a, b, a, b}
	if !equalIDs(firsts, want) {
		t.Fatalf("polling should rotate endpoints, got %v want %v", firsts, want)
	}

	// 切到均衡负载：首选端点应会在两个端点之间分布
	if err := rs.UpdateFailoverGroup(groupID, "均衡组", "model-rr", 600, StrategyBalanced, true); err != nil {
		t.Fatalf("update group: %v", err)
	}
	ps.ResetFailoverState(groupID)
	group, _ = rs.GetFailoverGroupByID(groupID)

	heads := map[int64]bool{}
	for i := 0; i < 40; i++ {
		res, err := ps.resolveGroupTargets(group)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		heads[res.Targets[0].ID] = true
	}
	if len(heads) < 2 {
		t.Fatalf("balanced strategy should spread the first pick, got %v", heads)
	}
}

// 可切换状态码的假上游（用于模拟端点恢复）
func switchableUpstream(t *testing.T, status int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	code := &atomic.Int32{}
	code.Store(status)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(code.Load()))
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, code
}

// 全端点冷却后的兜底重试一旦成功，必须立刻解除成功端点的冷却，
// 否则 Begin() 会在每次请求时重置计时器，所有端点将永远停留在"冷却中"。
func TestFallbackSuccessClearsCooldown(t *testing.T) {
	rs, ps := newTestProxy(t)

	badSrv, badCode := switchableUpstream(t, http.StatusInternalServerError)
	flakySrv, flakyCode := switchableUpstream(t, http.StatusInternalServerError)
	_ = badCode

	const cooldownSeconds = 600
	groupID, err := rs.AddFailoverGroup("兜底组", "model-fb", cooldownSeconds, StrategyFailover)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}
	badID := addEndpoint(t, rs, groupID, "bad", badSrv.URL)
	flakyID := addEndpoint(t, rs, groupID, "flaky", flakySrv.URL)

	group, err := rs.GetFailoverGroupByID(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}

	attempt := func(target *database.ModelRoute) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, target.APIUrl, strings.NewReader(`{}`))
		return ps.httpClient.Do(req)
	}

	failOnce := func() {
		res, err := ps.resolveGroupTargets(group)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		resp, _, err := ps.doWithFailover(res, attempt)
		if err == nil {
			resp.Body.Close()
			t.Fatal("expected every endpoint to fail")
		}
	}

	// 两轮全失败，让两个端点都进入冷却
	failOnce()
	failOnce()

	status := ps.FailoverStatus(groupID, cooldownSeconds, group.Strategy)
	if !coolingOf(status, badID) || !coolingOf(status, flakyID) {
		t.Fatalf("both endpoints should be cooling, got bad=%v flaky=%v",
			coolingOf(status, badID), coolingOf(status, flakyID))
	}

	// 第二个端点恢复
	flakyCode.Store(http.StatusOK)

	res, err := ps.resolveGroupTargets(group)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.AllCooling || len(res.Targets) != 2 {
		t.Fatalf("expected all-cooling fallback over both endpoints, got allCooling=%v targets=%v",
			res.AllCooling, ids(res.Targets))
	}

	resp, served, err := ps.doWithFailover(res, attempt)
	if err != nil {
		t.Fatalf("fallback request: %v", err)
	}
	resp.Body.Close()
	if served == nil || served.ID != flakyID {
		t.Fatalf("recovered endpoint should serve the request, got %+v", served)
	}

	status = ps.FailoverStatus(groupID, cooldownSeconds, group.Strategy)
	if coolingOf(status, flakyID) {
		t.Fatal("cooldown of the endpoint that just served a request must be cleared")
	}
	if !coolingOf(status, badID) {
		t.Fatal("endpoints that still fail must stay cooling")
	}

	// 后续请求应直接命中已恢复端点，而不再走"全部冷却"兜底
	res, err = ps.resolveGroupTargets(group)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.AllCooling {
		t.Fatal("group must not be in all-cooling fallback any more")
	}
	if len(res.Targets) != 1 || res.Targets[0].ID != flakyID {
		t.Fatalf("only the recovered endpoint should be a candidate, got %v", ids(res.Targets))
	}
}

func coolingOf(status FailoverGroupStatus, endpointID int64) bool {
	for _, st := range status.Endpoints {
		if st.EndpointID == endpointID {
			return st.Cooling
		}
	}
	return false
}

func remainOf(status FailoverGroupStatus, endpointID int64) int {
	for _, st := range status.Endpoints {
		if st.EndpointID == endpointID {
			return st.RemainSecond
		}
	}
	return -1
}
