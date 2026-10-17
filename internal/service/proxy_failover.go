package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openai-router-go/internal/database"

	log "github.com/sirupsen/logrus"
)

// routeResolution 一次请求的目标解析结果
type routeResolution struct {
	Group   *database.FailoverGroup // nil 表示普通（未分组）路由
	Targets []*database.ModelRoute  // 按优先级排列的候选端点
	Model   string                  // 上游实际使用的模型名
}

// resolveTargets 解析请求模型对应的候选端点
// 顺序：重定向关键字 -> 故障转移分组 -> 普通路由（随机负载均衡）
func (s *ProxyService) resolveTargets(model string) (*routeResolution, error) {
	// 1. 重定向关键字
	if s.config.RedirectEnabled && (model == s.config.RedirectKeyword || strings.HasPrefix(model, s.config.RedirectKeyword+":")) {
		log.Infof("[Redirect] %s -> group=%d route=%d model=%s",
			model, s.config.RedirectTargetGroupID, s.config.RedirectTargetRouteID, s.config.RedirectTargetModel)

		if s.config.RedirectTargetGroupID > 0 {
			if g, err := s.routeService.GetFailoverGroupByID(s.config.RedirectTargetGroupID); err == nil {
				return s.resolveGroupTargets(g)
			} else {
				log.Warnf("[Redirect] target group %d unavailable: %v", s.config.RedirectTargetGroupID, err)
			}
		}
		if s.config.RedirectTargetRouteID > 0 {
			if r, err := s.routeService.GetRouteByID(s.config.RedirectTargetRouteID); err == nil {
				return &routeResolution{Targets: []*database.ModelRoute{r}, Model: r.Model}, nil
			} else {
				log.Warnf("[Redirect] target route %d unavailable: %v", s.config.RedirectTargetRouteID, err)
			}
		}
		if s.config.RedirectTargetModel == "" {
			return nil, fmt.Errorf("redirect target not configured")
		}
		model = s.config.RedirectTargetModel
	}

	// 2. 故障转移分组
	if g, err := s.routeService.GetFailoverGroupByModel(model); err == nil && g != nil {
		return s.resolveGroupTargets(g)
	}

	// 3. 普通路由
	route, err := s.routeService.GetRouteByModel(model)
	if err != nil {
		return nil, err
	}
	return &routeResolution{Targets: []*database.ModelRoute{route}, Model: route.Model}, nil
}

// resolveGroupTargets 生成分组候选端点列表（跳过冷却中的端点）
func (s *ProxyService) resolveGroupTargets(group *database.FailoverGroup) (*routeResolution, error) {
	endpoints, err := s.routeService.GetFailoverEndpoints(group.ID, true)
	if err != nil {
		return nil, err
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("failover group '%s' has no enabled endpoint", group.Name)
	}

	cooldown := time.Duration(group.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 600 * time.Second
	}
	cooling := s.failover.Begin(group.ID, cooldown)

	targets := make([]*database.ModelRoute, 0, len(endpoints))
	for i := range endpoints {
		if cooling[endpoints[i].ID] {
			continue
		}
		ep := endpoints[i]
		ep.Model = group.Model
		targets = append(targets, &ep)
	}

	if len(targets) == 0 {
		// 全部端点都在冷却：按优先级兜底，避免整个分组不可用
		log.Warnf("[Failover] group %s: all endpoints cooling, falling back to priority order", group.Name)
		for i := range endpoints {
			ep := endpoints[i]
			ep.Model = group.Model
			targets = append(targets, &ep)
		}
	}

	return &routeResolution{Group: group, Targets: targets, Model: group.Model}, nil
}

// isFailoverStatus 判断该状态码是否应触发故障转移
// 依据需求：网络错误、5xx、401/403 触发；429 与其他 4xx 不触发
func isFailoverStatus(code int) bool {
	return code >= 500 || code == http.StatusUnauthorized || code == http.StatusForbidden
}

// doWithFailover 依次尝试候选端点，返回第一个可用的响应
// attempt 负责按目标端点构造并发送请求
func (s *ProxyService) doWithFailover(res *routeResolution, attempt func(target *database.ModelRoute) (*http.Response, error)) (*http.Response, *database.ModelRoute, error) {
	var lastErr error

	for i, target := range res.Targets {
		resp, err := attempt(target)
		if err != nil {
			lastErr = err
			if res.Group != nil {
				s.failover.MarkFailed(res.Group.ID, target.ID)
				log.Warnf("[Failover] group=%s endpoint=%s failed (%v), trying next", res.Group.Name, target.Name, err)
			}
			continue
		}

		if isFailoverStatus(resp.StatusCode) {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
			if res.Group != nil {
				s.failover.MarkFailed(res.Group.ID, target.ID)
				log.Warnf("[Failover] group=%s endpoint=%s returned %d, trying next", res.Group.Name, target.Name, resp.StatusCode)
			}
			continue
		}

		if i > 0 && res.Group != nil {
			log.Infof("[Failover] group=%s served by endpoint=%s (candidate #%d)", res.Group.Name, target.Name, i+1)
		}
		return resp, target, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no available endpoint for model '%s'", res.Model)
	}
	return nil, nil, lastErr
}

// FailoverStatus 返回指定分组的冷却状态（供 UI 展示）
func (s *ProxyService) FailoverStatus(groupID int64, cooldownSeconds int) FailoverGroupStatus {
	eps, err := s.routeService.GetFailoverEndpoints(groupID, true)
	if err != nil {
		eps = nil
	}
	cooldown := time.Duration(cooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 600 * time.Second
	}
	return s.failover.Status(groupID, cooldown, eps)
}

// ResetFailoverState 端点或分组变更后清空冷却状态
func (s *ProxyService) ResetFailoverState(groupID int64) {
	s.failover.Reset(groupID)
}
