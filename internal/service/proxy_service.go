package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openai-router-go/internal/adapters"
	"openai-router-go/internal/config"
	"openai-router-go/internal/database"

	log "github.com/sirupsen/logrus"
)

type ProxyService struct {
	routeService *RouteService
	config       *config.Config
	httpClient   *http.Client
	failover     *FailoverManager
}

// partialToolCall 用于累积流式 tool_calls 的分片数据
type partialToolCall struct {
	index  int
	id     string
	name   string
	args   string
	fields map[string]interface{}
}

// sendContentBlockStart 发送 Claude content_block_start 事件
func (s *ProxyService) sendContentBlockStart(writer io.Writer, flusher http.Flusher, index int, blockType, blockID string) {
	contentBlock := map[string]interface{}{
		"type": blockType,
	}

	// 根据不同的块类型添加必需的字段
	switch blockType {
	case "thinking":
		// thinking 块必须有 thinking 字段
		contentBlock["thinking"] = ""
	case "text":
		// text 块必须有 text 字段
		contentBlock["text"] = ""
	case "tool_use":
		// tool_use 块需要 id 和 name
		if blockID != "" {
			contentBlock["id"] = blockID
		}
		contentBlock["name"] = "" // name 将在后续的 delta 中填充
		contentBlock["input"] = map[string]interface{}{}
	}

	contentBlockStart := map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	}

	blockStartData, _ := json.Marshal(contentBlockStart)
	fmt.Fprintf(writer, "event: content_block_start\ndata: %s\n\n", string(blockStartData))
	flusher.Flush()
}

// sendContentBlockStop 发送 Claude content_block_stop 事件
func (s *ProxyService) sendContentBlockStop(writer io.Writer, flusher http.Flusher, index int) {
	contentBlockStop := map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	}
	blockStopData, _ := json.Marshal(contentBlockStop)
	fmt.Fprintf(writer, "event: content_block_stop\ndata: %s\n\n", string(blockStopData))
	flusher.Flush()
}

func NewProxyService(routeService *RouteService, cfg *config.Config) *ProxyService {
	return &ProxyService{
		routeService: routeService,
		config:       cfg,
		failover:     NewFailoverManager(),
		httpClient: &http.Client{
			Timeout: 0, // 不设置总超时，因为大模型生成非常耗时
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          200,
				MaxIdleConnsPerHost:   100,
				MaxConnsPerHost:       0, // 不限制并发连接，允许多会话同时请求同一端点
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: 0, // 大模型首字可能很慢，交给调用方控制
			},
		},
	}
}

// getRedirectRoute 获取重定向目标路�?
// 如果配置�?RedirectTargetRouteID，优先使用该ID获取路由
// 否则根据 RedirectTargetModel 查找路由
func (s *ProxyService) getRedirectRoute() (*database.ModelRoute, error) {
	// 优先使用指定的路由ID
	if s.config.RedirectTargetRouteID > 0 {
		route, err := s.routeService.GetRouteByID(s.config.RedirectTargetRouteID)
		if err == nil {
			return route, nil
		}
		log.Warnf("Failed to get route by ID %d, falling back to model lookup: %v", s.config.RedirectTargetRouteID, err)
	}

	// 回退到按模型名查�?
	if s.config.RedirectTargetModel == "" {
		return nil, fmt.Errorf("redirect target model not configured")
	}
	return s.routeService.GetRouteByModel(s.config.RedirectTargetModel)
}

// ProxyRequest 代理请求
func (s *ProxyService) ProxyRequest(requestBody []byte, headers map[string]string) ([]byte, int, error) {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("'model' field is required")
	}

	// 详细日志：记录请求头和请求体
	log.Infof("=== PROXY REQUEST START ===")
	log.Infof("Request model: %s", model)
	log.Infof("Request headers:")
	for k, v := range headers {
		// 隐藏敏感信息
		if strings.Contains(strings.ToLower(k), "authorization") || strings.Contains(strings.ToLower(k), "key") {
			log.Infof("  %s: ***REDACTED***", k)
		} else {
			log.Infof("  %s: %s", k, v)
		}
	}
	log.Infof("Request body: %s", string(requestBody))
	log.Infof("=== PROXY REQUEST DETAILS ===")

	// 提取真实的模型名（处�?Gemini streamGenerateContent 的情况）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字（支持带后缀的模型名�?
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return nil, http.StatusNotFound, fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	requestFormat := detectRequestFormat(reqData)
	if requestFormat == "cursor" {
		convertedReq, cerr := s.adaptCursorRequest(reqData, model)
		if cerr != nil {
			return nil, http.StatusInternalServerError, cerr
		}
		reqData = convertedReq
		requestBody, _ = json.Marshal(reqData)
		requestFormat = "openai"
	}

	var adapterName string
	startTime := time.Now()

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, requestFormat)

		var transformedBody []byte
		var targetURL string
		if adapterName != "" {
			adapter := adapters.GetAdapter(adapterName)
			if adapter == nil {
				return nil, fmt.Errorf("adapter not found: %s", adapterName)
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = s.buildAdapterURL(cleanAPIUrl, adapterName, model)
		} else {
			transformedBody = requestBody
			targetURL = buildOpenAIChatURL(target.APIUrl)
		}

		log.Infof("[Proxy] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		s.routeService.LogRequest(model, 0, 0, 0, 0, false, ferr.Error())
		return nil, http.StatusServiceUnavailable, ferr
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, err.Error())
		return nil, http.StatusInternalServerError, err
	}

	// 详细日志：记录返回结�?
	log.Infof("=== RESPONSE RESULT ===")
	log.Infof("Response status code: %d", resp.StatusCode)
	log.Infof("Response time: %v", time.Since(startTime))
	log.Infof("Response headers:")
	for k, v := range resp.Header {
		log.Infof("  %s: %v", k, v)
	}
	log.Infof("Response body: %s", string(responseBody))
	log.Infof("=== RESPONSE RESULT END ===")

	log.Infof("Response received from %s in %v, status: %d", route.Name, time.Since(startTime), resp.StatusCode)

	// 记录使用情况（使用实际模型名而不是重定向关键字）
	if resp.StatusCode == http.StatusOK {
		var respData map[string]interface{}
		if err := json.Unmarshal(responseBody, &respData); err == nil {
			if usage, ok := respData["usage"].(map[string]interface{}); ok {
				promptTokens := 0
				completionTokens := 0
				totalTokens := 0
				if v, ok := usage["prompt_tokens"].(float64); ok {
					promptTokens = int(v)
				}
				if v, ok := usage["completion_tokens"].(float64); ok {
					completionTokens = int(v)
				}
				if v, ok := usage["total_tokens"].(float64); ok {
					totalTokens = int(v)
				}
				// 兼容 Claude API 的 input_tokens/output_tokens
				if v, ok := usage["input_tokens"].(float64); ok && promptTokens == 0 {
					promptTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok && completionTokens == 0 {
					completionTokens = int(v)
				}
				if totalTokens == 0 {
					totalTokens = promptTokens + completionTokens
				}
				s.routeService.LogRequest(model, route.ID, promptTokens, completionTokens, totalTokens, true, "")
			}
		}
	} else {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, string(responseBody))
	}

	// 如果使用了适配器，转换响应
	if adapterName != "" {
		adapter := adapters.GetAdapter(adapterName)
		if adapter != nil {
			var respData map[string]interface{}
			if err := json.Unmarshal(responseBody, &respData); err == nil {
				log.Infof("=== ADAPTER TRANSFORMATION ===")
				log.Infof("Original response: %s", string(responseBody))
				adaptedResp, err := adapter.AdaptResponse(respData)
				if err != nil {
					log.Errorf("Failed to adapt response: %v", err)
				} else {
					responseBody, _ = json.Marshal(adaptedResp)
					log.Infof("Adapted response: %s", string(responseBody))
				}
				log.Infof("=== ADAPTER TRANSFORMATION END ===")
			}
		}
	}

	return responseBody, resp.StatusCode, nil
}

// ProxyStreamRequest 代理流式请求
func (s *ProxyService) ProxyStreamRequest(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	originalModel := model

	// 详细日志：记录流式请求开�?
	log.Infof("=== STREAM PROXY REQUEST START ===")
	log.Infof("Stream request model: %s", originalModel)
	log.Infof("Stream request headers:")
	for k, v := range headers {
		// 隐藏敏感信息
		if strings.Contains(strings.ToLower(k), "authorization") || strings.Contains(strings.ToLower(k), "key") {
			log.Infof("  %s: ***REDACTED***", k)
		} else {
			log.Infof("  %s: %s", k, v)
		}
	}
	log.Infof("Stream request body: %s", string(requestBody))

	// 提取真实的模型名（处理 Gemini streamGenerateContent 的情况）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	requestFormat := detectRequestFormat(reqData)
	if requestFormat == "cursor" {
		convertedReq, cerr := s.adaptCursorRequest(reqData, model)
		if cerr != nil {
			return cerr
		}
		reqData = convertedReq
		requestBody, _ = json.Marshal(reqData)
		requestFormat = "openai"
	}

	var adapterName string

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, requestFormat)

		var transformedBody []byte
		var targetURL string
		if adapterName != "" {
			adapter := adapters.GetAdapter(adapterName)
			if adapter == nil {
				return nil, fmt.Errorf("adapter not found: %s", adapterName)
			}
			reqData["stream"] = true
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = s.buildAdapterStreamURL(cleanAPIUrl, adapterName, model)
		} else {
			transformedBody = requestBody
			targetURL = buildOpenAIChatURL(target.APIUrl)
		}

		log.Infof("[Stream] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		if adapterName == "anthropic" {
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 流式传输响应
	// 使用实际路由到的模型名（model）用于统�?
	if adapterName != "" {
		// 需要转换SSE�?
		return s.streamWithAdapter(resp.Body, writer, flusher, adapterName, model, route.ID)
	} else {
		// 直接转发SSE�?
		return s.streamDirect(resp.Body, writer, flusher, model, route.ID)
	}
}

