package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// MinSignatureLength 有效签名的最小长度
const MinSignatureLength = 50

// CursorAdapter 处理 Cursor IDE 的特殊格式
// Cursor 使用 OpenAI 接口但 tools 和 messages 格式类似 Anthropic/Claude
// 主要差异：
// 1. Tool 定义使用扁平格式 {name, description, input_schema} 而非 OpenAI 的嵌套格式
// 2. Tool calls 在 assistant 消息的 content 数组中作为 tool_use 块
// 3. Tool results 在 user 消息的 content 数组中作为 tool_result 块
// 4. Thinking/reasoning 内容需要特殊处理
type CursorAdapter struct {
	currentSessionID string // 当前请求的会话ID
}

func init() {
	RegisterAdapter("cursor", &CursorAdapter{})
	RegisterAdapter("cursor-to-openai", &CursorAdapter{})
}

// NewCursorAdapter 创建新的 Cursor 适配器
func NewCursorAdapter() *CursorAdapter {
	return &CursorAdapter{}
}

// SetSessionID 设置当前会话ID（用于流式响应）
func (a *CursorAdapter) SetSessionID(sessionID string) {
	a.currentSessionID = sessionID
}

// AdaptRequest 将 Cursor 格式请求转换为标准 OpenAI 格式
func (a *CursorAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	openaiReq := make(map[string]interface{})

	// 设置模型
	openaiReq["model"] = model

	// 生成会话ID（基于消息内容）
	var sessionID string
	if messages, ok := reqData["messages"].([]interface{}); ok {
		sessionID = GenerateSessionID(messages)
		if sessionID != "" {
			log.Debugf("[Cursor] Generated session ID: %s", sessionID[:8])
		}

		// 获取会话签名（如果有）
		storedSig := GetSignatureForSession(sessionID)
		if storedSig != "" {
			log.Debugf("[Cursor] Using stored signature for session %s (len=%d)", sessionID[:8], len(storedSig))
		}

		// 过滤无效的 thinking 块（使用会话签名）
		filtered := FilterInvalidThinkingBlocksWithSession(messages, sessionID)
		if filtered > 0 {
			log.Infof("[Cursor] Filtered %d invalid thinking block(s) in session %s", filtered, sessionID[:8])
		}

		// 转换消息
		openaiMessages := a.convertMessages(messages)
		openaiReq["messages"] = openaiMessages

		// 保存会话ID到请求中（用于后续流式响应）
		openaiReq["_session_id"] = sessionID
	}

	// 转换 tools - 处理 Cursor 扁平格式和 OpenAI 嵌套格式
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		openaiTools := a.convertTools(tools)
		if len(openaiTools) > 0 {
			openaiReq["tools"] = openaiTools
		}
	}

	// 转换 tool_choice
	if toolChoice := reqData["tool_choice"]; toolChoice != nil {
		openaiReq["tool_choice"] = a.convertToolChoice(toolChoice)
	}

	// 复制其他标准参数
	copyIfExists(reqData, openaiReq, "max_tokens")
	copyIfExists(reqData, openaiReq, "max_completion_tokens")
	copyIfExists(reqData, openaiReq, "temperature")
	copyIfExists(reqData, openaiReq, "top_p")
	copyIfExists(reqData, openaiReq, "stream")
	copyIfExists(reqData, openaiReq, "stop")
	copyIfExists(reqData, openaiReq, "presence_penalty")
	copyIfExists(reqData, openaiReq, "frequency_penalty")
	copyIfExists(reqData, openaiReq, "user")

	// 如果是流式请求，启用 usage 统计
	if stream, ok := reqData["stream"].(bool); ok && stream {
		openaiReq["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
	}

	return openaiReq, nil
}

// convertTools 转换工具定义，支持 Cursor 扁平格式和 OpenAI 嵌套格式
func (a *CursorAdapter) convertTools(tools []interface{}) []interface{} {
	openaiTools := make([]interface{}, 0, len(tools))

	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}

		var openaiTool map[string]interface{}

		// 检查是否是 Cursor 扁平格式（直接有 name 字段）
		if name, hasName := toolMap["name"].(string); hasName {
			// Cursor 扁平格式: {name, description, input_schema}
			description, _ := toolMap["description"].(string)
			inputSchema := toolMap["input_schema"]

			// 清理 JSON Schema
			cleanedSchema := sanitizeJSONSchema(inputSchema)

			openaiTool = map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": description,
					"parameters":  cleanedSchema,
				},
			}
			log.Debugf("[Cursor] Converted flat tool: %s", name)
		} else if function, hasFunction := toolMap["function"].(map[string]interface{}); hasFunction {
			// 标准 OpenAI 嵌套格式: {type: "function", function: {name, description, parameters}}
			toolType, _ := toolMap["type"].(string)
			if toolType != "" && toolType != "function" {
				continue // 跳过非 function 类型
			}

			name, _ := function["name"].(string)
			description, _ := function["description"].(string)
			parameters := function["parameters"]

			// 清理 JSON Schema
			cleanedSchema := sanitizeJSONSchema(parameters)

			openaiTool = map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": description,
					"parameters":  cleanedSchema,
				},
			}
		} else {
			// 未知格式，跳过
			continue
		}

		openaiTools = append(openaiTools, openaiTool)
	}

	return openaiTools
}

