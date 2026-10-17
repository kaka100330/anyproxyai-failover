package service

import (
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	"openai-router-go/internal/database"

	log "github.com/sirupsen/logrus"
)

// ---------------------------------------------------------------------------
// 故障转移分组 CRUD
// ---------------------------------------------------------------------------

// FailoverGroupView 分组 + 端点（供前端展示）
type FailoverGroupView struct {
	database.FailoverGroup
	Endpoints []database.ModelRoute `json:"endpoints"`
}

// GetAllFailoverGroups 获取所有故障转移分组
func (s *RouteService) GetAllFailoverGroups() ([]database.FailoverGroup, error) {
	rows, err := s.db.Query(`SELECT id, name, model, COALESCE(cooldown_seconds, 600), enabled, created_at, updated_at
	                         FROM failover_groups ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []database.FailoverGroup
	for rows.Next() {
		var g database.FailoverGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Model, &g.CooldownSeconds, &g.Enabled, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// GetFailoverGroupsWithEndpoints 获取分组及其全部端点（含已禁用端点）
func (s *RouteService) GetFailoverGroupsWithEndpoints() ([]FailoverGroupView, error) {
	groups, err := s.GetAllFailoverGroups()
	if err != nil {
		return nil, err
	}

	views := make([]FailoverGroupView, 0, len(groups))
	for _, g := range groups {
		eps, err := s.GetFailoverEndpoints(g.ID, false)
		if err != nil {
			return nil, err
		}
		views = append(views, FailoverGroupView{FailoverGroup: g, Endpoints: eps})
	}
	return views, nil
}

// GetFailoverGroupByModel 按对外模型ID查找启用的分组
func (s *RouteService) GetFailoverGroupByModel(model string) (*database.FailoverGroup, error) {
	return s.scanFailoverGroup(`SELECT id, name, model, COALESCE(cooldown_seconds, 600), enabled, created_at, updated_at
	                            FROM failover_groups WHERE model = ? AND enabled = 1`, model)
}

// GetFailoverGroupByID 按ID查找分组（仅启用）
func (s *RouteService) GetFailoverGroupByID(id int64) (*database.FailoverGroup, error) {
	return s.scanFailoverGroup(`SELECT id, name, model, COALESCE(cooldown_seconds, 600), enabled, created_at, updated_at
	                            FROM failover_groups WHERE id = ? AND enabled = 1`, id)
}

func (s *RouteService) scanFailoverGroup(query string, arg interface{}) (*database.FailoverGroup, error) {
	var g database.FailoverGroup
	err := s.db.QueryRow(query, arg).Scan(&g.ID, &g.Name, &g.Model, &g.CooldownSeconds, &g.Enabled, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("failover group not found: %v", arg)
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// AddFailoverGroup 新建分组
func (s *RouteService) AddFailoverGroup(name, model string, cooldownSeconds int) (int64, error) {
	if name == "" || model == "" {
		return 0, fmt.Errorf("分组名称和模型ID不能为空")
	}
	if cooldownSeconds <= 0 {
		cooldownSeconds = 600
	}

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM failover_groups WHERE model = ?`, model).Scan(&cnt); err != nil {
		return 0, err
	}
	if cnt > 0 {
		return 0, fmt.Errorf("模型 %s 已被其他故障转移分组使用", model)
	}

	now := time.Now()
	res, err := s.db.Exec(`INSERT INTO failover_groups (name, model, cooldown_seconds, enabled, created_at, updated_at)
	                       VALUES (?, ?, ?, 1, ?, ?)`, name, model, cooldownSeconds, now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	log.Infof("Failover group added: id=%d name=%s model=%s cooldown=%ds", id, name, model, cooldownSeconds)
	return id, nil
}

// UpdateFailoverGroup 更新分组；模型ID变更时同步组内端点
func (s *RouteService) UpdateFailoverGroup(id int64, name, model string, cooldownSeconds int, enabled bool) error {
	if name == "" || model == "" {
		return fmt.Errorf("分组名称和模型ID不能为空")
	}
	if cooldownSeconds <= 0 {
		cooldownSeconds = 600
	}

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM failover_groups WHERE model = ? AND id <> ?`, model, id).Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return fmt.Errorf("模型 %s 已被其他故障转移分组使用", model)
	}

	res, err := s.db.Exec(`UPDATE failover_groups SET name = ?, model = ?, cooldown_seconds = ?, enabled = ?, updated_at = ?
	                       WHERE id = ?`, name, model, cooldownSeconds, enabled, time.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("failover group not found: id=%d", id)
	}

	// 组内端点使用同一个模型ID
	if _, err := s.db.Exec(`UPDATE model_routes SET model = ?, updated_at = ? WHERE failover_group_id = ?`, model, time.Now(), id); err != nil {
		log.Warnf("Failed to sync endpoint model for group %d: %v", id, err)
	}

	log.Infof("Failover group updated: id=%d name=%s model=%s enabled=%v", id, name, model, enabled)
	return nil
}

// DeleteFailoverGroup 删除分组及其全部端点
func (s *RouteService) DeleteFailoverGroup(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM request_logs WHERE route_id IN (SELECT id FROM model_routes WHERE failover_group_id = ?)`, id); err != nil {
		log.Warnf("Failed to delete endpoint logs: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM model_routes WHERE failover_group_id = ?`, id); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM failover_groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("failover group not found: id=%d", id)
	}
	log.Infof("Failover group deleted: id=%d", id)
	return nil
}

// ---------------------------------------------------------------------------
// 分组端点
// ---------------------------------------------------------------------------

// GetFailoverEndpoints 获取分组端点；onlyEnabled = true 时只返回启用的端点
// 结果按 priority 升序（越小越优先）
func (s *RouteService) GetFailoverEndpoints(groupID int64, onlyEnabled bool) ([]database.ModelRoute, error) {
	query := `SELECT ` + routeColumns + ` FROM model_routes WHERE failover_group_id = ?`
	if onlyEnabled {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY COALESCE(priority, 0) ASC, id ASC`

	rows, err := s.db.Query(query, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var eps []database.ModelRoute
	for rows.Next() {
		ep, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		eps = append(eps, ep)
	}
	return eps, nil
}

// AddFailoverEndpoint 向分组追加端点，优先级排在末尾
func (s *RouteService) AddFailoverEndpoint(groupID int64, name, apiUrl, apiKey, format string) error {
	if groupID <= 0 {
		return fmt.Errorf("无效的分组ID")
	}
	group, err := s.GetFailoverGroupByID(groupID)
	if err != nil {
		return err
	}

	var nextPriority int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(priority), -1) + 1 FROM model_routes WHERE failover_group_id = ?`, groupID).Scan(&nextPriority); err != nil {
		nextPriority = 0
	}

	if format == "" {
		format = "openai"
	}
	now := time.Now()
	_, err = s.db.Exec(`INSERT INTO model_routes (name, model, api_url, api_key, format, failover_group_id, priority, enabled, created_at, updated_at)
	                    VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		name, group.Model, apiUrl, apiKey, format, groupID, nextPriority, now, now)
	if err != nil {
		return err
	}
	log.Infof("Failover endpoint added: group=%d name=%s priority=%d", groupID, name, nextPriority)
	return nil
}

// UpdateFailoverEndpoint 更新端点；priority < 0 表示保持原优先级
func (s *RouteService) UpdateFailoverEndpoint(id int64, name, apiUrl, apiKey, format string, priority int) error {
	if priority < 0 {
		return s.UpdateEndpoint(id, name, "", apiUrl, apiKey, format, -1)
	}
	// 保留端点所属分组的模型ID
	var model string
	if err := s.db.QueryRow(`SELECT model FROM model_routes WHERE id = ?`, id).Scan(&model); err != nil {
		return fmt.Errorf("endpoint not found: id=%d", id)
	}
	return s.UpdateEndpoint(id, name, model, apiUrl, apiKey, format, priority)
}

// DeleteFailoverEndpoint 删除端点
func (s *RouteService) DeleteFailoverEndpoint(id int64) error {
	return s.DeleteRoute(id)
}

// MoveFailoverEndpoint 调整端点优先级：direction < 0 上移，> 0 下移
func (s *RouteService) MoveFailoverEndpoint(id int64, direction int) error {
	var groupID int64
	if err := s.db.QueryRow(`SELECT COALESCE(failover_group_id, 0) FROM model_routes WHERE id = ?`, id).Scan(&groupID); err != nil {
		return fmt.Errorf("endpoint not found: id=%d", id)
	}
	if groupID == 0 {
		return fmt.Errorf("路由未属于任何故障转移分组")
	}

	eps, err := s.GetFailoverEndpoints(groupID, false)
	if err != nil {
		return err
	}
	idx := -1
	for i, ep := range eps {
		if ep.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("endpoint not found in group: id=%d", id)
	}

	target := idx + direction
	if target < 0 || target >= len(eps) {
		return nil // 已在边界，静默忽略
	}

	// 交换两条记录的优先级
	a, b := eps[idx], eps[target]
	if _, err := s.db.Exec(`UPDATE model_routes SET priority = ? WHERE id = ?`, b.Priority, a.ID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE model_routes SET priority = ? WHERE id = ?`, a.Priority, b.ID); err != nil {
		return err
	}

	// 优先级出现重复时重新编号，保证顺序稳定
	return s.normalizePriorities(groupID)
}

func (s *RouteService) normalizePriorities(groupID int64) error {
	eps, err := s.GetFailoverEndpoints(groupID, false)
	if err != nil {
		return err
	}
	sort.SliceStable(eps, func(i, j int) bool { return eps[i].Priority < eps[j].Priority })
	for i, ep := range eps {
		if ep.Priority != i {
			if _, err := s.db.Exec(`UPDATE model_routes SET priority = ? WHERE id = ?`, i, ep.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 故障转移运行时状态（冷却）
// ---------------------------------------------------------------------------

// FailoverEndpointStatus 端点冷却状态（供前端展示）
type FailoverEndpointStatus struct {
	EndpointID   int64 `json:"endpoint_id"`
	Cooling      bool  `json:"cooling"`
	RemainSecond int   `json:"remain_seconds"`
}

// FailoverGroupStatus 分组运行状态
type FailoverGroupStatus struct {
	GroupID     int64                    `json:"group_id"`
	ActiveID    int64                    `json:"active_endpoint_id"`
	LastActive  time.Time                `json:"last_active"`
	Endpoints   []FailoverEndpointStatus `json:"endpoints"`
}

type failoverGroupRuntime struct {
	lastActivity time.Time
	failed       map[int64]time.Time
}

// FailoverManager 维护每个分组的冷却状态
// 规则：端点失败后被标记；分组每收到一次请求就把计时器重置为 N 秒；
// 只有分组静默超过 N 秒后，失败标记才会清除，选择回到最高优先级。
type FailoverManager struct {
	mu     sync.Mutex
	groups map[int64]*failoverGroupRuntime
}

func NewFailoverManager() *FailoverManager {
	return &FailoverManager{groups: make(map[int64]*failoverGroupRuntime)}
}

func (m *FailoverManager) runtime(groupID int64) *failoverGroupRuntime {
	rt, ok := m.groups[groupID]
	if !ok {
		rt = &failoverGroupRuntime{failed: make(map[int64]time.Time)}
		m.groups[groupID] = rt
	}
	return rt
}

// Begin 开始一次分组请求：先判断是否已静默足够久（是则清除冷却），再刷新活动时间
// 返回当前处于冷却中的端点集合
func (m *FailoverManager) Begin(groupID int64, cooldown time.Duration) map[int64]bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	rt := m.runtime(groupID)
	now := time.Now()

	if !rt.lastActivity.IsZero() && now.Sub(rt.lastActivity) > cooldown {
		if len(rt.failed) > 0 {
			log.Infof("[Failover] group %d idle for %v, resetting cooldowns", groupID, now.Sub(rt.lastActivity).Round(time.Second))
		}
		rt.failed = make(map[int64]time.Time)
	}
	rt.lastActivity = now

	cooling := make(map[int64]bool, len(rt.failed))
	for id := range rt.failed {
		cooling[id] = true
	}
	return cooling
}

// MarkFailed 标记端点失败
func (m *FailoverManager) MarkFailed(groupID, endpointID int64) {
	if groupID <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rt := m.runtime(groupID)
	now := time.Now()
	rt.failed[endpointID] = now
	rt.lastActivity = now
	log.Warnf("[Failover] endpoint %d in group %d marked as failed", endpointID, groupID)
}

// Reset 清除分组的冷却状态（端点/分组被编辑后调用）
func (m *FailoverManager) Reset(groupID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.groups, groupID)
}

// Status 返回分组当前状态
func (m *FailoverManager) Status(groupID int64, cooldown time.Duration, endpoints []database.ModelRoute) FailoverGroupStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	rt := m.runtime(groupID)
	status := FailoverGroupStatus{
		GroupID:    groupID,
		LastActive: rt.lastActivity,
		Endpoints:  make([]FailoverEndpointStatus, 0, len(endpoints)),
	}

	for _, ep := range endpoints {
		st := FailoverEndpointStatus{EndpointID: ep.ID, Cooling: false}
		if failedAt, ok := rt.failed[ep.ID]; ok {
			remain := cooldown - time.Since(failedAt)
			if remain > 0 {
				st.Cooling = true
				st.RemainSecond = int(remain.Seconds())
			}
		}
		if !st.Cooling && status.ActiveID == 0 {
			status.ActiveID = ep.ID
		}
		status.Endpoints = append(status.Endpoints, st)
	}

	return status
}