// ProxyStreamRequestWithAdapter 代理流式请求，使用指定的适配�?
func (s *ProxyService) ProxyStreamRequestWithAdapter(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher, forceAdapter string) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	originalModel := model

	// 详细日志：记录流式请求开�?
	log.Infof("=== STREAM PROXY REQUEST START (FORCED ADAPTER: %s) ===", forceAdapter)
	log.Infof("Stream request model: %s", originalModel)
	log.Infof("Stream request headers:")
	for k, v := range headers {
		// 隐藏敏感信息
		if strings.Contains(strings.ToLower(k), "authorization") || strings.Contains(strings.ToLower(k), "key") {
			log.Infof("  %s: ***REDACTED***", k)
		} else {
			log.Infof("  %s: %s", k, v)
		}
	}
	log.Infof("Stream request body: %s", string(requestBody))

	// 提取真实的模型名（处理 Gemini streamGenerateContent 的情况）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)
	reqData["stream"] = true

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")

		var transformedBody []byte
		var targetURL string
		if forceAdapter != "" {
			adapter := adapters.GetAdapter(forceAdapter)
			if adapter == nil {
				return nil, fmt.Errorf("forced adapter not found: %s", forceAdapter)
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = s.buildAdapterStreamURL(cleanAPIUrl, forceAdapter, model)
		} else {
			transformedBody, _ = json.Marshal(reqData)
			targetURL = buildOpenAIChatURL(target.APIUrl)
		}

		log.Infof("[Stream/forced=%s] %s -> %s (endpoint: %s)", forceAdapter, model, targetURL, target.Name)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		if forceAdapter == "anthropic" {
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 需要转换SSE流，使用实际路由到的模型�?
	return s.streamWithAdapter(resp.Body, writer, flusher, "openai-to-claude", model, route.ID)
}

// ProxyStreamRequestWithClaudeConversion 代理流式请求，保持原始请求格式但将响应转换为 Claude 格式
func (s *ProxyService) ProxyStreamRequestWithClaudeConversion(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	originalModel := model

	// 详细日志：记录流式请求开�?
	log.Infof("=== STREAM PROXY REQUEST START (CLAUDE CONVERSION) ===")
	log.Infof("Stream request model: %s", originalModel)
	log.Infof("Stream request headers:")
	for k, v := range headers {
		// 隐藏敏感信息
		if strings.Contains(strings.ToLower(k), "authorization") || strings.Contains(strings.ToLower(k), "key") {
			log.Infof("  %s: ***REDACTED***", k)
		} else {
			log.Infof("  %s: %s", k, v)
		}
	}
	log.Infof("Stream request body: %s", string(requestBody))

	// 提取真实的模型名（处理 Gemini streamGenerateContent 的情况）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	reqData["stream"] = true
	requestBody, _ = json.Marshal(reqData)

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		targetURL := buildOpenAIChatURL(target.APIUrl)
		log.Infof("[Stream/ClaudeConversion] %s -> %s (endpoint: %s)", model, targetURL, target.Name)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(requestBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 需要转换SSE流，使用实际路由到的模型�?
	return s.streamWithAdapter(resp.Body, writer, flusher, "openai-to-claude", model, route.ID)
}

// ProxyAnthropicRequest 代理 Anthropic 专用请求，不转换响应格式
func (s *ProxyService) ProxyAnthropicRequest(requestBody []byte, headers map[string]string) ([]byte, int, error) {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("'model' field is required")
	}

	log.Infof("Received Anthropic request for model: %s", model)

	// 提取真实的模型名（处理可能的后缀）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字（支持带后缀的模型名）
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return nil, http.StatusNotFound, fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	var adapterName string
	startTime := time.Now()

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, "claude")

		var transformedBody []byte
		var targetURL string

		switch adapterName {
		case "":
			transformedBody = requestBody
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
		case "claude-to-openai":
			adapter := adapters.GetAdapter("claude-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("claude-to-openai adapter not found")
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
		default:
			log.Warnf("Unsupported adapter for Anthropic request: %s", adapterName)
			transformedBody = requestBody
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
		}

		log.Infof("[Anthropic] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		if adapterName == "" && normalizeFormat(target.Format) == "claude" {
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		s.routeService.LogRequest(model, 0, 0, 0, 0, false, ferr.Error())
		return nil, http.StatusServiceUnavailable, ferr
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, err.Error())
		return nil, http.StatusInternalServerError, err
	}

	log.Infof("Response received from %s in %v, status: %d", route.Name, time.Since(startTime), resp.StatusCode)

	// 记录使用情况和响应转换（如果上游�?OpenAI 格式�?
	if resp.StatusCode == http.StatusOK {
		var respData map[string]interface{}
		if err := json.Unmarshal(responseBody, &respData); err == nil {
			// 记录使用情况
			if usage, ok := respData["usage"].(map[string]interface{}); ok {
				if totalTokens, ok := usage["total_tokens"].(float64); ok {
					promptTokens := 0
					completionTokens := 0
					if pt, ok := usage["prompt_tokens"].(float64); ok {
						promptTokens = int(pt)
					}
					if ct, ok := usage["completion_tokens"].(float64); ok {
						completionTokens = int(ct)
					}
					s.routeService.LogRequest(model, route.ID, promptTokens, completionTokens, int(totalTokens), true, "")
				}
			}

			// 如果使用了claude-to-openai适配�?说明上游是OpenAI格式,需要将响应转换�?Anthropic 格式
			if adapterName == "claude-to-openai" {
				log.Infof("Converting OpenAI response to Anthropic format for /api/anthropic endpoint")
				// �?OpenAI 格式响应转换�?Anthropic 格式
				anthropicResp := s.convertOpenAIToAnthropicResponse(respData)
				if convertedBody, err := json.Marshal(anthropicResp); err == nil {
					log.Infof("Successfully converted response to Anthropic format")
					return convertedBody, resp.StatusCode, nil
				} else {
					log.Errorf("Failed to marshal Anthropic response: %v", err)
				}
			}
		} else {
			log.Errorf("Failed to unmarshal response body: %v", err)
		}
	} else {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, string(responseBody))
	}

	// 对于 Anthropic 上游或转换失败的情况，返回原始响�?
	log.Infof("Returning original response (adapter=%s)", adapterName)
	return responseBody, resp.StatusCode, nil
}

// ProxyAnthropicStreamRequest 代理 Anthropic 专用流式请求
// 请求来自 /api/anthropic/v1/messages，格式为 Claude 格式
// 根据路由配置的 format 决定是否需要转换
func (s *ProxyService) ProxyAnthropicStreamRequest(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	originalModel := model

	// 提取真实的模型名（处理 Gemini streamGenerateContent 的情况）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	var adapterName string

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, "claude")

		var transformedBody []byte
		var targetURL string

		if adapterName == "claude-to-openai" {
			adapter := adapters.GetAdapter("claude-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("adapter not found: claude-to-openai")
			}
			reqData["stream"] = true
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
		} else {
			transformedBody = requestBody
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
		}

		log.Infof("[Anthropic Stream] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		if adapterName == "anthropic" {
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 根据适配器决定如何处理响应流
	// 使用实际路由到的模型名（model）而不是原始请求的模型名（originalModel）用于统�?
	_ = originalModel // 保留原始模型名用于响�?
	if adapterName == "claude-to-openai" {
		// 需要将 OpenAI 流式响应转换�?Claude 流式响应
		log.Infof("[Anthropic Stream] Converting OpenAI stream response to Claude format")
		return s.streamOpenAIToClaude(resp.Body, writer, flusher, model, route.ID)
	}

	// 直接转发SSE流（目标�?Claude 格式，无需转换�?
	return s.streamDirect(resp.Body, writer, flusher, model, route.ID)
}

// streamWithAdapter 使用适配器处理流式响�?
func (s *ProxyService) streamWithAdapter(reader io.Reader, writer io.Writer, flusher http.Flusher, adapterName, model string, routeID int64) error {
	// 获取反向适配器（用于响应转换�?
	// 例如：请求用 openai-to-claude，响应应该用 claude-to-openai
	reverseAdapterName := getReverseAdapterName(adapterName)
	if reverseAdapterName == "" {
		return fmt.Errorf("no reverse adapter for: %s", adapterName)
	}

	log.Infof("[Stream Adapter] Request adapter: %s, Response adapter: %s", adapterName, reverseAdapterName)

	adapter := adapters.GetAdapter(reverseAdapterName)
	if adapter == nil {
		return fmt.Errorf("adapter not found: %s", reverseAdapterName)
	}

	// 发送开始事�?
	startEvents := adapter.AdaptStreamStart(model)
	log.Infof("[Stream Adapter] Sending %d start events", len(startEvents))
	for _, event := range startEvents {
		eventData, _ := json.Marshal(event)
		log.Infof("[STREAM TO CLIENT] Start event: %s", string(eventData))
		fmt.Fprintf(writer, "data: %s\n\n", string(eventData))
	}
	flusher.Flush()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024) // 1MB max

	// 用于累积token统计信息
	var totalPromptTokens int
	var totalCompletionTokens int
	var chunkCount int

	log.Infof("[Stream Adapter] Starting to read chunks from backend...")

	for scanner.Scan() {
		line := scanner.Text()

		log.Infof("[Stream Adapter] Raw line from backend: %s", line)

		// 跳过空行和事件行
		if line == "" || strings.HasPrefix(line, "event:") {
			log.Infof("[Stream Adapter] Skipping line (empty or event)")
			continue
		}

		log.Infof("[Stream Adapter] Processing data line: %s", line)

		// 处理SSE格式: "data: {...}" �?"data:{...}"
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data) // 去掉可能的空�?

			// 检查是否是结束标记
			if data == "[DONE]" {
				fmt.Fprintf(writer, "data: [DONE]\n\n")
				flusher.Flush()
				totalTokens := totalPromptTokens + totalCompletionTokens
				s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalTokens, true, "")
				return nil
			}

			// 解析JSON
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				log.Warnf("Failed to parse chunk: %v, data: %s", err, data)
				continue
			}

			// 从原始chunk中提取token使用信息（适配器转换前�?
			// 根据反向适配器判断远端格�?
			if strings.HasPrefix(reverseAdapterName, "claude-") || reverseAdapterName == "anthropic" {
				// 远端�?Claude 格式
				if chunkType, ok := chunk["type"].(string); ok {
					switch chunkType {
					case "message_start":
						if message, ok := chunk["message"].(map[string]interface{}); ok {
							if usage, ok := message["usage"].(map[string]interface{}); ok {
								if inputTokens, ok := usage["input_tokens"].(float64); ok {
									totalPromptTokens = int(inputTokens)
								}
							}
						}
					case "message_delta":
						if delta, ok := chunk["delta"].(map[string]interface{}); ok {
							if usage, ok := delta["usage"].(map[string]interface{}); ok {
								if outputTokens, ok := usage["output_tokens"].(float64); ok {
									totalCompletionTokens = int(outputTokens)
								}
							}
						}
					}
				}
			} else if strings.HasPrefix(reverseAdapterName, "gemini-") || reverseAdapterName == "gemini" {
				// 远端�?Gemini 格式
				if usageMetadata, ok := chunk["usageMetadata"].(map[string]interface{}); ok {
					if promptTokens, ok := usageMetadata["promptTokenCount"].(float64); ok {
						totalPromptTokens = int(promptTokens)
					}
					if candidatesTokens, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
						totalCompletionTokens = int(candidatesTokens)
					}
				}
			}

			// 使用适配器转换chunk
			log.Infof("[Stream Adapter] Calling adapter.AdaptStreamChunk() for chunk type: %v", chunk["type"])
			adaptedChunk, err := adapter.AdaptStreamChunk(chunk)
			if err != nil {
				log.Warnf("[Stream Adapter] Failed to adapt chunk: %v", err)
				continue
			}

			log.Infof("[Stream Adapter] Adapter returned: adaptedChunk=%v (is nil: %v)", adaptedChunk, adaptedChunk == nil)

			// 只有�?adaptedChunk 不为 nil 时才发�?
			if adaptedChunk != nil {
				chunkCount++
				// 发送转换后的chunk
				adaptedData, _ := json.Marshal(adaptedChunk)
				log.Infof("[STREAM TO CLIENT] Chunk #%d: %s", chunkCount, string(adaptedData))
				fmt.Fprintf(writer, "data: %s\n\n", string(adaptedData))
				flusher.Flush()
			} else {
				log.Infof("[Stream Adapter] Adapted chunk is nil - skipping")
			}
		}
	}

	log.Infof("[Stream Adapter] Finished reading stream. Total chunks sent: %d", chunkCount)

	if err := scanner.Err(); err != nil {
		s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalPromptTokens+totalCompletionTokens, false, err.Error())
		return err
	}

	// 发送结束事�?
	endEvents := adapter.AdaptStreamEnd()
	for _, event := range endEvents {
		eventData, _ := json.Marshal(event)
		log.Infof("[STREAM TO CLIENT] %s", string(eventData))
		fmt.Fprintf(writer, "data: %s\n\n", string(eventData))
	}
	flusher.Flush()

	totalTokens := totalPromptTokens + totalCompletionTokens
	s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalTokens, true, "")
	return nil
}

// streamDirect 直接转发流式响应
func (s *ProxyService) streamDirect(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64) error {
	buf := make([]byte, 4096)
	var responseBuffer bytes.Buffer
	var bytesWritten int64

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			// 将数据写入缓冲区以便后续解析token使用信息
			responseBuffer.Write(buf[:n])
			bytesWritten += int64(n)

			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				log.Errorf("[Stream Direct] Failed to write to client: %v", writeErr)
				s.routeService.LogRequest(model, routeID, 0, 0, 0, false, writeErr.Error())
				return writeErr
			}
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				log.Debugf("[Stream Direct] Stream completed. Total bytes: %d", bytesWritten)

				// 尝试从响应中提取token使用信息
				responseStr := responseBuffer.String()
				log.Debugf("[Stream Direct] Response buffer length: %d bytes", len(responseStr))

				// 仅在debug模式下记录响应内容（前500字符）
				if len(responseStr) > 0 {
					previewLen := 500
					if len(responseStr) < previewLen {
						previewLen = len(responseStr)
					}
					log.Debugf("[Stream Direct] Response preview: %s", responseStr[:previewLen])
				}

				promptTokens, completionTokens := s.extractTokensFromStreamResponse(responseStr)
				totalTokens := promptTokens + completionTokens
				log.Infof("[Stream Direct] Extracted tokens: prompt=%d, completion=%d, total=%d", promptTokens, completionTokens, totalTokens)
				s.routeService.LogRequest(model, routeID, promptTokens, completionTokens, totalTokens, true, "")
				return nil
			}
			log.Errorf("[Stream Direct] Stream error: %v", err)
			s.routeService.LogRequest(model, routeID, 0, 0, 0, false, err.Error())
			return err
		}
	}
}