// convertMessages 转换消息，处理 Cursor 的 tool_use、tool_result 和 thinking 格式
func (a *CursorAdapter) convertMessages(messages []interface{}) []interface{} {
	openaiMessages := make([]interface{}, 0, len(messages))

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msgMap["role"].(string)
		content := msgMap["content"]

		switch role {
		case "system":
			// system 消息直接传递
			openaiMessages = append(openaiMessages, map[string]interface{}{
				"role":    "system",
				"content": extractTextContent(content),
			})

		case "user":
			// 检查 content 是否包含 tool_result
			if contentArr, isArray := content.([]interface{}); isArray {
				userMsgs := a.convertUserMessage(contentArr)
				openaiMessages = append(openaiMessages, userMsgs...)
			} else {
				// 普通文本消息
				openaiMessages = append(openaiMessages, map[string]interface{}{
					"role":    "user",
					"content": extractTextContent(content),
				})
			}

		case "assistant":
			// 检查是否有 tool_calls 字段（标准 OpenAI 格式）
			if toolCalls, hasToolCalls := msgMap["tool_calls"].([]interface{}); hasToolCalls && len(toolCalls) > 0 {
				// 已经是 OpenAI 格式
				assistantMsg := map[string]interface{}{
					"role":       "assistant",
					"tool_calls": toolCalls,
				}
				if contentStr := extractTextContent(content); contentStr != "" {
					assistantMsg["content"] = contentStr
				}
				// 保留 reasoning_content
				if reasoningContent, ok := msgMap["reasoning_content"].(string); ok && reasoningContent != "" {
					assistantMsg["reasoning_content"] = reasoningContent
				}
				openaiMessages = append(openaiMessages, assistantMsg)
			} else if contentArr, isArray := content.([]interface{}); isArray {
				// Cursor/Anthropic 格式：content 数组中包含 tool_use/thinking 块
				assistantMsg := a.convertAssistantMessage(contentArr)
				// 保留原始的 reasoning_content（如果有）
				if reasoningContent, ok := msgMap["reasoning_content"].(string); ok && reasoningContent != "" {
					if _, hasReasoning := assistantMsg["reasoning_content"]; !hasReasoning {
						assistantMsg["reasoning_content"] = reasoningContent
					}
				}
				openaiMessages = append(openaiMessages, assistantMsg)
			} else {
				// 普通文本消息
				assistantMsg := map[string]interface{}{
					"role":    "assistant",
					"content": extractTextContent(content),
				}
				// 保留 reasoning_content
				if reasoningContent, ok := msgMap["reasoning_content"].(string); ok && reasoningContent != "" {
					assistantMsg["reasoning_content"] = reasoningContent
				}
				openaiMessages = append(openaiMessages, assistantMsg)
			}

		case "tool":
			// 标准 OpenAI tool 消息，直接传递
			openaiMessages = append(openaiMessages, msgMap)

		default:
			// 其他角色直接传递
			openaiMessages = append(openaiMessages, msgMap)
		}
	}

	return openaiMessages
}

// convertUserMessage 转换包含 tool_result 的用户消息
func (a *CursorAdapter) convertUserMessage(contentArr []interface{}) []interface{} {
	result := make([]interface{}, 0)
	var textParts []string

	for _, block := range contentArr {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)

		switch blockType {
		case "tool_result":
			// 转换为 OpenAI 的 tool 角色消息
			toolUseID, _ := blockMap["tool_use_id"].(string)
			content := extractToolResultContentCursor(blockMap["content"])

			result = append(result, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      content,
			})
			log.Debugf("[Cursor] Converted tool_result: %s", toolUseID)

		case "text":
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}

		default:
			// 其他类型尝试提取文本
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	// 如果有文本内容，添加为用户消息
	if len(textParts) > 0 {
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": strings.Join(textParts, "\n"),
		})
	}

	return result
}

