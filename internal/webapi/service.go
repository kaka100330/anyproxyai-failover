package webapi

import (
	"fmt"
	"time"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
	"openai-router-go/internal/service"

	log "github.com/sirupsen/logrus"
)

// RouteInfo 是前端使用的路由/端点 DTO。
type RouteInfo struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Model           string `json:"model"`
	APIUrl          string `json:"api_url"`
	APIKey          string `json:"api_key"`
	Format          string `json:"format"`
	UpstreamModel   string `json:"upstream_model"`
	FailoverGroupID int64  `json:"failover_group_id"`
	Priority        int    `json:"priority"`
	Enabled         bool   `json:"enabled"`
	Created         string `json:"created"`
	Updated         string `json:"updated"`
}

func routeInfoFromModel(route database.ModelRoute) RouteInfo {
	return RouteInfo{
		ID:              route.ID,
		Name:            route.Name,
		Model:           route.Model,
		APIUrl:          route.APIUrl,
		APIKey:          route.APIKey,
		Format:          route.Format,
		UpstreamModel:   route.UpstreamModel,
		FailoverGroupID: route.FailoverGroupID,
		Priority:        route.Priority,
		Enabled:         route.Enabled,
		Created:         route.CreatedAt.Format("2006-01-02 15:04:05"),
		Updated:         route.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

type StatsInfo struct {
	RouteCount    int     `json:"route_count"`
	ModelCount    int     `json:"model_count"`
	TotalRequests int64   `json:"total_requests"`
	TotalTokens   int64   `json:"total_tokens"`
	TodayRequests int64   `json:"today_requests"`
	TodayTokens   int64   `json:"today_tokens"`
	SuccessRate   float64 `json:"success_rate"`
}

type FailoverGroupInfo struct {
	ID              int64       `json:"id"`
	Name            string      `json:"name"`
	Model           string      `json:"model"`
	CooldownSeconds int         `json:"cooldown_seconds"`
	Strategy        string      `json:"strategy"`
	Enabled         bool        `json:"enabled"`
	Endpoints       []RouteInfo `json:"endpoints"`
	Status          interface{} `json:"status"`
}

// Service 是 Web 管理 API 暴露的所有操作。它与 Wails 的 AppService 解耦，
// 因此服务端构建不需要 GTK/WebKit 等桌面依赖。
type Service struct {
	RouteService *service.RouteService
	ProxyService *service.ProxyService
	Config       *config.Config
	Restart      func()
}

func NewService(routeService *service.RouteService, proxyService *service.ProxyService, cfg *config.Config) *Service {
	return &Service{RouteService: routeService, ProxyService: proxyService, Config: cfg}
}

// GetLanguage 获取当前语言设置。
func (s *Service) GetLanguage() string { return s.Config.Language }

// SetLanguage 设置语言。
func (s *Service) SetLanguage(lang string) error {
	s.Config.Language = lang
	return s.Config.Save()
}

// GetRoutes 获取所有路由（含分组端点）。
func (s *Service) GetRoutes() ([]RouteInfo, error) {
	routes, err := s.RouteService.GetAllRoutes()
	if err != nil {
		return nil, err
	}
	result := make([]RouteInfo, len(routes))
	for i, route := range routes {
		result[i] = routeInfoFromModel(route)
	}
	return result, nil
}

// AddRoute 添加未分组路由。
func (s *Service) AddRoute(name, model, apiUrl, apiKey, format string) error {
	return s.RouteService.AddRoute(name, model, apiUrl, apiKey, format)
}

// UpdateRoute 更新未分组路由。
func (s *Service) UpdateRoute(id int64, name, model, apiUrl, apiKey, format string) error {
	return s.RouteService.UpdateRoute(id, name, model, apiUrl, apiKey, format)
}

// DeleteRoute 删除路由。
func (s *Service) DeleteRoute(id int64) error {
	return s.RouteService.DeleteRoute(id)
}

// GetFailoverGroups 获取所有故障转移分组及其端点。
func (s *Service) GetFailoverGroups() ([]FailoverGroupInfo, error) {
	groups, err := s.RouteService.GetFailoverGroupsWithEndpoints()
	if err != nil {
		return nil, err
	}
	result := make([]FailoverGroupInfo, 0, len(groups))
	for _, g := range groups {
		eps := make([]RouteInfo, 0, len(g.Endpoints))
		for _, ep := range g.Endpoints {
			eps = append(eps, routeInfoFromModel(ep))
		}
		result = append(result, FailoverGroupInfo{
			ID:              g.ID,
			Name:            g.Name,
			Model:           g.Model,
			CooldownSeconds: g.CooldownSeconds,
			Strategy:        g.Strategy,
			Enabled:         g.Enabled,
			Endpoints:       eps,
			Status:          s.ProxyService.FailoverStatus(g.ID, g.CooldownSeconds, g.Strategy),
		})
	}
	return result, nil
}

// AddFailoverGroup 新建故障转移分组。
func (s *Service) AddFailoverGroup(name, model string, cooldownSeconds int, strategy string) (int64, error) {
	return s.RouteService.AddFailoverGroup(name, model, cooldownSeconds, strategy)
}

// UpdateFailoverGroup 更新故障转移分组。
func (s *Service) UpdateFailoverGroup(id int64, name, model string, cooldownSeconds int, strategy string, enabled bool) error {
	err := s.RouteService.UpdateFailoverGroup(id, name, model, cooldownSeconds, strategy, enabled)
	if err == nil {
		s.ProxyService.ResetFailoverState(id)
	}
	return err
}

// DeleteFailoverGroup 删除故障转移分组。
func (s *Service) DeleteFailoverGroup(id int64) error {
	err := s.RouteService.DeleteFailoverGroup(id)
	if err == nil {
		s.ProxyService.ResetFailoverState(id)
	}
	return err
}

// AddFailoverEndpoint 向分组添加端点。
func (s *Service) AddFailoverEndpoint(groupID int64, name, apiUrl, apiKey, format string) error {
	return s.AddFailoverEndpointWithUpstream(groupID, name, "", apiUrl, apiKey, format)
}

// AddFailoverEndpointWithUpstream 向分组添加端点，并指定上游模型。
func (s *Service) AddFailoverEndpointWithUpstream(groupID int64, name, upstreamModel, apiUrl, apiKey, format string) error {
	err := s.RouteService.AddFailoverEndpointWithUpstream(groupID, name, upstreamModel, apiUrl, apiKey, format)
	if err == nil {
		s.ProxyService.ResetFailoverState(groupID)
	}
	return err
}

// UpdateFailoverEndpoint 更新分组端点。
func (s *Service) UpdateFailoverEndpoint(id int64, name, apiUrl, apiKey, format string, priority int) error {
	err := s.RouteService.UpdateFailoverEndpoint(id, name, apiUrl, apiKey, format, priority)
	if err == nil {
		s.ProxyService.ResetFailoverState(s.endpointGroupID(id))
	}
	return err
}

// SetFailoverEndpointUpstreamModel 设置端点上游模型。
func (s *Service) SetFailoverEndpointUpstreamModel(id int64, upstreamModel string) error {
	err := s.RouteService.SetFailoverEndpointUpstreamModel(id, upstreamModel)
	if err == nil {
		s.ProxyService.ResetFailoverState(s.endpointGroupID(id))
	}
	return err
}

// DeleteFailoverEndpoint 删除分组端点。
func (s *Service) DeleteFailoverEndpoint(id int64) error {
	groupID := s.endpointGroupID(id)
	err := s.RouteService.DeleteFailoverEndpoint(id)
	if err == nil {
		s.ProxyService.ResetFailoverState(groupID)
	}
	return err
}

// MoveFailoverEndpoint 调整端点优先级。
func (s *Service) MoveFailoverEndpoint(id int64, direction int) error {
	groupID := s.endpointGroupID(id)
	err := s.RouteService.MoveFailoverEndpoint(id, direction)
	if err == nil {
		s.ProxyService.ResetFailoverState(groupID)
	}
	return err
}

func (s *Service) endpointGroupID(id int64) int64 {
	routes, err := s.RouteService.GetAllRoutes()
	if err != nil {
		return 0
	}
	for _, r := range routes {
		if r.ID == id {
			return r.FailoverGroupID
		}
	}
	return 0
}

// GetStats 获取统计信息。
func (s *Service) GetStats() (StatsInfo, error) {
	stats, err := s.RouteService.GetStats()
	if err != nil {
		return StatsInfo{}, err
	}
	result := StatsInfo{}
	if v, ok := stats["route_count"].(int); ok {
		result.RouteCount = v
	}
	if v, ok := stats["model_count"].(int); ok {
		result.ModelCount = v
	}
	if v, ok := stats["total_requests"].(int); ok {
		result.TotalRequests = int64(v)
	}
	if v, ok := stats["total_tokens"].(int); ok {
		result.TotalTokens = int64(v)
	}
	if v, ok := stats["today_requests"].(int); ok {
		result.TodayRequests = int64(v)
	}
	if v, ok := stats["today_tokens"].(int); ok {
		result.TodayTokens = int64(v)
	}
	if v, ok := stats["success_rate"].(float64); ok {
		result.SuccessRate = v
	}
	return result, nil
}

func (s *Service) GetDailyStats(days int) ([]map[string]interface{}, error) {
	return s.RouteService.GetDailyStats(days)
}

func (s *Service) GetHourlyStats() ([]map[string]interface{}, error) {
	return s.RouteService.GetHourlyStats()
}

func (s *Service) GetSecondlyStats(minutes int) ([]map[string]interface{}, error) {
	return s.RouteService.GetSecondlyStats(minutes)
}

func (s *Service) GetModelRanking(limit int) ([]map[string]interface{}, error) {
	return s.RouteService.GetModelRanking(limit)
}

func (s *Service) ClearStats() error { return s.RouteService.ClearStats() }

func (s *Service) CompressDatabase() (map[string]interface{}, error) {
	return s.RouteService.CompressDatabase()
}

func (s *Service) GetUsageSummary() (map[string]interface{}, error) {
	return s.RouteService.GetUsageSummary()
}

// GetConfig 获取前端展示所需的配置。
func (s *Service) GetConfig() map[string]interface{} {
	endpoint := s.Config.PublicBaseURL
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", s.Config.Host, s.Config.Port)
	}
	return map[string]interface{}{
		"localApiKey":           s.Config.LocalAPIKey,
		"openaiEndpoint":        endpoint,
		"redirectEnabled":       s.Config.RedirectEnabled,
		"redirectKeyword":       s.Config.RedirectKeyword,
		"redirectTargetModel":   s.Config.RedirectTargetModel,
		"redirectTargetName":    s.Config.RedirectTargetName,
		"redirectTargetRouteId": s.Config.RedirectTargetRouteID,
		"redirectTargetGroupId": s.Config.RedirectTargetGroupID,
		"minimizeToTray":        false,
		"autoStart":             false,
		"enableFileLog":         s.Config.EnableFileLog,
		"sanitizeThinking":      s.Config.SanitizeThinking,
		"port":                  s.Config.Port,
		"webMode":               true,
	}
}

// UpdateConfig 更新重定向等配置。
func (s *Service) UpdateConfig(redirectEnabled bool, redirectKeyword, redirectTargetModel string, redirectTargetRouteId int64, redirectTargetGroupId int64) error {
	s.Config.RedirectEnabled = redirectEnabled
	s.Config.RedirectKeyword = redirectKeyword
	s.Config.RedirectTargetModel = redirectTargetModel
	s.Config.RedirectTargetRouteID = redirectTargetRouteId
	s.Config.RedirectTargetGroupID = redirectTargetGroupId
	return s.Config.Save()
}

func (s *Service) UpdatePort(port int) error {
	s.Config.Port = port
	return s.Config.Save()
}

func (s *Service) UpdateLocalApiKey(newApiKey string) error {
	s.Config.LocalAPIKey = newApiKey
	return s.Config.Save()
}

// GetAppSettings 获取服务器端的应用设置（桌面专属项返回 false）。
func (s *Service) GetAppSettings() map[string]interface{} {
	return map[string]interface{}{
		"minimizeToTray":   false,
		"autoStart":        false,
		"autoStartEnabled": false,
	}
}

func (s *Service) SetMinimizeToTray(enabled bool) error { return nil }

func (s *Service) SetAutoStart(enabled bool) error { return nil }

func (s *Service) SetEnableFileLog(enabled bool) error {
	s.Config.EnableFileLog = enabled
	return s.Config.Save()
}

func (s *Service) SetSanitizeThinking(enabled bool) error {
	s.Config.SanitizeThinking = enabled
	return s.Config.Save()
}

func (s *Service) FetchRemoteModels(apiUrl, apiKey string) ([]string, error) {
	return s.ProxyService.FetchRemoteModels(apiUrl, apiKey)
}

func (s *Service) ImportRouteFromFormat(name, model, apiUrl, apiKey, targetFormat string) (string, error) {
	return s.RouteService.ImportRouteFromFormat(name, model, apiUrl, apiKey, targetFormat)
}

// RestartApp 请求进程退出；容器/systemd 会自动拉起。
func (s *Service) RestartApp() error {
	if s.Restart == nil {
		return fmt.Errorf("restart is not supported in this deployment")
	}
	log.Info("Restart requested from web UI")
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Restart()
	}()
	return nil
}
