package adapters

import (
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// OpenAIToClaudeAdapter 将 OpenAI 格式转换为 Claude 格式
type OpenAIToClaudeAdapter struct{}

func init() {
	RegisterAdapter("openai-to-claude", &OpenAIToClaudeAdapter{})
}

func (a *OpenAIToClaudeAdapter) AdaptRequest(request map[string]interface{}, targetModel string) (map[string]interface{}, error) {
	// 将 OpenAI 格式请求转换为 Claude 格式
	claudeReq := make(map[string]interface{})

	// 设置模型
	if targetModel != "" {
		claudeReq["model"] = targetModel
	} else if model, ok := request["model"].(string); ok {
		claudeReq["model"] = model
	}

	// 生成会话ID
	var sessionID string
	if messages, ok := request["messages"].([]interface{}); ok {
		sessionID = GenerateSessionID(messages)
		if sessionID != "" {
			log.Debugf("[OpenAI->Claude] Generated session ID: %s", sessionID[:8])

			// 保存到请求中（用于后续）
			claudeReq["_session_id"] = sessionID
		}
	}

	// 转换消息
	claudeMessages := make([]interface{}, 0)
	var systemContent string

	if messages, ok := request["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// 处理 system 消息 - Claude 使用单独的 system 字段
				if role == "system" {
					if contentStr, ok := content.(string); ok {
						if systemContent != "" {
							systemContent += "\n\n"
						}
						systemContent += contentStr
					}
					continue
				}

				// 处理 tool 消息 - 转换为 Claude 的 tool_result
				if role == "tool" {
					toolCallID, _ := msgMap["tool_call_id"].(string)
					contentStr := ""
					if cs, ok := content.(string); ok {
						contentStr = cs
					}

					claudeMessages = append(claudeMessages, map[string]interface{}{
						"role": "user",
						"content": []interface{}{
							map[string]interface{}{
								"type":        "tool_result",
								"tool_use_id": toolCallID,
								"content":     contentStr,
							},
						},
					})
					continue
				}

				// 处理 assistant 消息
				if role == "assistant" {
					assistantMsg := map[string]interface{}{
						"role": "assistant",
					}

					contentBlocks := make([]interface{}, 0)

					// **关键改进**: 处理 reasoning_content (thinking)
					if reasoningContent, ok := msgMap["reasoning_content"].(string); ok && reasoningContent != "" {
						// 获取会话签名
						sig := GetSignatureForSession(sessionID)
						if sig == "" {
							sig = GetThoughtSignature()
						}

						// Thinking 必须在最前面
						thinkingBlock := map[string]interface{}{
							"type":     "thinking",
							"thinking": reasoningContent,
						}
						if sig != "" && len(sig) >= MinSignatureLength {
							thinkingBlock["signature"] = sig
							log.Debugf("[OpenAI->Claude] Added signature to thinking block (len=%d)", len(sig))
						}
						contentBlocks = append(contentBlocks, thinkingBlock)
					}

					// 检查是否有 tool_calls
					if toolCalls, ok := msgMap["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
						// 添加文本内容（如果有）
						if contentStr, ok := content.(string); ok && contentStr != "" {
							contentBlocks = append(contentBlocks, map[string]interface{}{
								"type": "text",
								"text": contentStr,
							})
						}

						// 转换 tool_calls 为 tool_use
						for _, tc := range toolCalls {
							if tcMap, ok := tc.(map[string]interface{}); ok {
								id, _ := tcMap["id"].(string)
								if function, ok := tcMap["function"].(map[string]interface{}); ok {
									name, _ := function["name"].(string)
									arguments, _ := function["arguments"].(string)

									var input map[string]interface{}
									if err := json.Unmarshal([]byte(arguments), &input); err != nil {
										input = map[string]interface{}{"raw": arguments}
									}

									contentBlocks = append(contentBlocks, map[string]interface{}{
										"type":  "tool_use",
										"id":    id,
										"name":  name,
										"input": input,
									})
								}
							}
						}

						assistantMsg["content"] = contentBlocks
					} else {
						// 普通文本消息
						if len(contentBlocks) > 0 {
							// 已有 thinking 块
							if contentStr, ok := content.(string); ok && contentStr != "" {
								contentBlocks = append(contentBlocks, map[string]interface{}{
									"type": "text",
									"text": contentStr,
								})
							}
							assistantMsg["content"] = contentBlocks
						} else {
							// 纯文本
							assistantMsg["content"] = content
						}
					}

					claudeMessages = append(claudeMessages, assistantMsg)
					continue
				}

				// 处理 user 消息
				if role == "user" {
					claudeMessages = append(claudeMessages, map[string]interface{}{
						"role":    "user",
						"content": content,
					})
				}
			}
		}
	}

	claudeReq["messages"] = claudeMessages

	// 设置 system
	if systemContent != "" {
		claudeReq["system"] = systemContent
	}

	// 转换 tools
	if tools, ok := request["tools"].([]interface{}); ok && len(tools) > 0 {
		claudeTools := make([]interface{}, 0, len(tools))
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				if function, ok := toolMap["function"].(map[string]interface{}); ok {
					name, _ := function["name"].(string)
					description, _ := function["description"].(string)
					parameters := function["parameters"]

					claudeTools = append(claudeTools, map[string]interface{}{
						"name":         name,
						"description":  description,
						"input_schema": parameters,
					})
				}
			}
		}
		claudeReq["tools"] = claudeTools
	}

	// 转换 tool_choice
	if toolChoice := request["tool_choice"]; toolChoice != nil {
		claudeReq["tool_choice"] = a.convertToolChoice(toolChoice)
	}

	// 转换其他参数
	if maxTokens, ok := request["max_tokens"]; ok {
		claudeReq["max_tokens"] = maxTokens
	} else if maxCompletionTokens, ok := request["max_completion_tokens"]; ok {
		claudeReq["max_tokens"] = maxCompletionTokens
	} else {
		// Claude 需要 max_tokens
		claudeReq["max_tokens"] = 4096
	}

	if temperature, ok := request["temperature"]; ok {
		claudeReq["temperature"] = temperature
	}

	if topP, ok := request["top_p"]; ok {
		claudeReq["top_p"] = topP
	}

	if stream, ok := request["stream"]; ok {
		claudeReq["stream"] = stream
	}

	if stop, ok := request["stop"]; ok {
		claudeReq["stop_sequences"] = stop
	}

	return claudeReq, nil
}