// streamOpenAIToClaude 将 OpenAI 流式响应转换为 Claude 流式响应
// 用于 /api/anthropic 路径，当目标是 OpenAI 格式 API 时
// 支持：普通文本、thinking（reasoning_content）、tool_calls
func (s *ProxyService) streamOpenAIToClaude(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64) error {
	// 发送 Claude 流式响应的开始事件
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	// message_start 事件
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	startData, _ := json.Marshal(messageStart)
	fmt.Fprintf(writer, "event: message_start\ndata: %s\n\n", string(startData))
	flusher.Flush()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	var totalPromptTokens int
	var totalCompletionTokens int

	// 用于跟踪当前 active 的 content_block 类型
	// 可能的值: "text", "thinking", "tool_use"
	var currentBlockType string
	var blockIndex int

	// 用于累积 tool_calls（OpenAI 流式发送 tool_calls 是分片的：先发 name，再分片发 arguments）
	var toolCallsMap = make(map[int]*partialToolCall)

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				break
			}

			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// 提取 token 使用信息
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					totalPromptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					totalCompletionTokens = int(ct)
				}
			}

			// 提取内容
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {

						// 优先级1: 检查 reasoning_content (thinking 内容)
						if reasoningContent, ok := delta["reasoning_content"].(string); ok && reasoningContent != "" {
							// 如果当前不是 thinking block，先停止之前的 block
							if currentBlockType != "" && currentBlockType != "thinking" {
								s.sendContentBlockStop(writer, flusher, blockIndex)
								blockIndex++
							}

							// 如果当前不是 thinking block，开始新的 thinking block
							if currentBlockType != "thinking" {
								s.sendContentBlockStart(writer, flusher, blockIndex, "thinking", "")
								currentBlockType = "thinking"
							}

							// 发送 thinking delta
							deltaEvent := map[string]interface{}{
								"type":  "content_block_delta",
								"index": blockIndex,
								"delta": map[string]interface{}{
									"type":     "thinking_delta",
									"thinking": reasoningContent,
								},
							}
							deltaData, _ := json.Marshal(deltaEvent)
							fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(deltaData))
							flusher.Flush()
							continue
						}

						// 优先级2: 检查 tool_calls
						if toolCalls, ok := delta["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
							// 如果当前不是 tool_use block，先停止之前的 block
							if currentBlockType != "" && currentBlockType != "tool_use" {
								s.sendContentBlockStop(writer, flusher, blockIndex)
								blockIndex++
							}

							// 处理 tool_calls
							for _, tc := range toolCalls {
								tcMap, ok := tc.(map[string]interface{})
								if !ok {
									continue
								}

								indexFloat, ok := tcMap["index"].(float64)
								if !ok {
									continue
								}
								tcIndex := int(indexFloat)

								// 初始化 partial tool call
								if toolCallsMap[tcIndex] == nil {
									toolCallsMap[tcIndex] = &partialToolCall{
										index:  tcIndex,
										id:     fmt.Sprintf("toolu_%d", time.Now().UnixNano()),
										name:   "",
										args:   "",
										fields: make(map[string]interface{}),
									}
								}

								pt := toolCallsMap[tcIndex]

								// 处理 tool_call.id
								if id, ok := tcMap["id"].(string); ok {
									pt.id = id
								}

								// 处理 tool_call.type (应该是 "function")
								if t, ok := tcMap["type"].(string); ok {
									pt.fields["type"] = t
								}

								// 处理 function.name
								if function, ok := tcMap["function"].(map[string]interface{}); ok {
									if name, ok := function["name"].(string); ok {
										if currentBlockType != "tool_use" {
											s.sendContentBlockStart(writer, flusher, blockIndex, "tool_use", pt.id)
											currentBlockType = "tool_use"

											// 发送 tool_use 的 name delta
											nameDelta := map[string]interface{}{
												"type":  "content_block_delta",
												"index": blockIndex,
												"delta": map[string]interface{}{
													"type":         "input_json_delta",
													"partial_json": fmt.Sprintf(`{"name":"%s","input":{}`, name),
												},
											}
											nameDeltaData, _ := json.Marshal(nameDelta)
											fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(nameDeltaData))
											flusher.Flush()
										}
										pt.name = name
									}

									// 处理 function.arguments (分片到达)
									if args, ok := function["arguments"].(string); ok {
										pt.args += args

										// 发送 arguments delta（跳过开始括号）
										if len(pt.args) > len(pt.name)+len(`{"name":"`)+len(`","input":{`) {
											argPart := pt.args[len(pt.name)+len(`{"name":"`)+len(`","input":{}`):]
											argsDelta := map[string]interface{}{
												"type":  "content_block_delta",
												"index": blockIndex,
												"delta": map[string]interface{}{
													"type":         "input_json_delta",
													"partial_json": argPart,
												},
											}
											argsDeltaData, _ := json.Marshal(argsDelta)
											fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(argsDeltaData))
											flusher.Flush()
										}
									}
								}
							}
							continue
						}

						// 优先级3: 检查普通 content 文本
						if content, ok := delta["content"].(string); ok && content != "" {
							// 如果当前不是 text block，需要先开始一个新的 text block
							if currentBlockType != "text" {
								// 先停止之前的 block（如果有）
								if currentBlockType != "" {
									s.sendContentBlockStop(writer, flusher, blockIndex)
									blockIndex++
								}
								// 开始新的 text block
								s.sendContentBlockStart(writer, flusher, blockIndex, "text", "")
								currentBlockType = "text"
							}

							// 发送 content_block_delta 事件
							deltaEvent := map[string]interface{}{
								"type":  "content_block_delta",
								"index": blockIndex,
								"delta": map[string]interface{}{
									"type": "text_delta",
									"text": content,
								},
							}
							deltaData, _ := json.Marshal(deltaEvent)
							fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(deltaData))
							flusher.Flush()
						}
					}

					// 检查是否结束
					if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
						// 如果是 tool_calls 结束，需要完成 tool_use block
						if finishReason == "tool_calls" && currentBlockType == "tool_use" {
							// 关闭 JSON 对象
							closeDelta := map[string]interface{}{
								"type":  "content_block_delta",
								"index": blockIndex,
								"delta": map[string]interface{}{
									"type":         "input_json_delta",
									"partial_json": "}}",
								},
							}
							closeData, _ := json.Marshal(closeDelta)
							fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(closeData))
							flusher.Flush()
						}
					}
				}
			}
		}
	}

	// 停止最后的 content block
	if currentBlockType != "" {
		s.sendContentBlockStop(writer, flusher, blockIndex)
	}

	// message_delta 事件
	messageDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": totalCompletionTokens,
		},
	}
	deltaData, _ := json.Marshal(messageDelta)
	fmt.Fprintf(writer, "event: message_delta\ndata: %s\n\n", string(deltaData))
	flusher.Flush()

	// message_stop 事件
	messageStop := map[string]interface{}{
		"type": "message_stop",
	}
	stopData, _ := json.Marshal(messageStop)
	fmt.Fprintf(writer, "event: message_stop\ndata: %s\n\n", string(stopData))
	flusher.Flush()

	// 记录请求
	totalTokens := totalPromptTokens + totalCompletionTokens
	s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalTokens, true, "")

	return nil
}

// FetchRemoteModels 获取远程模型列表
func (s *ProxyService) FetchRemoteModels(apiUrl, apiKey string) ([]string, error) {
	// 记录原始 URL 是否�?"/" 结尾
	hasTrailingSlash := strings.HasSuffix(apiUrl, "/")

	// 移除末尾的斜�?
	apiUrl = strings.TrimSuffix(apiUrl, "/")

	// 添加 http/https 前缀（如果没有）
	if !strings.HasPrefix(apiUrl, "http://") && !strings.HasPrefix(apiUrl, "https://") {
		apiUrl = "https://" + apiUrl
	}

	// 构建 URL：如果原�?URL 末尾�?"/"，说明用户已指定完整路径，只�?/models
	// 否则加上 /v1/models
	var url string
	if hasTrailingSlash {
		url = apiUrl + "/models"
	} else {
		url = apiUrl + "/v1/models"
	}
	log.Infof("Fetching models from: %s", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// 读取响应�?
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse JSON response: %v (body: %s)", err, string(body))
	}

	models := make([]string, len(result.Data))
	for i, m := range result.Data {
		models[i] = m.ID
	}

	log.Infof("Successfully fetched %d models", len(models))
	return models, nil
}

// detectAdapter 智能检测需要使用的适配�?
// 参数: route - 路由配置, requestFormat - 请求格式 (openai/claude/gemini)
// 基于路由的format字段和requestFormat进行智能判断
// 相同格式直接透传,不同格式才使用适配�?
func (s *ProxyService) detectAdapter(apiUrl, model string) string {
	// 保持向后兼容:如果是旧的URL检测逻辑,使用启发式检�?
	return s.detectAdapterByURL(apiUrl, model)
}

// detectAdapterForRoute 根据路由配置和请求格式智能检测适配�?
// requestFormat: "openai", "claude", "gemini"
// route.Format: 目标API的格�?
// 返回: 适配器名�?空字符串表示直接透传
func (s *ProxyService) detectAdapterForRoute(route *database.ModelRoute, requestFormat string) string {
	if route == nil {
		return ""
	}

	// 标准化请求格�?
	requestFormat = normalizeFormat(requestFormat)

	// 获取目标格式(路由配置的format)
	targetFormat := normalizeFormat(route.Format)
	if targetFormat == "" {
		// 如果路由没有明确指定format,根据URL和模型名推断
		targetFormat = inferFormatFromRoute(route.APIUrl, route.Model)
	}

	log.Infof("[Format Detection] Request=%s, Target=%s, Route=%s", requestFormat, targetFormat, route.Name)

	// 相同格式直接透传
	if requestFormat == targetFormat {
		log.Infof("[Format Match] Same format detected, using passthrough")
		return ""
	}

	// 不同格式需要转�?
	return getAdapterName(requestFormat, targetFormat)
}

// normalizeFormat 标准化格式名�?
func normalizeFormat(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "claude", "anthropic":
		return "claude"
	case "gemini", "google":
		return "gemini"
	case "openai", "gpt", "":
		return "openai"
	default:
		return "openai"
	}
}

// inferFormatFromRoute 从路由信息推断格�?
func inferFormatFromRoute(apiUrl, model string) string {
	lowerURL := strings.ToLower(apiUrl)
	lowerModel := strings.ToLower(model)

	// 基于URL判断
	if strings.Contains(lowerURL, "anthropic") || strings.Contains(lowerURL, "claude") {
		return "claude"
	}
	if strings.Contains(lowerURL, "gemini") || strings.Contains(lowerURL, "googleapis.com") {
		return "gemini"
	}

	// 基于模型名判�?
	if strings.HasPrefix(lowerModel, "claude-") {
		return "claude"
	}
	if strings.HasPrefix(lowerModel, "gemini-") {
		return "gemini"
	}

	// 默认�?OpenAI 格式
	return "openai"
}

// getAdapterName 根据源格式和目标格式返回适配器名�?
func getAdapterName(requestFormat, targetFormat string) string {
	key := requestFormat + "->" + targetFormat
	switch key {
	case "openai->claude":
		return "openai-to-claude"
	case "openai->gemini":
		return "openai-to-gemini"
	case "claude->openai":
		return "claude-to-openai"
	case "gemini->openai":
		return "gemini-to-openai"
	case "claude->gemini":
		return "claude-to-gemini"
	case "gemini->claude":
		return "gemini-to-claude"
	default:
		log.Warnf("[Adapter] Unsupported conversion: %s", key)
		return ""
	}
}