// convertAssistantMessage 转换包含 tool_use 和 thinking 的助手消息
func (a *CursorAdapter) convertAssistantMessage(contentArr []interface{}) map[string]interface{} {
	assistantMsg := map[string]interface{}{
		"role": "assistant",
	}

	var textParts []string
	var thinkingParts []string
	var toolCalls []interface{}

	for _, block := range contentArr {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}

		blockType, _ := blockMap["type"].(string)

		switch blockType {
		case "tool_use":
			// 转换为 OpenAI 的 tool_calls 格式
			id, _ := blockMap["id"].(string)
			name, _ := blockMap["name"].(string)
			input := blockMap["input"]

			// 将 input 转为 JSON 字符串
			var arguments string
			if input != nil {
				if inputBytes, err := json.Marshal(input); err == nil {
					arguments = string(inputBytes)
				}
			}

			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": arguments,
				},
			})
			log.Debugf("[Cursor] Converted tool_use: %s -> %s", id, name)

		case "thinking":
			// 提取 thinking 内容
			if thinking, ok := blockMap["thinking"].(string); ok && thinking != "" {
				thinkingParts = append(thinkingParts, thinking)
			}
			// 存储 signature 供后续使用
			if signature, ok := blockMap["signature"].(string); ok && signature != "" {
				// 优先存储到当前会话
				if a.currentSessionID != "" {
					StoreSignatureForSession(a.currentSessionID, signature)
					log.Debugf("[Cursor] Stored signature for session %s (len=%d)", a.currentSessionID[:min(8, len(a.currentSessionID))], len(signature))
				} else {
					// Fallback 到全局存储
					StoreThoughtSignature(signature)
				}
			}
			log.Debugf("[Cursor] Extracted thinking block")

		case "text":
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}

		default:
			// 其他类型尝试提取文本
			if text, ok := blockMap["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		}
	}

	if len(textParts) > 0 {
		assistantMsg["content"] = strings.Join(textParts, "\n")
	}

	if len(thinkingParts) > 0 {
		assistantMsg["reasoning_content"] = strings.Join(thinkingParts, "\n")
	}

	if len(toolCalls) > 0 {
		assistantMsg["tool_calls"] = toolCalls
	}

	return assistantMsg
}

// convertToolChoice 转换 tool_choice 参数
func (a *CursorAdapter) convertToolChoice(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case string:
		// "auto", "none", "required" 直接返回
		return tc
	case map[string]interface{}:
		// Cursor/Anthropic 格式: {type: "auto"} 或 {type: "tool", name: "xxx"}
		if tcType, ok := tc["type"].(string); ok {
			switch tcType {
			case "auto":
				return "auto"
			case "any":
				return "required"
			case "tool":
				if name, ok := tc["name"].(string); ok {
					return map[string]interface{}{
						"type": "function",
						"function": map[string]interface{}{
							"name": name,
						},
					}
				}
			}
		}
		// 可能已经是 OpenAI 格式
		if tc["type"] == "function" {
			return tc
		}
	}
	return "auto"
}

// AdaptResponse 将 OpenAI 响应转换为 Cursor/Anthropic 格式（如果需要）
func (a *CursorAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	// Cursor 使用 OpenAI 接口，但需要处理 reasoning_content
	// 检查响应中是否有 reasoning_content，如果有则保留
	if choices, ok := respData["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				// 提取并存储 signature（如果有）
				if sig, ok := message["signature"].(string); ok && sig != "" {
					if a.currentSessionID != "" {
						StoreSignatureForSession(a.currentSessionID, sig)
					} else {
						StoreThoughtSignature(sig)
					}
				}
			}
		}
	}
	return respData, nil
}

// AdaptStreamChunk 转换流式响应块
func (a *CursorAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	// Cursor 使用 OpenAI 接口，流式响应保持 OpenAI 格式
	// 但需要处理 reasoning_content 和 signature
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				// 提取并存储 signature（如果有）
				if sig, ok := delta["signature"].(string); ok && sig != "" {
					if a.currentSessionID != "" {
						StoreSignatureForSession(a.currentSessionID, sig)
						log.Debugf("[Cursor-Stream] Stored signature for session %s (len=%d)", a.currentSessionID[:min(8, len(a.currentSessionID))], len(sig))
					} else {
						StoreThoughtSignature(sig)
					}
				}
			}
		}
	}
	return chunk, nil
}