// convertToolChoice 转换 tool_choice
func (a *OpenAIToClaudeAdapter) convertToolChoice(toolChoice interface{}) interface{} {
	switch tc := toolChoice.(type) {
	case string:
		switch tc {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "required":
			return map[string]interface{}{"type": "any"}
		case "none":
			return map[string]interface{}{"type": "auto"}
		}
	case map[string]interface{}:
		if tcType, ok := tc["type"].(string); ok && tcType == "function" {
			if function, ok := tc["function"].(map[string]interface{}); ok {
				if name, ok := function["name"].(string); ok {
					return map[string]interface{}{
						"type": "tool",
						"name": name,
					}
				}
			}
		}
	}
	return map[string]interface{}{"type": "auto"}
}

// extractSystemFromMessages 从消息中提取 system 内容（用于兼容）
func extractSystemFromMessages(messages []interface{}) string {
	var systemParts []string
	for _, msg := range messages {
		if msgMap, ok := msg.(map[string]interface{}); ok {
			if role, _ := msgMap["role"].(string); role == "system" {
				if content, ok := msgMap["content"].(string); ok {
					systemParts = append(systemParts, content)
				}
			}
		}
	}
	return strings.Join(systemParts, "\n\n")
}

func (a *OpenAIToClaudeAdapter) AdaptResponse(response map[string]interface{}) (map[string]interface{}, error) {
	// 将 OpenAI 响应转换为 Claude 格式
	adapted := make(map[string]interface{})

	// 基本字段
	adapted["id"] = "msg_default"
	adapted["type"] = "message"
	adapted["role"] = "assistant"
	adapted["model"] = "claude-3-sonnet-20240229"

	// 提取内容
	var contentText string
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				contentText = getStringValueOCClaude(message, "content", "")
			}
		}
	}

	// Claude 格式的 content
	adapted["content"] = []map[string]interface{}{
		{
			"type": "text",
			"text": contentText,
		},
	}

	// 转换使用量
	if usage, ok := response["usage"].(map[string]interface{}); ok {
		adapted["usage"] = map[string]interface{}{
			"input_tokens":  getIntValueOC(usage, "prompt_tokens", 0),
			"output_tokens": getIntValueOC(usage, "completion_tokens", 0),
		}
	}

	return adapted, nil
}