// getReverseAdapterName 获取反向适配器名称（用于响应转换�?
// 例如：请求用 openai-to-claude，响应应该用 claude-to-openai
func getReverseAdapterName(adapterName string) string {
	switch adapterName {
	case "openai-to-claude":
		return "claude-to-openai"
	case "claude-to-openai":
		return "openai-to-claude"
	case "openai-to-gemini":
		return "gemini-to-openai"
	case "gemini-to-openai":
		return "openai-to-gemini"
	case "claude-to-gemini":
		return "gemini-to-claude"
	case "gemini-to-claude":
		return "claude-to-gemini"
	case "anthropic":
		// 旧的 anthropic 适配器名称，映射�?claude-to-openai
		return "claude-to-openai"
	case "gemini":
		// 旧的 gemini 适配器名称，映射�?gemini-to-openai
		return "gemini-to-openai"
	default:
		log.Warnf("[Adapter] No reverse adapter for: %s", adapterName)
		return ""
	}
}

// detectAdapterByURL 旧的基于URL的启发式检测逻辑(向后兼容)
func (s *ProxyService) detectAdapterByURL(apiUrl, model string) string {
	lowerURL := strings.ToLower(apiUrl)
	lowerModel := strings.ToLower(model)

	// 先检查是否是标准�?OpenAI 格式 API 端点，如果是，则不应用任何适配�?
	// 这样可以避免�?Gemini 适配器错误地应用�?OpenAI 兼容�?API �?
	if isStandardOpenAIEndpoint(lowerURL) {
		return "" // 对标�?OpenAI 端点不使用适配�?
	}

	// 精确内容检�?- 基于API URL和模型名称中的关键词
	// 使用更精确的匹配避免误匹配，例如避免 "glm" �?"gemini" 的混�?
	if containsExactWord(lowerURL, "anthropic") || containsExactWord(lowerModel, "claude") {
		return "anthropic"
	}

	// 使用更严格的匹配来检�?Gemini，避�?"glm" �?"gemini" 等误匹配
	if containsExactWord(lowerURL, "gemini") || containsExactWord(lowerModel, "gemini") {
		return "gemini"
	}

	if containsExactWord(lowerURL, "deepseek") || containsExactWord(lowerModel, "deepseek") {
		return "deepseek"
	}

	return "" // 不需要适配�?
}

// buildAdapterURL 构建适配�?URL
func (s *ProxyService) buildAdapterURL(apiURL, adapterName, model string) string {
	switch adapterName {
	case "anthropic", "openai-to-claude":
		return buildClaudeMessagesURL(apiURL)
	case "gemini", "openai-to-gemini":
		// Gemini 使用不同�?URL 格式
		if strings.HasSuffix(apiURL, "/") {
			// 末尾有斜杠，去掉/v1（如果存在）然后添加models路径
			if strings.Contains(apiURL, "/v1/") {
				// 去掉/v1/部分
				baseUrl := strings.Replace(apiURL, "/v1/", "/", 1)
				return fmt.Sprintf("%smodels/%s:generateContent", baseUrl, model)
			} else if strings.HasSuffix(apiURL, "/v1") {
				// 去掉末尾�?v1
				baseUrl := strings.TrimSuffix(apiURL, "/v1")
				return fmt.Sprintf("%s/models/%s:generateContent", baseUrl, model)
			}
			// 末尾有斜杠但没有/v1，直接使用当前路�?
			return fmt.Sprintf("%smodels/%s:generateContent", apiURL, model)
		}
		return fmt.Sprintf("%s/v1/models/%s:generateContent", apiURL, model)
	case "deepseek":
		return buildOpenAIChatURL(apiURL)
	default:
		return buildOpenAIChatURL(apiURL)
	}
}

// buildAdapterStreamURL 构建适配器流�?URL
func (s *ProxyService) buildAdapterStreamURL(apiURL, adapterName, model string) string {
	switch adapterName {
	case "anthropic", "openai-to-claude":
		return buildClaudeMessagesURL(apiURL)
	case "gemini", "openai-to-gemini":
		// Gemini 使用不同�?URL 格式
		if strings.HasSuffix(apiURL, "/") {
			// 末尾有斜杠，去掉/v1（如果存在）然后添加models路径
			if strings.Contains(apiURL, "/v1/") {
				// 去掉/v1/部分
				baseUrl := strings.Replace(apiURL, "/v1/", "/", 1)
				return fmt.Sprintf("%smodels/%s:streamGenerateContent", baseUrl, model)
			} else if strings.HasSuffix(apiURL, "/v1") {
				// 去掉末尾�?v1
				baseUrl := strings.TrimSuffix(apiURL, "/v1")
				return fmt.Sprintf("%s/models/%s:streamGenerateContent", baseUrl, model)
			}
			// 末尾有斜杠但没有/v1，直接使用当前路�?
			return fmt.Sprintf("%smodels/%s:streamGenerateContent", apiURL, model)
		}
		return fmt.Sprintf("%s/v1/models/%s:streamGenerateContent", apiURL, model)
	case "deepseek":
		return buildOpenAIChatURL(apiURL)
	default:
		return buildOpenAIChatURL(apiURL)
	}
}

// isStandardOpenAIEndpoint 检�?URL 是否为标准的 OpenAI API 端点
// 如果是，则不应该应用任何适配器转�?
func isStandardOpenAIEndpoint(url string) bool {
	// 检查是否包含标准的 OpenAI API 路径
	if containsExactWord(url, "/v1/chat/completions") ||
		containsExactWord(url, "/v1/completions") ||
		containsExactWord(url, "/v1/embeddings") ||
		containsExactWord(url, "/v1/images/generations") ||
		containsExactWord(url, "/v1/audio/transcriptions") ||
		containsExactWord(url, "/v1/audio/speech") {
		return true
	}

	// 检查常见的 OpenAI 兼容 API 基础路径
	if containsExactWord(url, "openai.com") ||
		containsExactWord(url, "api.openai.com") ||
		containsExactWord(url, "/openai/v1") ||
		containsExactWord(url, "/v1") { // 这太宽泛了，需要更具体的检�?
		// 对于 /v1 路径，需要更具体的检查，仅在确实包含 chat/completions 时才认为�?OpenAI API
		if strings.Contains(url, "/v1/chat/completions") ||
			strings.Contains(url, "/v1/completions") {
			return true
		}
	}

	return false
}

// containsExactWord 检�?needle 是否作为一个独立的单词存在�?haystack �?
// 通过检查边界字符来确保精确匹配，避免子串误匹配
func containsExactWord(haystack, needle string) bool {
	if haystack == needle {
		return true
	}

	// 使用更精确的查找方法，确�?needle 前后是边界或分隔�?
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			// 检查前面的字符（如果存在）是否为非字母数字字符
			prevIsBoundary := i == 0 || !isAlphanumeric(rune(haystack[i-1]))
			// 检查后面的字符（如果存在）是否为非字母数字字符
			nextIsBoundary := i+len(needle) == len(haystack) || !isAlphanumeric(rune(haystack[i+len(needle)]))

			if prevIsBoundary && nextIsBoundary {
				return true
			}
		}
	}

	return false
}

// isAlphanumeric 检查字符是否为字母或数�?
func isAlphanumeric(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// buildOpenAIChatURL 智能构建 OpenAI chat completions URL
// 如果 apiUrl 末尾�?/，则不添�?/v1 前缀（用于兼容如智谱等非标准路径�?API�?
// 例如�?
//   - https://api.openai.com/v1 -> https://api.openai.com/v1/chat/completions
//   - https://open.bigmodel.cn/api/coding/paas/v4/ -> https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
//   - https://api.kkyyxx.xyz -> https://api.kkyyxx.xyz/v1/chat/completions
func buildOpenAIChatURL(apiUrl string) string {
	if strings.HasSuffix(apiUrl, "/") {
		// 末尾有斜杠，说明是固定API路径，直接添�?chat/completions
		return apiUrl + "chat/completions"
	}
	// 末尾没有斜杠，是标准API，需要添�?/v1/chat/completions
	return apiUrl + "/v1/chat/completions"
}

// buildClaudeMessagesURL 智能构建 Claude messages URL
func buildClaudeMessagesURL(apiUrl string) string {
	if strings.HasSuffix(apiUrl, "/") {
		// 末尾有斜杠，去掉/v1（如果存在）然后添加messages
		if strings.Contains(apiUrl, "/v1/") {
			// 去掉/v1/部分
			baseUrl := strings.Replace(apiUrl, "/v1/", "/", 1)
			return baseUrl + "messages"
		} else if strings.HasSuffix(apiUrl, "/v1") {
			// 去掉末尾�?v1
			baseUrl := strings.TrimSuffix(apiUrl, "/v1")
			return baseUrl + "/messages"
		}
		// 末尾有斜杠但没有/v1，直接使用当前路�?+ messages
		return apiUrl + "messages"
	}
	return apiUrl + "/v1/messages"
}

// convertOpenAIToAnthropicResponse �?OpenAI 格式响应转换�?Anthropic 格式
func (s *ProxyService) convertOpenAIToAnthropicResponse(openaiResp map[string]interface{}) map[string]interface{} {
	// OpenAI 响应格式示例:
	// {
	//   "id": "chatcmpl-...",
	//   "object": "chat.completion",
	//   "created": 1234567890,
	//   "model": "gpt-3.5-turbo",
	//   "choices": [
	//     {
	//       "index": 0,
	//       "message": {
	//         "role": "assistant",
	//         "content": "Hello! How can I help you?"
	//       },
	//       "finish_reason": "stop"
	//     }
	//   ],
	//   "usage": {
	//     "prompt_tokens": 10,
	//     "completion_tokens": 20,
	//     "total_tokens": 30
	//   }
	// }
	//
	// Anthropic 响应格式示例:
	// {
	//   "id": "msg_...",
	//   "type": "message",
	//   "role": "assistant",
	//   "content": [
	//     {
	//       "type": "text",
	//       "text": "Hello! How can I help you?"
	//     }
	//   ],
	//   "model": "claude-3-haiku-20240307",
	//   "stop_reason": "end_turn",
	//   "usage": {
	//     "input_tokens": 10,
	//     "output_tokens": 20
	//   }
	// }

	anthropicResp := make(map[string]interface{})

	// 复制 ID
	if id, ok := openaiResp["id"].(string); ok {
		anthropicResp["id"] = id
	} else {
		anthropicResp["id"] = "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}

	// 设置类型
	anthropicResp["type"] = "message"

	// 设置角色
	anthropicResp["role"] = "assistant"

	// 复制模型�?
	if model, ok := openaiResp["model"].(string); ok {
		anthropicResp["model"] = model
	}

	// 转换 content - �?choices[0].message.content �?content[{type, text}]
	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					anthropicResp["content"] = []map[string]interface{}{
						{
							"type": "text",
							"text": content,
						},
					}
				}
			}

			// 转换 finish_reason �?stop_reason
			if finishReason, ok := choice["finish_reason"].(string); ok {
				switch finishReason {
				case "stop":
					anthropicResp["stop_reason"] = "end_turn"
				case "length":
					anthropicResp["stop_reason"] = "max_tokens"
				default:
					anthropicResp["stop_reason"] = finishReason
				}
			}
		}
	}

	// 转换 usage - �?{prompt_tokens, completion_tokens, total_tokens} �?{input_tokens, output_tokens}
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		anthropicUsage := make(map[string]interface{})
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			anthropicUsage["input_tokens"] = int(promptTokens)
		}
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			anthropicUsage["output_tokens"] = int(completionTokens)
		}
		anthropicResp["usage"] = anthropicUsage
	}

	log.Infof("Converted OpenAI response to Anthropic format: %+v", anthropicResp)
	return anthropicResp
}