// AdaptStreamStart 流式响应开始
func (a *CursorAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *CursorAdapter) AdaptStreamEnd() []map[string]interface{} {
	return nil
}

// ============ 辅助函数 ============

// extractTextContent 从 content 中提取文本
func extractTextContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemMap["type"] == "text" {
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

// extractToolResultContentCursor 提取 tool_result 的内容（Cursor 专用）
func extractToolResultContentCursor(content interface{}) string {
	if content == nil {
		return "(empty result)"
	}

	switch c := content.(type) {
	case string:
		if c == "" {
			return "(empty result)"
		}
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemMap["type"] == "text" {
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, text)
					}
				} else {
					// 其他类型序列化为 JSON
					if jsonBytes, err := json.Marshal(itemMap); err == nil {
						parts = append(parts, string(jsonBytes))
					}
				}
			} else if str, ok := item.(string); ok {
				parts = append(parts, str)
			}
		}
		result := strings.Join(parts, "\n")
		if result == "" {
			return "(empty result)"
		}
		return result
	case map[string]interface{}:
		if c["type"] == "text" {
			if text, ok := c["text"].(string); ok {
				return text
			}
		}
		if jsonBytes, err := json.Marshal(c); err == nil {
			return string(jsonBytes)
		}
	}

	return fmt.Sprintf("%v", content)
}

// sanitizeJSONSchema 清理 JSON Schema，移除不支持的字段
func sanitizeJSONSchema(schema interface{}) interface{} {
	if schema == nil {
		return map[string]interface{}{}
	}

	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		return schema
	}

	// 需要跳过的字段
	skipFields := map[string]bool{
		"additionalProperties": true,
		"$schema":              true,
		"title":                true,
		"default":              true,
	}

	result := make(map[string]interface{})

	for key, value := range schemaMap {
		// 跳过空的 required 数组
		if key == "required" {
			if arr, ok := value.([]interface{}); ok && len(arr) == 0 {
				continue
			}
		}

		// 跳过不支持的字段
		if skipFields[key] {
			continue
		}

		// 处理 anyOf - 简化为第一个非 null 选项
		if key == "anyOf" {
			if anyOfArr, ok := value.([]interface{}); ok {
				for _, option := range anyOfArr {
					if optionMap, ok := option.(map[string]interface{}); ok {
						// 跳过 null 类型和 not 约束
						if optionMap["type"] == "null" {
							continue
						}
						if _, hasNot := optionMap["not"]; hasNot {
							continue
						}
						// 使用第一个有效选项
						sanitized := sanitizeJSONSchema(optionMap)
						if sanitizedMap, ok := sanitized.(map[string]interface{}); ok {
							for k, v := range sanitizedMap {
								result[k] = v
							}
						}
						break
					}
				}
				continue
			}
		}

		// 递归处理嵌套对象
		if key == "properties" {
			if propsMap, ok := value.(map[string]interface{}); ok {
				sanitizedProps := make(map[string]interface{})
				for propName, propValue := range propsMap {
					sanitizedProps[propName] = sanitizeJSONSchema(propValue)
				}
				result[key] = sanitizedProps
				continue
			}
		}

		// 递归处理其他嵌套对象
		if valueMap, ok := value.(map[string]interface{}); ok {
			result[key] = sanitizeJSONSchema(valueMap)
		} else if valueArr, ok := value.([]interface{}); ok {
			sanitizedArr := make([]interface{}, len(valueArr))
			for i, item := range valueArr {
				if itemMap, ok := item.(map[string]interface{}); ok {
					sanitizedArr[i] = sanitizeJSONSchema(itemMap)
				} else {
					sanitizedArr[i] = item
				}
			}
			result[key] = sanitizedArr
		} else {
			result[key] = value
		}
	}

	return result
}

// copyIfExists 如果源 map 中存在指定 key，则复制到目标 map
func copyIfExists(src, dst map[string]interface{}, key string) {
	if val, ok := src[key]; ok {
		dst[key] = val
	}
}

// generateCursorID 生成唯一 ID
func generateCursorID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// HasValidSignature 检查 thinking 块是否有有效签名
func HasValidSignature(block map[string]interface{}) bool {
	if blockType, ok := block["type"].(string); ok && blockType != "thinking" {
		return true // 非 thinking 块总是有效
	}

	thinking, _ := block["thinking"].(string)
	signature, _ := block["signature"].(string)

	// 空 thinking + 任意签名 = 有效
	if thinking == "" && signature != "" {
		return true
	}

	// 非空 thinking + 有效签名 = 有效
	if signature != "" && len(signature) >= MinSignatureLength {
		return true
	}

	return false
}

