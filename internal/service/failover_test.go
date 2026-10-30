package service

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"openai-router-go/internal/database"
)

// ---------------------------------------------------------------------------
// 冷却计时器：分组每收到一次请求就把计时器重置为 N 秒
// ---------------------------------------------------------------------------

func TestBeginResetsCooldownTimerOnEveryRequest(t *testing.T) {
	m := NewFailoverManager()
	const groupID int64 = 1
	const endpointID int64 = 10
	cooldown := 3 * time.Second

	m.MarkFailed(groupID, endpointID)

	// 每 500ms 来一次请求：计时器应被重置，剩余时间始终接近 N 秒
	for i := 0; i < 5; i++ {
		time.Sleep(500 * time.Millisecond)

		cooling := m.Begin(groupID, cooldown)
		if !cooling[endpointID] {
			t.Fatalf("round %d: endpoint must stay cooling while the group keeps receiving requests", i)
		}

		status := m.Status(groupID, cooldown, StrategyFailover, []database.ModelRoute{{ID: endpointID}})
		if !status.Endpoints[0].Cooling {
			t.Fatalf("round %d: status must report the endpoint as cooling", i)
		}
		// 若计时器没有被重置，累计 2.5s 后剩余时间早已所剩无几（旧实现即为如此）
		if status.Endpoints[0].RemainSecond < 2 {
			t.Fatalf("round %d: cooldown timer was not reset, remain=%ds (want ~%ds)",
				i, status.Endpoints[0].RemainSecond, int(cooldown.Seconds()))
		}
	}
}

func TestBeginClearsCooldownAfterIdle(t *testing.T) {
	m := NewFailoverManager()
	const groupID int64 = 2
	const endpointID int64 = 20
	cooldown := 150 * time.Millisecond

	m.MarkFailed(groupID, endpointID)
	if cooling := m.Begin(groupID, cooldown); !cooling[endpointID] {
		t.Fatal("endpoint should be cooling right after the failure")
	}

	// 分组静默超过 N 秒后恢复
	time.Sleep(250 * time.Millisecond)
	if cooling := m.Begin(groupID, cooldown); cooling[endpointID] {
		t.Fatal("cooldown must be cleared after the group stays idle longer than the cooldown")
	}

	status := m.Status(groupID, cooldown, StrategyFailover, []database.ModelRoute{{ID: endpointID}})
	if status.Endpoints[0].Cooling {
		t.Fatal("status must report the endpoint as available after idle reset")
	}
	if status.ActiveID != endpointID {
		t.Fatalf("active endpoint should fall back to priority head, got %d", status.ActiveID)
	}
}

func TestStatusExpiresCooldownAfterIdle(t *testing.T) {
	m := NewFailoverManager()
	const groupID int64 = 3
	const endpointID int64 = 30
	cooldown := 100 * time.Millisecond

	m.MarkFailed(groupID, endpointID)
	time.Sleep(200 * time.Millisecond)

	// 只有 Status 被调用（没有新请求）：冷却也应视为到期，避免 UI 与调度不一致
	status := m.Status(groupID, cooldown, StrategyFailover, []database.ModelRoute{{ID: endpointID}})
	if status.Endpoints[0].Cooling {
		t.Fatal("status must not report cooling once the group has been idle longer than the cooldown")
	}
}

func TestResetClearsGroupRuntime(t *testing.T) {
	m := NewFailoverManager()
	m.MarkFailed(4, 40)
	m.Reset(4)
	if cooling := m.Begin(4, time.Minute); cooling[40] {
		t.Fatal("reset must drop all cooldown state")
	}
}

// ---------------------------------------------------------------------------
// 分组策略：故障转移 / 均衡负载 / 轮询
// ---------------------------------------------------------------------------

func eps(ids ...int64) []*database.ModelRoute {
	out := make([]*database.ModelRoute, 0, len(ids))
	for _, id := range ids {
		out = append(out, &database.ModelRoute{ID: id})
	}
	return out
}