// convertClaudeToOpenAIResponse �?Claude 格式响应转换�?OpenAI 格式
func (s *ProxyService) convertClaudeToOpenAIResponse(claudeResp map[string]interface{}) map[string]interface{} {
	openaiResp := make(map[string]interface{})

	// 基本字段
	if id, ok := claudeResp["id"].(string); ok {
		openaiResp["id"] = id
	} else {
		openaiResp["id"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	openaiResp["object"] = "chat.completion"
	openaiResp["created"] = time.Now().Unix()

	if model, ok := claudeResp["model"].(string); ok {
		openaiResp["model"] = model
	}

	// 转换 content
	var content string
	if contentArray, ok := claudeResp["content"].([]interface{}); ok {
		for _, item := range contentArray {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemType, ok := itemMap["type"].(string); ok && itemType == "text" {
					if text, ok := itemMap["text"].(string); ok {
						content += text
					}
				}
			}
		}
	}

	// 转换 stop_reason
	finishReason := "stop"
	if stopReason, ok := claudeResp["stop_reason"].(string); ok {
		switch stopReason {
		case "end_turn":
			finishReason = "stop"
		case "max_tokens":
			finishReason = "length"
		case "tool_use":
			finishReason = "tool_calls"
		default:
			finishReason = stopReason
		}
	}

	openaiResp["choices"] = []interface{}{
		map[string]interface{}{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": finishReason,
		},
	}

	// 转换 usage
	if usage, ok := claudeResp["usage"].(map[string]interface{}); ok {
		openaiUsage := make(map[string]interface{})
		if inputTokens, ok := usage["input_tokens"].(float64); ok {
			openaiUsage["prompt_tokens"] = int(inputTokens)
		}
		if outputTokens, ok := usage["output_tokens"].(float64); ok {
			openaiUsage["completion_tokens"] = int(outputTokens)
		}
		if promptTokens, ok := openaiUsage["prompt_tokens"].(int); ok {
			if completionTokens, ok := openaiUsage["completion_tokens"].(int); ok {
				openaiUsage["total_tokens"] = promptTokens + completionTokens
			}
		}
		openaiResp["usage"] = openaiUsage
	}

	return openaiResp
}

// convertOpenAIToGeminiResponse 将 OpenAI 格式响应转换为 Gemini 格式
// 使用 Google 官方 Gemini API 响应格式，包装为 APIMart 格式
func (s *ProxyService) convertOpenAIToGeminiResponse(openaiResp map[string]interface{}) map[string]interface{} {
	geminiData := make(map[string]interface{})

	// 转换 choices 为 candidates
	var text string
	var finishReason string
	var toolCalls []interface{}

	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					text = content
				}
				// 提取 tool_calls
				if tc, ok := message["tool_calls"].([]interface{}); ok {
					toolCalls = tc
				}
			}
			if fr, ok := choice["finish_reason"].(string); ok {
				switch fr {
				case "stop":
					finishReason = "STOP"
				case "length":
					finishReason = "MAX_TOKENS"
				case "tool_calls":
					finishReason = "STOP" // Gemini 使用 STOP，工具调用通过 functionCall 表示
				default:
					finishReason = "STOP"
				}
			}
		}
	}

	// 构建 parts
	var parts []interface{}

	// 如果有文本内容，添加 text part
	if text != "" {
		parts = append(parts, map[string]interface{}{
			"text": text,
		})
	}

	// 如果有 tool_calls，转换为 Gemini 的 functionCall 格式
	if len(toolCalls) > 0 {
		for _, tc := range toolCalls {
			if toolCall, ok := tc.(map[string]interface{}); ok {
				if function, ok := toolCall["function"].(map[string]interface{}); ok {
					name, _ := function["name"].(string)
					argsStr, _ := function["arguments"].(string)

					// 解析 arguments JSON 字符串
					var args map[string]interface{}
					if argsStr != "" {
						json.Unmarshal([]byte(argsStr), &args)
					}
					if args == nil {
						args = make(map[string]interface{})
					}

					parts = append(parts, map[string]interface{}{
						"functionCall": map[string]interface{}{
							"name": name,
							"args": args,
						},
					})
				}
			}
		}
	}

	// 如果没有任何 parts，添加空文本
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{
			"text": "",
		})
	}

	geminiData["candidates"] = []interface{}{
		map[string]interface{}{
			"content": map[string]interface{}{
				"role":  "model",
				"parts": parts,
			},
			"finishReason": finishReason,
			"index":        0,
			"safetyRatings": []interface{}{
				map[string]interface{}{
					"category":    "HARM_CATEGORY_HATE_SPEECH",
					"probability": "NEGLIGIBLE",
				},
			},
		},
	}

	// 添加 promptFeedback
	geminiData["promptFeedback"] = map[string]interface{}{
		"safetyRatings": []interface{}{
			map[string]interface{}{
				"category":    "HARM_CATEGORY_HATE_SPEECH",
				"probability": "NEGLIGIBLE",
			},
		},
	}

	// 转换 usage 为 usageMetadata
	usageMetadata := make(map[string]interface{})
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			usageMetadata["promptTokenCount"] = int(promptTokens)
		} else if promptTokens, ok := usage["prompt_tokens"].(int); ok {
			usageMetadata["promptTokenCount"] = promptTokens
		}
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			usageMetadata["candidatesTokenCount"] = int(completionTokens)
		} else if completionTokens, ok := usage["completion_tokens"].(int); ok {
			usageMetadata["candidatesTokenCount"] = completionTokens
		}
		if totalTokens, ok := usage["total_tokens"].(float64); ok {
			usageMetadata["totalTokenCount"] = int(totalTokens)
		} else if totalTokens, ok := usage["total_tokens"].(int); ok {
			usageMetadata["totalTokenCount"] = totalTokens
		}
	}
	geminiData["usageMetadata"] = usageMetadata

	// 包装为 APIMart 格式: {"code": 200, "data": {...}}
	geminiResp := map[string]interface{}{
		"code": 200,
		"data": geminiData,
	}

	return geminiResp
}

// extractTokensFromStreamResponse 从流式响应中提取token使用信息（支�?OpenAI �?Claude 格式�?
func (s *ProxyService) extractTokensFromStreamResponse(response string) (promptTokens, completionTokens int) {
	// 将响应按行分�?
	lines := strings.Split(response, "\n")

	for _, line := range lines {
		// 处理SSE格式: "data: {...}" �?"data:{...}"
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)

			// 跳过结束标记
			if data == "[DONE]" {
				continue
			}

			// 解析JSON
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// OpenAI 格式: usage.prompt_tokens, usage.completion_tokens
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok && promptTokens == 0 {
					promptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					completionTokens = int(ct) // 使用最后一个�?
				}
			}

			// Claude 格式: message_start.message.usage.input_tokens
			if chunkType, ok := chunk["type"].(string); ok {
				switch chunkType {
				case "message_start":
					if message, ok := chunk["message"].(map[string]interface{}); ok {
						if usage, ok := message["usage"].(map[string]interface{}); ok {
							if inputTokens, ok := usage["input_tokens"].(float64); ok {
								promptTokens = int(inputTokens)
							}
						}
					}
				case "message_delta":
					if usage, ok := chunk["usage"].(map[string]interface{}); ok {
						if outputTokens, ok := usage["output_tokens"].(float64); ok {
							completionTokens = int(outputTokens)
						}
					}
				}
			}
		}
	}

	return promptTokens, completionTokens
}

// ProxyGeminiRequest 代理 Gemini 格式的非流式请求
// 请求来自 /api/v1/gemini/models/{model}:generateContent
func (s *ProxyService) ProxyGeminiRequest(requestBody []byte, headers map[string]string) ([]byte, int, error) {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("'model' field is required")
	}

	// 首先检查是否是重定向关键字
	res, err := s.resolveTargets(model)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return nil, http.StatusNotFound, fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	var needConvertResponse string

	resp, _, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		targetFormat := normalizeFormat(target.Format)
		if targetFormat == "" {
			targetFormat = inferFormatFromRoute(target.APIUrl, target.Model)
		}

		var transformedBody []byte
		var targetURL string
		needConvertResponse = "none"

		switch targetFormat {
		case "gemini":
			transformedBody = requestBody
			targetURL = fmt.Sprintf("%s/v1beta/models/%s:generateContent", cleanAPIUrl, model)
		case "openai":
			adapter := adapters.GetAdapter("gemini-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("gemini-to-openai adapter not found")
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
			needConvertResponse = "openai"
		case "claude":
			geminiToOpenAI := adapters.GetAdapter("gemini-to-openai")
			if geminiToOpenAI == nil {
				return nil, fmt.Errorf("gemini-to-openai adapter not found")
			}
			openaiReq, aerr := geminiToOpenAI.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, fmt.Errorf("gemini-to-openai conversion failed: %v", aerr)
			}
			openaiToClaude := adapters.GetAdapter("openai-to-claude")
			if openaiToClaude == nil {
				return nil, fmt.Errorf("openai-to-claude adapter not found")
			}
			claudeReq, aerr := openaiToClaude.AdaptRequest(openaiReq, model)
			if aerr != nil {
				return nil, fmt.Errorf("openai-to-claude conversion failed: %v", aerr)
			}
			transformedBody, _ = json.Marshal(claudeReq)
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
			needConvertResponse = "claude"
		default:
			return nil, fmt.Errorf("unsupported target format: %s", targetFormat)
		}

		log.Infof("[Gemini] %s -> %s (endpoint: %s, target format: %s)", model, targetURL, target.Name, targetFormat)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			switch targetFormat {
			case "claude":
				proxyReq.Header.Set("x-api-key", target.APIKey)
				proxyReq.Header.Set("anthropic-version", "2023-06-01")
			case "gemini":
				proxyReq.Header.Set("x-goog-api-key", target.APIKey)
			default:
				proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
			}
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return nil, http.StatusServiceUnavailable, ferr
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// 根据需要转换响应
	if resp.StatusCode == http.StatusOK && needConvertResponse != "none" {
		var respData map[string]interface{}
		if err := json.Unmarshal(responseBody, &respData); err == nil {
			log.Infof("[Gemini Request] Original response: %s", string(responseBody))
			switch needConvertResponse {
			case "openai":
				// OpenAI -> Gemini
				log.Infof("[Gemini Request] Converting OpenAI response to Gemini format")
				geminiResp := s.convertOpenAIToGeminiResponse(respData)
				if convertedBody, err := json.Marshal(geminiResp); err == nil {
					log.Infof("[Gemini Request] Converted Gemini response: %s", string(convertedBody))
					return convertedBody, resp.StatusCode, nil
				} else {
					log.Errorf("[Gemini Request] Failed to marshal Gemini response: %v", err)
				}
			case "claude":
				// Claude -> OpenAI -> Gemini
				log.Infof("[Gemini Request] Converting Claude response to Gemini format")
				// 先将 Claude 转换为 OpenAI
				openaiResp := s.convertClaudeToOpenAIResponse(respData)
				// 再将 OpenAI 转换为 Gemini
				geminiResp := s.convertOpenAIToGeminiResponse(openaiResp)
				if convertedBody, err := json.Marshal(geminiResp); err == nil {
					log.Infof("[Gemini Request] Converted Gemini response: %s", string(convertedBody))
					return convertedBody, resp.StatusCode, nil
				} else {
					log.Errorf("[Gemini Request] Failed to marshal Gemini response: %v", err)
				}
			}
		} else {
			log.Errorf("[Gemini Request] Failed to unmarshal response: %v", err)
		}
	}

	log.Infof("[Gemini Request] Returning original response (no conversion or conversion failed)")
	return responseBody, resp.StatusCode, nil
}