// FilterInvalidThinkingBlocks 过滤无效的 thinking 块
// 返回过滤的块数量
func FilterInvalidThinkingBlocks(messages []interface{}) int {
	return FilterInvalidThinkingBlocksWithSession(messages, defaultSessionID)
}

// FilterInvalidThinkingBlocksWithSession 过滤无效的 thinking 块（使用会话签名）
// 返回过滤的块数量
func FilterInvalidThinkingBlocksWithSession(messages []interface{}, sessionID string) int {
	totalFiltered := 0

	// 优先使用会话签名，fallback 到全局签名
	globalSig := GetSignatureForSession(sessionID)
	if globalSig == "" {
		globalSig = GetThoughtSignature()
	}

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msgMap["role"].(string)
		if role != "assistant" && role != "model" {
			continue
		}

		content, ok := msgMap["content"].([]interface{})
		if !ok {
			continue
		}

		newBlocks := make([]interface{}, 0, len(content))
		for _, block := range content {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				newBlocks = append(newBlocks, block)
				continue
			}

			blockType, _ := blockMap["type"].(string)
			if blockType == "thinking" {
				if HasValidSignature(blockMap) {
					// 有效的 thinking 块 - 保留但移除 cache_control
					cleaned := map[string]interface{}{
						"type":     "thinking",
						"thinking": blockMap["thinking"],
					}
					if sig, ok := blockMap["signature"].(string); ok && sig != "" {
						cleaned["signature"] = sig

						// 存储到会话（如果有会话ID）
						if sessionID != "" && sessionID != defaultSessionID {
							StoreSignatureForSession(sessionID, sig)
						}
					}
					newBlocks = append(newBlocks, cleaned)
				} else if globalSig != "" && len(globalSig) >= MinSignatureLength {
					// 无效签名但有全局/会话签名 - 修复
					thinkingText, _ := blockMap["thinking"].(string)
					log.Debugf("[Thinking-Filter] Repairing thinking block with signature from session %s (len=%d)", sessionID[:min(8, len(sessionID))], len(thinkingText))
					newBlocks = append(newBlocks, map[string]interface{}{
						"type":      "thinking",
						"thinking":  thinkingText,
						"signature": globalSig,
					})
				} else {
					// 无效签名 - 转换为 text 或丢弃
					thinkingText, _ := blockMap["thinking"].(string)
					if strings.TrimSpace(thinkingText) != "" {
						log.Debugf("[Thinking-Filter] Converting thinking block with invalid signature to text (len=%d)", len(thinkingText))
						newBlocks = append(newBlocks, map[string]interface{}{
							"type": "text",
							"text": thinkingText,
						})
					} else {
						log.Debugf("[Thinking-Filter] Dropping empty thinking block with invalid signature")
					}
					totalFiltered++
				}
			} else {
				// 非 thinking 块 - 保留但移除 cache_control
				cleanedBlock := make(map[string]interface{})
				for k, v := range blockMap {
					if k != "cache_control" {
						cleanedBlock[k] = v
					}
				}
				newBlocks = append(newBlocks, cleanedBlock)
			}
		}

		// 确保消息至少有一个块
		if len(newBlocks) == 0 {
			newBlocks = append(newBlocks, map[string]interface{}{
				"type": "text",
				"text": "",
			})
		}

		msgMap["content"] = newBlocks
	}

	if totalFiltered > 0 {
		log.Infof("[Thinking-Filter] Filtered %d invalid thinking block(s)", totalFiltered)
	}

	return totalFiltered
}

// min 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ShouldDisableThinkingDueToHistory 检查是否因历史不兼容而需要禁用 thinking
func ShouldDisableThinkingDueToHistory(messages []interface{}) bool {
	// 从后向前查找最后一个 assistant 消息
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}

		role, _ := msg["role"].(string)
		if role != "assistant" && role != "model" {
			continue
		}

		content, ok := msg["content"].([]interface{})
		if !ok {
			return false
		}

		hasToolUse := false
		hasThinking := false

		for _, block := range content {
			blockMap, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := blockMap["type"].(string)
			if blockType == "tool_use" {
				hasToolUse = true
			}
			if blockType == "thinking" {
				hasThinking = true
			}
		}

		// Tool use 但没有 thinking = 不兼容
		if hasToolUse && !hasThinking {
			log.Infof("[Thinking-Mode] Detected ToolUse without Thinking in history. Disabling thinking.")
			return true
		}

		// 只检查最近的 assistant 消息
		return false
	}

	return false
}