func (a *OpenAIToClaudeAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	// 调试日志：打印接收到的 chunk
	chunkJSON, _ := json.Marshal(chunk)
	fmt.Printf("[ADAPTER DEBUG] Received chunk: %s\n", string(chunkJSON))

	// 这是关键：将 OpenAI 流式 chunk 转换为 Claude 格式的事件流
	adapted := make(map[string]interface{})

	// 检查是否是 OpenAI 的 chat.completion.chunk
	if getStringValueOCClaude(chunk, "object", "") == "chat.completion.chunk" {
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			fmt.Printf("[ADAPTER DEBUG] No choices found\n")
			return nil, nil
		}

		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			fmt.Printf("[ADAPTER DEBUG] Invalid choice format\n")
			return nil, nil
		}

		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			fmt.Printf("[ADAPTER DEBUG] No delta found\n")
			return nil, nil
		}

		fmt.Printf("[ADAPTER DEBUG] Delta: %+v\n", delta)

		// 检查是否有内容
		if content, hasContent := delta["content"].(string); hasContent && content != "" {
			fmt.Printf("[ADAPTER DEBUG] Found content: %s\n", content)
			// 生成 content_block_delta 事件
			adapted["type"] = "content_block_delta"
			adapted["index"] = 0
			adapted["delta"] = map[string]interface{}{
				"type": "text_delta",
				"text": content,
			}
			return adapted, nil
		}

		// 检查是否有 role 信息
		if role, hasRole := delta["role"].(string); hasRole && role != "" {
			fmt.Printf("[ADAPTER DEBUG] Found role: %s\n", role)
			// 对于 role 信息，我们暂时跳过，因为已经在 AdaptStreamStart 中处理了
			return nil, nil
		}

		// 检查是否结束
		if finishReason, hasFinish := choice["finish_reason"].(string); hasFinish && finishReason != "" {
			fmt.Printf("[ADAPTER DEBUG] Found finish_reason: %s\n", finishReason)
			// 生成 message_stop 事件
			adapted["type"] = "message_stop"
			return adapted, nil
		}

		fmt.Printf("[ADAPTER DEBUG] No content or finish reason found\n")
	} else {
		fmt.Printf("[ADAPTER DEBUG] Not a chat.completion.chunk, object: %s\n", getStringValueOCClaude(chunk, "object", "unknown"))
	}

	// 对于没有内容但有其他信息的 chunk，返回 nil 以跳过
	return nil, nil
}

func (a *OpenAIToClaudeAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	// 生成 Claude 流式响应的开始事件
	var events []map[string]interface{}

	// message_start 事件
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_" + generateID(),
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
	events = append(events, messageStart)

	// content_block_start 事件
	contentBlockStart := map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	}
	events = append(events, contentBlockStart)

	return events
}

func (a *OpenAIToClaudeAdapter) AdaptStreamEnd() []map[string]interface{} {
	// 生成 Claude 流式响应的结束事件
	var events []map[string]interface{}

	// content_block_stop 事件
	contentBlockStop := map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}
	events = append(events, contentBlockStop)

	// message_delta 事件（usage 信息）
	messageDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": 0,
		},
	}
	events = append(events, messageDelta)

	return events
}

// 生成简单的 ID
func generateID() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 29)
	for i := range b {
		b[i] = charset[i%len(charset)]
	}
	return string(b)
}

// 辅助函数
func getStringValueOCClaude(m map[string]interface{}, key string, defaultValue string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}

func getIntValueOC(m map[string]interface{}, key string, defaultValue int) int {
	if val, ok := m[key]; ok {
		if f, ok := val.(float64); ok {
			return int(f)
		}
	}
	return defaultValue
}