// ProxyGeminiStreamRequest 代理 Gemini 格式的流式请求
// 请求来自 /api/v1/gemini/models/{model}:streamGenerateContent
func (s *ProxyService) ProxyGeminiStreamRequest(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(model)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	var responseConversionType string

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		targetFormat := normalizeFormat(target.Format)
		if targetFormat == "" {
			targetFormat = inferFormatFromRoute(target.APIUrl, target.Model)
		}

		var transformedBody []byte
		var targetURL string
		responseConversionType = "none"

		switch targetFormat {
		case "gemini":
			transformedBody = requestBody
			targetURL = fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", cleanAPIUrl, model)
		case "openai":
			adapter := adapters.GetAdapter("gemini-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("gemini-to-openai adapter not found")
			}
			reqData["stream"] = true
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
			responseConversionType = "openai-to-gemini"
		case "claude":
			geminiToOpenAI := adapters.GetAdapter("gemini-to-openai")
			if geminiToOpenAI == nil {
				return nil, fmt.Errorf("gemini-to-openai adapter not found")
			}
			reqData["stream"] = true
			openaiReq, aerr := geminiToOpenAI.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, fmt.Errorf("gemini-to-openai conversion failed: %v", aerr)
			}
			openaiToClaude := adapters.GetAdapter("openai-to-claude")
			if openaiToClaude == nil {
				return nil, fmt.Errorf("openai-to-claude adapter not found")
			}
			claudeReq, aerr := openaiToClaude.AdaptRequest(openaiReq, model)
			if aerr != nil {
				return nil, fmt.Errorf("openai-to-claude conversion failed: %v", aerr)
			}
			transformedBody, _ = json.Marshal(claudeReq)
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
			responseConversionType = "claude-to-gemini"
		default:
			return nil, fmt.Errorf("unsupported target format: %s", targetFormat)
		}

		log.Infof("[Gemini Stream] %s -> %s (endpoint: %s, target format: %s)", model, targetURL, target.Name, targetFormat)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		proxyReq.Header.Set("Accept", "text/event-stream")
		if target.APIKey != "" {
			switch targetFormat {
			case "claude":
				proxyReq.Header.Set("x-api-key", target.APIKey)
				proxyReq.Header.Set("anthropic-version", "2023-06-01")
			case "gemini":
				proxyReq.Header.Set("x-goog-api-key", target.APIKey)
			default:
				proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
			}
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	// 检查响应状�?
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 根据响应转换类型来处理流
	switch responseConversionType {
	case "openai-to-gemini":
		// �?OpenAI 流式响应转换�?Gemini 流式响应
		log.Infof("[Gemini Stream] Converting OpenAI stream response to Gemini format")
		return s.streamOpenAIToGemini(resp.Body, writer, flusher, model, route.ID)
	case "claude-to-gemini":
		// �?Claude 流式响应转换�?Gemini 流式响应
		log.Infof("[Gemini Stream] Converting Claude stream response to Gemini format")
		return s.streamClaudeToGemini(resp.Body, writer, flusher, model, route.ID)
	default:
		// 直接转发流式响应
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			writer.Write(line)
			flusher.Flush()
		}
		return nil
	}
}

// streamOpenAIToGemini 将 OpenAI 流式响应转换为 Gemini 流式响应
func (s *ProxyService) streamOpenAIToGemini(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64) error {
	log.Infof("[OpenAI->Gemini Stream] Starting conversion for model: %s", model)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	var totalPromptTokens int
	var totalCompletionTokens int
	var chunkCount int

	// 用于累积 tool_calls（OpenAI 流式发送 tool_calls 是分片的：先发 name，再分片发 arguments）
	type toolCallAccumulator struct {
		ID        string
		Name      string
		Arguments string
	}
	toolCallsMap := make(map[int]*toolCallAccumulator) // key 是 tool_call 的 index

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				log.Infof("[OpenAI->Gemini Stream] Received [DONE], total chunks: %d", chunkCount)
				break
			}

			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				log.Warnf("[OpenAI->Gemini Stream] Failed to parse chunk: %v", err)
				continue
			}

			// 提取 token 使用信息
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					totalPromptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					totalCompletionTokens = int(ct)
				}
			}

			// 提取内容并转换为 Gemini 格式
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						// 处理文本内容
						if content, ok := delta["content"].(string); ok && content != "" {
							chunkCount++
							// 构建 Google 官方 Gemini 格式的流式响应
							geminiChunk := map[string]interface{}{
								"candidates": []interface{}{
									map[string]interface{}{
										"content": map[string]interface{}{
											"role": "model",
											"parts": []interface{}{
												map[string]interface{}{
													"text": content,
												},
											},
										},
										"index": 0,
									},
								},
							}

							chunkData, _ := json.Marshal(geminiChunk)
							log.Debugf("[OpenAI->Gemini Stream] Chunk #%d: %s", chunkCount, string(chunkData))
							fmt.Fprintf(writer, "data: %s\n\n", string(chunkData))
							flusher.Flush()
						}

						// 处理 tool_calls - 累积分片数据
						if toolCalls, ok := delta["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
							for _, tc := range toolCalls {
								if toolCall, ok := tc.(map[string]interface{}); ok {
									// 获取 tool_call 的 index
									tcIndex := 0
									if idx, ok := toolCall["index"].(float64); ok {
										tcIndex = int(idx)
									}

									// 初始化累积器
									if toolCallsMap[tcIndex] == nil {
										toolCallsMap[tcIndex] = &toolCallAccumulator{}
									}
									acc := toolCallsMap[tcIndex]

									// 提取 id
									if id, ok := toolCall["id"].(string); ok && id != "" {
										acc.ID = id
									}

									// 提取 function 信息
									if function, ok := toolCall["function"].(map[string]interface{}); ok {
										if name, ok := function["name"].(string); ok && name != "" {
											acc.Name = name
											log.Infof("[OpenAI->Gemini Stream] Tool call #%d name: %s", tcIndex, name)
										}
										if argsFragment, ok := function["arguments"].(string); ok {
											acc.Arguments += argsFragment
										}
									}
								}
							}
						}
					}

					// 检查是否结束
					if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
						log.Infof("[OpenAI->Gemini Stream] Finish reason: %s", finishReason)

						// 如果是 tool_calls 结束，先发送累积的 tool_calls
						if finishReason == "tool_calls" && len(toolCallsMap) > 0 {
							var functionCallParts []interface{}
							for idx := 0; idx < len(toolCallsMap); idx++ {
								acc := toolCallsMap[idx]
								if acc != nil && acc.Name != "" {
									var args map[string]interface{}
									if acc.Arguments != "" {
										if err := json.Unmarshal([]byte(acc.Arguments), &args); err != nil {
											log.Warnf("[OpenAI->Gemini Stream] Failed to parse tool_call arguments: %v", err)
											args = make(map[string]interface{})
										}
									}
									if args == nil {
										args = make(map[string]interface{})
									}

									functionCallParts = append(functionCallParts, map[string]interface{}{
										"functionCall": map[string]interface{}{
											"name": acc.Name,
											"args": args,
										},
									})
									log.Infof("[OpenAI->Gemini Stream] Sending tool call: name=%s, args=%s", acc.Name, acc.Arguments)
								}
							}

							if len(functionCallParts) > 0 {
								chunkCount++
								geminiChunk := map[string]interface{}{
									"candidates": []interface{}{
										map[string]interface{}{
											"content": map[string]interface{}{
												"role":  "model",
												"parts": functionCallParts,
											},
											"index": 0,
										},
									},
								}

								chunkData, _ := json.Marshal(geminiChunk)
								log.Infof("[OpenAI->Gemini Stream] Tool calls chunk: %s", string(chunkData))
								fmt.Fprintf(writer, "data: %s\n\n", string(chunkData))
								flusher.Flush()
							}
						}

						// 发送带有 finishReason 的最终块（包装为 APIMart 格式）
						geminiData := map[string]interface{}{
							"candidates": []interface{}{
								map[string]interface{}{
									"finishReason": "STOP",
									"index":        0,
									"content": map[string]interface{}{
										"role":  "model",
										"parts": []interface{}{},
									},
									"safetyRatings": []interface{}{
										map[string]interface{}{
											"category":    "HARM_CATEGORY_HATE_SPEECH",
											"probability": "NEGLIGIBLE",
										},
									},
								},
							},
							"usageMetadata": map[string]interface{}{
								"promptTokenCount":     totalPromptTokens,
								"candidatesTokenCount": totalCompletionTokens,
								"totalTokenCount":      totalPromptTokens + totalCompletionTokens,
							},
						}
						geminiChunk := map[string]interface{}{
							"code": 200,
							"data": geminiData,
						}

						chunkData, _ := json.Marshal(geminiChunk)
						log.Infof("[OpenAI->Gemini Stream] Final chunk: %s", string(chunkData))
						fmt.Fprintf(writer, "data: %s\n\n", string(chunkData))
						flusher.Flush()
					}
				}
			}
		}
	}

	// 记录请求
	totalTokens := totalPromptTokens + totalCompletionTokens
	log.Infof("[OpenAI->Gemini Stream] Completed: promptTokens=%d, completionTokens=%d, totalTokens=%d", totalPromptTokens, totalCompletionTokens, totalTokens)
	s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalTokens, true, "")

	return nil
}

// streamClaudeToGemini 将 Claude 流式响应转换为 Gemini 流式响应
func (s *ProxyService) streamClaudeToGemini(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64) error {
	log.Infof("[Claude->Gemini Stream] Starting conversion for model: %s", model)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	var totalInputTokens int
	var totalOutputTokens int
	var chunkCount int

	for scanner.Scan() {
		line := scanner.Text()

		// 跳过空行和事件行
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}

		log.Infof("[Claude->Gemini Stream] Processing line: %s", line)

		// Claude SSE 格式: "data: {...}" 或 "data:{...}"
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data) // 去掉可能的空格

			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				log.Warnf("[Claude->Gemini Stream] Failed to parse JSON: %v, data: %s", err, data)
				continue
			}

			eventType, _ := event["type"].(string)
			log.Infof("[Claude->Gemini Stream] Event type: %s", eventType)

			switch eventType {
			case "message_start":
				// 提取 input_tokens
				if message, ok := event["message"].(map[string]interface{}); ok {
					if usage, ok := message["usage"].(map[string]interface{}); ok {
						if inputTokens, ok := usage["input_tokens"].(float64); ok {
							totalInputTokens = int(inputTokens)
						}
					}
				}

			case "content_block_delta":
				// 提取文本内容
				if delta, ok := event["delta"].(map[string]interface{}); ok {
					if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
						if text, ok := delta["text"].(string); ok && text != "" {
							chunkCount++
							log.Infof("[Claude->Gemini Stream] Converting text chunk #%d: %s", chunkCount, text)

							// 构建 Gemini 格式的流式响应（包装为 APIMart 格式）
							geminiData := map[string]interface{}{
								"candidates": []interface{}{
									map[string]interface{}{
										"content": map[string]interface{}{
											"role": "model",
											"parts": []interface{}{
												map[string]interface{}{
													"text": text,
												},
											},
										},
										"index": 0,
									},
								},
							}
							geminiChunk := map[string]interface{}{
								"code": 200,
								"data": geminiData,
							}

							chunkData, _ := json.Marshal(geminiChunk)
							log.Infof("[Claude->Gemini Stream] Sending to client: %s", string(chunkData))
							fmt.Fprintf(writer, "data: %s\n\n", string(chunkData))
							flusher.Flush()
						}
					}
				}

			case "message_delta":
				// 提取 output_tokens
				if usage, ok := event["usage"].(map[string]interface{}); ok {
					if outputTokens, ok := usage["output_tokens"].(float64); ok {
						totalOutputTokens = int(outputTokens)
					}
				}

			case "message_stop":
				// 发送带有 finishReason 的最终块（包装为 APIMart 格式）
				geminiData := map[string]interface{}{
					"candidates": []interface{}{
						map[string]interface{}{
							"finishReason": "STOP",
							"index":        0,
							"content": map[string]interface{}{
								"role":  "model",
								"parts": []interface{}{},
							},
							"safetyRatings": []interface{}{
								map[string]interface{}{
									"category":    "HARM_CATEGORY_HATE_SPEECH",
									"probability": "NEGLIGIBLE",
								},
							},
						},
					},
					"usageMetadata": map[string]interface{}{
						"promptTokenCount":     totalInputTokens,
						"candidatesTokenCount": totalOutputTokens,
						"totalTokenCount":      totalInputTokens + totalOutputTokens,
					},
				}
				geminiChunk := map[string]interface{}{
					"code": 200,
					"data": geminiData,
				}

				chunkData, _ := json.Marshal(geminiChunk)
				fmt.Fprintf(writer, "data: %s\n\n", string(chunkData))
				flusher.Flush()
			}
		}
	}

	log.Infof("[Claude->Gemini Stream] Stream completed. Total chunks: %d, Input tokens: %d, Output tokens: %d",
		chunkCount, totalInputTokens, totalOutputTokens)

	// 记录请求
	totalTokens := totalInputTokens + totalOutputTokens
	s.routeService.LogRequest(model, routeID, totalInputTokens, totalOutputTokens, totalTokens, true, "")

	return nil
}