func ids(list []*database.ModelRoute) []int64 {
	out := make([]int64, 0, len(list))
	for _, ep := range list {
		out = append(out, ep.ID)
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOrderEndpointsFailoverKeepsPriority(t *testing.T) {
	m := NewFailoverManager()
	got := m.OrderEndpoints(5, StrategyFailover, eps(1, 2, 3))
	if !equalIDs(ids(got), []int64{1, 2, 3}) {
		t.Fatalf("failover must keep priority order, got %v", ids(got))
	}
}

func TestOrderEndpointsPollingRotates(t *testing.T) {
	m := NewFailoverManager()
	available := eps(1, 2, 3)

	want := [][]int64{{1, 2, 3}, {2, 3, 1}, {3, 1, 2}, {1, 2, 3}}
	for i, w := range want {
		got := m.OrderEndpoints(6, StrategyPolling, available)
		if !equalIDs(ids(got), w) {
			t.Fatalf("polling round %d: got %v want %v", i, ids(got), w)
		}
	}
}

func TestOrderEndpointsPollingResetsIndependentGroups(t *testing.T) {
	m := NewFailoverManager()
	available := eps(1, 2)
	m.OrderEndpoints(7, StrategyPolling, available) // group 7 -> cursor 1

	got := m.OrderEndpoints(8, StrategyPolling, available) // group 8 has its own cursor
	if !equalIDs(ids(got), []int64{1, 2}) {
		t.Fatalf("polling cursors must be per group, got %v", ids(got))
	}
}

func TestOrderEndpointsBalancedKeepsWholeSet(t *testing.T) {
	m := NewFailoverManager()
	available := eps(1, 2, 3)

	heads := make(map[int64]bool)
	for i := 0; i < 100; i++ {
		got := m.OrderEndpoints(9, StrategyBalanced, available)
		if len(got) != 3 {
			t.Fatalf("balanced must keep every candidate, got %v", ids(got))
		}
		seen := make(map[int64]bool)
		for _, id := range ids(got) {
			seen[id] = true
		}
		if !seen[1] || !seen[2] || !seen[3] {
			t.Fatalf("balanced dropped a candidate: %v", ids(got))
		}
		heads[got[0].ID] = true
		// 原切片不能被就地打乱（调用方还在用优先级顺序）
		if available[0].ID != 1 || available[1].ID != 2 || available[2].ID != 3 {
			t.Fatalf("balanced must not mutate the caller's slice, got %v", ids(available))
		}
	}
	if len(heads) < 2 {
		t.Fatal("balanced should spread the first pick across endpoints")
	}
}

func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]string{
		"":             StrategyFailover,
		"failover":     StrategyFailover,
		"unknown":      StrategyFailover,
		"balanced":     StrategyBalanced,
		"Balance":      StrategyBalanced,
		"load_balance": StrategyBalanced,
		"polling":      StrategyPolling,
		"RoundRobin":   StrategyPolling,
		"  POL  ":      StrategyFailover,
		"round_robin":  StrategyPolling,
	}
	for in, want := range cases {
		if got := NormalizeStrategy(in); got != want {
			t.Fatalf("NormalizeStrategy(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 分组策略持久化
// ---------------------------------------------------------------------------

func newTestRouteService(t *testing.T) *RouteService {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRouteService(db)
}

func TestFailoverGroupStrategyRoundTrip(t *testing.T) {
	svc := newTestRouteService(t)

	id, err := svc.AddFailoverGroup("主力组", "model-a", 600, StrategyPolling)
	if err != nil {
		t.Fatalf("add group: %v", err)
	}

	g, err := svc.GetFailoverGroupByID(id)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if g.Strategy != StrategyPolling {
		t.Fatalf("strategy = %q, want %q", g.Strategy, StrategyPolling)
	}

	if err := svc.UpdateFailoverGroup(id, "主力组", "model-a", 300, StrategyBalanced, true); err != nil {
		t.Fatalf("update group: %v", err)
	}
	g, err = svc.GetFailoverGroupByID(id)
	if err != nil {
		t.Fatalf("get group after update: %v", err)
	}
	if g.Strategy != StrategyBalanced || g.CooldownSeconds != 300 {
		t.Fatalf("after update strategy=%q cooldown=%d", g.Strategy, g.CooldownSeconds)
	}

	// 无法识别的策略值回落到故障转移
	if err := svc.UpdateFailoverGroup(id, "主力组", "model-a", 300, "weird-strategy", true); err != nil {
		t.Fatalf("update group: %v", err)
	}
	g, _ = svc.GetFailoverGroupByID(id)
	if g.Strategy != StrategyFailover {
		t.Fatalf("unknown strategy should fall back to failover, got %q", g.Strategy)
	}

	groups, err := svc.GetAllFailoverGroups()
	if err != nil {
		t.Fatalf("get all groups: %v", err)
	}
	if len(groups) != 1 || groups[0].Strategy != StrategyFailover {
		t.Fatalf("unexpected groups: %+v", groups)
	}
}

// 旧库没有 strategy 列，迁移后必须能读到一个合法的默认策略
func TestMigrateAddsStrategyColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE failover_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		model TEXT NOT NULL,
		cooldown_seconds INTEGER DEFAULT 600,
		enabled INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO failover_groups (name, model, cooldown_seconds, enabled) VALUES ('legacy', 'model-legacy', 600, 1);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	raw.Close()

	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer db.Close()

	svc := NewRouteService(db)
	groups, err := svc.GetAllFailoverGroups()
	if err != nil {
		t.Fatalf("get all groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 legacy group, got %d", len(groups))
	}
	if groups[0].Strategy != StrategyFailover {
		t.Fatalf("legacy group strategy = %q, want %q", groups[0].Strategy, StrategyFailover)
	}
}