// ProxyClaudeCodeRequest 代理 Claude Code 专用请求
// 请求来自 /api/claudecode/v1/messages，格式为 Claude Code 格式（包含工具链、系统提示词等）
// 智能检测目标路由格式：如果目标是 Claude 格式则直接透传，如果是 OpenAI 格式则转换
func (s *ProxyService) ProxyClaudeCodeRequest(requestBody []byte, headers map[string]string) ([]byte, int, error) {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("'model' field is required")
	}

	log.Infof("[Claude Code] Received request for model: %s", model)

	// 提取真实的模型名（处理可能的后缀）
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return nil, http.StatusNotFound, fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	var needConvertResponse bool
	startTime := time.Now()

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		targetFormat := normalizeFormat(target.Format)
		if targetFormat == "" {
			targetFormat = inferFormatFromRoute(target.APIUrl, target.Model)
		}

		var transformedBody []byte
		var targetURL string
		needConvertResponse = false

		if targetFormat == "claude" || targetFormat == "anthropic" {
			transformedBody = requestBody
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
		} else {
			adapter := adapters.GetAdapter("claudecode-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("claudecode-to-openai adapter not found")
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
			needConvertResponse = true
		}

		log.Infof("[Claude Code] %s -> %s (endpoint: %s, target format: %s)", model, targetURL, target.Name, targetFormat)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if targetFormat == "claude" || targetFormat == "anthropic" {
			if target.APIKey != "" {
				proxyReq.Header.Set("x-api-key", target.APIKey)
			}
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		} else {
			if target.APIKey != "" {
				proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
			} else if auth := headers["Authorization"]; auth != "" {
				proxyReq.Header.Set("Authorization", auth)
			}
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		s.routeService.LogRequest(model, 0, 0, 0, 0, false, ferr.Error())
		return nil, http.StatusServiceUnavailable, ferr
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, err.Error())
		return nil, http.StatusInternalServerError, err
	}

	log.Infof("[Claude Code] Response received from %s in %v, status: %d", route.Name, time.Since(startTime), resp.StatusCode)

	// 记录使用情况并处理响�?
	if resp.StatusCode == http.StatusOK {
		var respData map[string]interface{}
		if err := json.Unmarshal(responseBody, &respData); err == nil {
			// 记录 token 使用
			if needConvertResponse {
				// OpenAI 格式响应
				if usage, ok := respData["usage"].(map[string]interface{}); ok {
					promptTokens := 0
					completionTokens := 0
					totalTokens := 0
					if pt, ok := usage["prompt_tokens"].(float64); ok {
						promptTokens = int(pt)
					}
					if ct, ok := usage["completion_tokens"].(float64); ok {
						completionTokens = int(ct)
					}
					if tt, ok := usage["total_tokens"].(float64); ok {
						totalTokens = int(tt)
					}
					s.routeService.LogRequest(model, route.ID, promptTokens, completionTokens, totalTokens, true, "")
				}

				// �?OpenAI 响应转换�?Claude 格式
				log.Infof("[Claude Code] Converting OpenAI response to Claude format")
				adapter := adapters.GetAdapter("claudecode-to-openai")
				if adapter != nil {
					claudeResp, err := adapter.AdaptResponse(respData)
					if err != nil {
						log.Errorf("[Claude Code] Failed to adapt response: %v", err)
					} else {
						if convertedBody, err := json.Marshal(claudeResp); err == nil {
							log.Infof("[Claude Code] Successfully converted response to Claude format")
							return convertedBody, resp.StatusCode, nil
						}
					}
				}
			} else {
				// Claude 格式响应，直接透传，但记录 token
				if usage, ok := respData["usage"].(map[string]interface{}); ok {
					inputTokens := 0
					outputTokens := 0
					if it, ok := usage["input_tokens"].(float64); ok {
						inputTokens = int(it)
					}
					if ot, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(ot)
					}
					s.routeService.LogRequest(model, route.ID, inputTokens, outputTokens, inputTokens+outputTokens, true, "")
				}
				// 直接返回 Claude 格式响应
				return responseBody, resp.StatusCode, nil
			}
		}
	} else {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, string(responseBody))
	}

	return responseBody, resp.StatusCode, nil
}

// ProxyClaudeCodeStreamRequest 代理 Claude Code 专用流式请求
// 请求来自 /api/claudecode/v1/messages，格式为 Claude Code 格式
// 智能检测目标路由格式：如果目标是 Claude 格式则直接透传，如果是 OpenAI 格式则转换
func (s *ProxyService) ProxyClaudeCodeStreamRequest(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	log.Infof("[Claude Code Stream] Received request for model: %s", model)

	// 提取真实的模型名
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 首先检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	reqData["stream"] = true
	requestBody, _ = json.Marshal(reqData)

	var needConvertResponse bool

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		targetFormat := normalizeFormat(target.Format)
		if targetFormat == "" {
			targetFormat = inferFormatFromRoute(target.APIUrl, target.Model)
		}

		var transformedBody []byte
		var targetURL string
		needConvertResponse = false

		if targetFormat == "claude" || targetFormat == "anthropic" {
			transformedBody = requestBody
			targetURL = buildClaudeMessagesURL(cleanAPIUrl)
		} else {
			adapter := adapters.GetAdapter("claudecode-to-openai")
			if adapter == nil {
				return nil, fmt.Errorf("claudecode-to-openai adapter not found")
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = buildOpenAIChatURL(target.APIUrl)
			needConvertResponse = true
		}

		log.Infof("[Claude Code Stream] %s -> %s (endpoint: %s, target format: %s)", model, targetURL, target.Name, targetFormat)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if targetFormat == "claude" || targetFormat == "anthropic" {
			if target.APIKey != "" {
				proxyReq.Header.Set("x-api-key", target.APIKey)
			}
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		} else {
			if target.APIKey != "" {
				proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
			} else if auth := headers["Authorization"]; auth != "" {
				proxyReq.Header.Set("Authorization", auth)
			}
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend error: %d - %s", resp.StatusCode, string(body))
	}

	// 使用实际路由到的模型名用于统�?
	if needConvertResponse {
		// �?OpenAI 流式响应转换�?Claude 流式响应
		log.Infof("[Claude Code Stream] Converting OpenAI stream response to Claude format")
		return s.streamOpenAIToClaudeCode(resp.Body, writer, flusher, model, route.ID)
	} else {
		// Claude 格式响应，直接透传
		log.Infof("[Claude Code Stream] Passing through Claude stream response directly")
		return s.streamDirect(resp.Body, writer, flusher, model, route.ID)
	}
}

// streamOpenAIToClaudeCode �?OpenAI 流式响应转换�?Claude Code 流式响应
// 专门用于 /api/claudecode 路径，支持工具调用等高级功能
func (s *ProxyService) streamOpenAIToClaudeCode(reader io.Reader, writer io.Writer, flusher http.Flusher, model string, routeID int64) error {
	// 发�?Claude 流式响应的开始事�?
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	// message_start 事件
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	startData, _ := json.Marshal(messageStart)
	fmt.Fprintf(writer, "event: message_start\ndata: %s\n\n", string(startData))
	flusher.Flush()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	var totalPromptTokens int
	var totalCompletionTokens int
	var contentBlockStarted bool
	var toolCallsStarted bool
	var currentToolCalls []map[string]interface{}
	var contentIndex int

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			if data == "[DONE]" {
				break
			}

			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			// 提取 token 使用信息
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					totalPromptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					totalCompletionTokens = int(ct)
				}
			}

			// 提取内容
			if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						// 处理文本内容
						if content, ok := delta["content"].(string); ok && content != "" {
							if !contentBlockStarted {
								// content_block_start 事件
								contentBlockStart := map[string]interface{}{
									"type":  "content_block_start",
									"index": contentIndex,
									"content_block": map[string]interface{}{
										"type": "text",
										"text": "",
									},
								}
								blockStartData, _ := json.Marshal(contentBlockStart)
								fmt.Fprintf(writer, "event: content_block_start\ndata: %s\n\n", string(blockStartData))
								flusher.Flush()
								contentBlockStarted = true
							}

							// 发�?content_block_delta 事件
							deltaEvent := map[string]interface{}{
								"type":  "content_block_delta",
								"index": contentIndex,
								"delta": map[string]interface{}{
									"type": "text_delta",
									"text": content,
								},
							}
							deltaData, _ := json.Marshal(deltaEvent)
							fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(deltaData))
							flusher.Flush()
						}

						// 处理工具调用
						if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
							for _, tc := range toolCalls {
								if tcMap, ok := tc.(map[string]interface{}); ok {
									// 关闭之前的文本块
									if contentBlockStarted && !toolCallsStarted {
										contentBlockStop := map[string]interface{}{
											"type":  "content_block_stop",
											"index": contentIndex,
										}
										blockStopData, _ := json.Marshal(contentBlockStop)
										fmt.Fprintf(writer, "event: content_block_stop\ndata: %s\n\n", string(blockStopData))
										flusher.Flush()
										contentIndex++
									}
									toolCallsStarted = true

									// 获取工具调用索引
									tcIndex := 0
									if idx, ok := tcMap["index"].(float64); ok {
										tcIndex = int(idx)
									}

									// 确保 currentToolCalls 有足够的空间
									for len(currentToolCalls) <= tcIndex {
										currentToolCalls = append(currentToolCalls, map[string]interface{}{
											"id":        "",
											"name":      "",
											"arguments": "",
										})
									}

									// 更新工具调用信息
									if id, ok := tcMap["id"].(string); ok && id != "" {
										currentToolCalls[tcIndex]["id"] = id
									}
									if function, ok := tcMap["function"].(map[string]interface{}); ok {
										if name, ok := function["name"].(string); ok && name != "" {
											currentToolCalls[tcIndex]["name"] = name

											// 发�?tool_use content_block_start
											toolBlockStart := map[string]interface{}{
												"type":  "content_block_start",
												"index": contentIndex + tcIndex,
												"content_block": map[string]interface{}{
													"type":  "tool_use",
													"id":    currentToolCalls[tcIndex]["id"],
													"name":  name,
													"input": map[string]interface{}{},
												},
											}
											toolBlockStartData, _ := json.Marshal(toolBlockStart)
											fmt.Fprintf(writer, "event: content_block_start\ndata: %s\n\n", string(toolBlockStartData))
											flusher.Flush()
										}
										if args, ok := function["arguments"].(string); ok && args != "" {
											currentToolCalls[tcIndex]["arguments"] = currentToolCalls[tcIndex]["arguments"].(string) + args

											// 发�?input_json_delta
											inputDelta := map[string]interface{}{
												"type":  "content_block_delta",
												"index": contentIndex + tcIndex,
												"delta": map[string]interface{}{
													"type":         "input_json_delta",
													"partial_json": args,
												},
											}
											inputDeltaData, _ := json.Marshal(inputDelta)
											fmt.Fprintf(writer, "event: content_block_delta\ndata: %s\n\n", string(inputDeltaData))
											flusher.Flush()
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// 关闭所有打开的内容块
	if contentBlockStarted && !toolCallsStarted {
		contentBlockStop := map[string]interface{}{
			"type":  "content_block_stop",
			"index": contentIndex,
		}
		blockStopData, _ := json.Marshal(contentBlockStop)
		fmt.Fprintf(writer, "event: content_block_stop\ndata: %s\n\n", string(blockStopData))
		flusher.Flush()
	}

	// 关闭工具调用�?
	for i := range currentToolCalls {
		toolBlockStop := map[string]interface{}{
			"type":  "content_block_stop",
			"index": contentIndex + i,
		}
		toolBlockStopData, _ := json.Marshal(toolBlockStop)
		fmt.Fprintf(writer, "event: content_block_stop\ndata: %s\n\n", string(toolBlockStopData))
		flusher.Flush()
	}

	// 确定 stop_reason
	stopReason := "end_turn"
	if len(currentToolCalls) > 0 {
		stopReason = "tool_use"
	}

	// message_delta 事件
	messageDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": totalCompletionTokens,
		},
	}
	deltaData, _ := json.Marshal(messageDelta)
	fmt.Fprintf(writer, "event: message_delta\ndata: %s\n\n", string(deltaData))
	flusher.Flush()

	// message_stop 事件
	messageStop := map[string]interface{}{
		"type": "message_stop",
	}
	stopData, _ := json.Marshal(messageStop)
	fmt.Fprintf(writer, "event: message_stop\ndata: %s\n\n", string(stopData))
	flusher.Flush()

	// 记录请求
	totalTokens := totalPromptTokens + totalCompletionTokens
	s.routeService.LogRequest(model, routeID, totalPromptTokens, totalCompletionTokens, totalTokens, true, "")

	return nil
}

// ============ Cursor IDE 格式检测和处理 ============

// isCursorFormat 检测请求是否为 Cursor IDE 格式
// Cursor 使用 OpenAI 接口但 tools 和 messages 格式类似 Anthropic/Claude
// 主要特征：
// 1. Tool 定义使用扁平格式 {name, description, input_schema} 而非 OpenAI 的嵌套格式
// 2. Tool calls 在 assistant 消息的 content 数组中作为 tool_use 块
// 3. Tool results 在 user 消息的 content 数组中作为 tool_result 块
func isCursorFormat(reqData map[string]interface{}) bool {
	// 检查 tools 是否使用 Cursor 扁平格式
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				// Cursor 扁平格式：直接有 name 和 input_schema 字段，没有 function 嵌套
				if _, hasName := toolMap["name"].(string); hasName {
					if _, hasInputSchema := toolMap["input_schema"]; hasInputSchema {
						log.Debugf("[Cursor Detection] Found Cursor flat tool format")
						return true
					}
				}
			}
		}
	}

	// 检查 messages 是否包含 Cursor/Anthropic 格式的 tool_use 或 tool_result
	if messages, ok := reqData["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				if content, ok := msgMap["content"].([]interface{}); ok {
					for _, block := range content {
						if blockMap, ok := block.(map[string]interface{}); ok {
							blockType, _ := blockMap["type"].(string)
							if blockType == "tool_use" || blockType == "tool_result" {
								log.Debugf("[Cursor Detection] Found Cursor %s block in messages", blockType)
								return true
							}
						}
					}
				}
			}
		}
	}

	return false
}

// detectRequestFormat 检测请求的格式类型
// 返回: "cursor", "openai", "claude", "gemini"
func detectRequestFormat(reqData map[string]interface{}) string {
	// 首先检查是否是 Cursor 格式
	if isCursorFormat(reqData) {
		return "cursor"
	}

	// 检查是否有 Claude 特有的字段
	if _, hasSystem := reqData["system"]; hasSystem {
		// Claude 使用单独的 system 字段
		return "claude"
	}

	// 检查 messages 格式
	if messages, ok := reqData["messages"].([]interface{}); ok && len(messages) > 0 {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				// 检查是否有 tool_calls 字段（OpenAI 格式）
				if _, hasToolCalls := msgMap["tool_calls"]; hasToolCalls {
					return "openai"
				}
			}
		}
	}

	// 默认为 OpenAI 格式
	return "openai"
}

// adaptCursorRequest 将 Cursor 格式请求转换为标准 OpenAI 格式
// 包含 thinking 块的验证和修复
func (s *ProxyService) adaptCursorRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	adapter := adapters.GetAdapter("cursor")
	if adapter == nil {
		return nil, fmt.Errorf("cursor adapter not found")
	}

	// 转换请求
	convertedReq, err := adapter.AdaptRequest(reqData, model)
	if err != nil {
		return nil, err
	}

	// 检查是否需要禁用 thinking（基于历史兼容性）
	if messages, ok := convertedReq["messages"].([]interface{}); ok {
		if adapters.ShouldDisableThinkingDueToHistory(messages) {
			log.Infof("[Cursor] Disabling thinking due to incompatible history")
			// 移除 thinking 相关配置
			delete(convertedReq, "thinking")
		}

		// 过滤无效的 thinking 块
		adapters.FilterInvalidThinkingBlocks(messages)
	}

	return convertedReq, nil
}

// ProxyCursorRequest 代理 Cursor IDE 专用请求
// Cursor 使用 OpenAI 兼容接口但 tools 和 messages 格式类似 Anthropic/Claude
// 自动检测并转换 Cursor 格式为标准 OpenAI 格式
func (s *ProxyService) ProxyCursorRequest(requestBody []byte, headers map[string]string) ([]byte, int, error) {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("'model' field is required")
	}

	log.Infof("[Cursor] Received request for model: %s", model)

	// 提取真实的模型名
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return nil, http.StatusNotFound, fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	requestFormat := detectRequestFormat(reqData)
	if requestFormat == "cursor" {
		convertedReq, cerr := s.adaptCursorRequest(reqData, model)
		if cerr != nil {
			return nil, http.StatusInternalServerError, cerr
		}
		reqData = convertedReq
		requestBody, _ = json.Marshal(reqData)
		requestFormat = "openai"
	}

	var adapterName string
	var targetURL string
	startTime := time.Now()

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, requestFormat)

		var transformedBody []byte
		if adapterName != "" {
			adapter := adapters.GetAdapter(adapterName)
			if adapter == nil {
				return nil, fmt.Errorf("adapter not found: %s", adapterName)
			}
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = s.buildAdapterURL(cleanAPIUrl, adapterName, model)
		} else {
			transformedBody = requestBody
			targetURL = buildOpenAIChatURL(target.APIUrl)
		}

		log.Infof("[Cursor] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		s.routeService.LogRequest(model, 0, 0, 0, 0, false, ferr.Error())
		return nil, http.StatusServiceUnavailable, ferr
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, err.Error())
		return nil, http.StatusInternalServerError, err
	}

	log.Infof("[Cursor] Response received from %s in %v, status: %d", route.Name, time.Since(startTime), resp.StatusCode)

	// 如果是认证错误，记录更详细的信息
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		errMsg := fmt.Sprintf("backend auth error: %d - %s (route: %s, id: %d, url: %s - please check API key configuration)", resp.StatusCode, string(responseBody), route.Name, route.ID, targetURL)
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, errMsg)
		return nil, resp.StatusCode, fmt.Errorf("%s", errMsg)
	}

	// 记录使用情况
	if resp.StatusCode == http.StatusOK {
		var respData map[string]interface{}
		if err := json.Unmarshal(responseBody, &respData); err == nil {
			if usage, ok := respData["usage"].(map[string]interface{}); ok {
				promptTokens := 0
				completionTokens := 0
				totalTokens := 0
				if v, ok := usage["prompt_tokens"].(float64); ok {
					promptTokens = int(v)
				}
				if v, ok := usage["completion_tokens"].(float64); ok {
					completionTokens = int(v)
				}
				if v, ok := usage["total_tokens"].(float64); ok {
					totalTokens = int(v)
				}
				// 兼容 Claude API 的 input_tokens/output_tokens
				if v, ok := usage["input_tokens"].(float64); ok && promptTokens == 0 {
					promptTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok && completionTokens == 0 {
					completionTokens = int(v)
				}
				if totalTokens == 0 {
					totalTokens = promptTokens + completionTokens
				}
				s.routeService.LogRequest(model, route.ID, promptTokens, completionTokens, totalTokens, true, "")
			}
		}
	} else {
		s.routeService.LogRequest(model, route.ID, 0, 0, 0, false, string(responseBody))
	}

	// 如果使用了适配器，转换响应
	if adapterName != "" {
		adapter := adapters.GetAdapter(adapterName)
		if adapter != nil {
			var respData map[string]interface{}
			if err := json.Unmarshal(responseBody, &respData); err == nil {
				adaptedResp, err := adapter.AdaptResponse(respData)
				if err != nil {
					log.Errorf("[Cursor] Failed to adapt response: %v", err)
				} else {
					responseBody, _ = json.Marshal(adaptedResp)
				}
			}
		}
	}

	return responseBody, resp.StatusCode, nil
}

// ProxyCursorStreamRequest 代理 Cursor IDE 专用流式请求
func (s *ProxyService) ProxyCursorStreamRequest(requestBody []byte, headers map[string]string, writer io.Writer, flusher http.Flusher) error {
	// 解析请求
	var reqData map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqData); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}

	model, ok := reqData["model"].(string)
	if !ok || model == "" {
		return fmt.Errorf("'model' field is required")
	}

	log.Infof("[Cursor Stream] Received request for model: %s", model)

	// 提取真实的模型名
	realModel := model
	if strings.Contains(model, ":streamGenerateContent") {
		realModel = strings.TrimSuffix(model, ":streamGenerateContent")
	}

	// 检查是否是重定向关键字
	var route *database.ModelRoute
	var err error
	res, err := s.resolveTargets(realModel)
	if err != nil {
		availableModels, _ := s.routeService.GetAvailableModels()
		return fmt.Errorf("model '%s' not found in route list. Available models: %v (detail: %v)", model, availableModels, err)
	}
	model = res.Model
	reqData["model"] = model
	requestBody, _ = json.Marshal(reqData)

	requestFormat := detectRequestFormat(reqData)
	if requestFormat == "cursor" {
		convertedReq, cerr := s.adaptCursorRequest(reqData, model)
		if cerr != nil {
			return cerr
		}
		reqData = convertedReq
		requestBody, _ = json.Marshal(reqData)
		requestFormat = "openai"
	}

	var adapterName string
	var targetURL string

	resp, route, ferr := s.doWithFailover(res, func(target *database.ModelRoute) (*http.Response, error) {
		cleanAPIUrl := strings.TrimSuffix(target.APIUrl, "/")
		adapterName = s.detectAdapterForRoute(target, requestFormat)

		var transformedBody []byte

		if adapterName != "" {
			adapter := adapters.GetAdapter(adapterName)
			if adapter == nil {
				return nil, fmt.Errorf("adapter not found: %s", adapterName)
			}
			reqData["stream"] = true
			transformedReq, aerr := adapter.AdaptRequest(reqData, model)
			if aerr != nil {
				return nil, aerr
			}
			transformedBody, _ = json.Marshal(transformedReq)
			targetURL = s.buildAdapterStreamURL(cleanAPIUrl, adapterName, model)
		} else {
			reqData["stream"] = true
			reqData["stream_options"] = map[string]interface{}{"include_usage": true}
			transformedBody, _ = json.Marshal(reqData)
			targetURL = buildOpenAIChatURL(target.APIUrl)
		}

		log.Infof("[Cursor Stream] %s -> %s (endpoint: %s, adapter: %s)", model, targetURL, target.Name, adapterName)

		proxyReq, rerr := http.NewRequest("POST", targetURL, bytes.NewReader(transformedBody))
		if rerr != nil {
			return nil, rerr
		}
		proxyReq.Header.Set("Content-Type", "application/json")
		if target.APIKey != "" {
			proxyReq.Header.Set("Authorization", "Bearer "+target.APIKey)
		} else if auth := headers["Authorization"]; auth != "" {
			proxyReq.Header.Set("Authorization", auth)
		}
		if adapterName == "anthropic" {
			proxyReq.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.httpClient.Do(proxyReq)
	})
	if ferr != nil {
		return ferr
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// 如果是认证错误，提供更详细的路由信息
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("backend auth error: %d - %s (route: %s, id: %d, url: %s - please check API key configuration)", resp.StatusCode, string(body), route.Name, route.ID, targetURL)
		}
		return fmt.Errorf("backend error: %d - %s (route: %s, url: %s)", resp.StatusCode, string(body), route.Name, targetURL)
	}

	// 流式传输响应
	if adapterName != "" {
		return s.streamWithAdapter(resp.Body, writer, flusher, adapterName, model, route.ID)
	} else {
		return s.streamDirect(resp.Body, writer, flusher, model, route.ID)
	}
}
