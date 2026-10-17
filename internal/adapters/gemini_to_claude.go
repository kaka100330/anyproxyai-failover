package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GeminiToClaudeAdapter 将 Gemini 格式转换为 Claude 格式
type GeminiToClaudeAdapter struct{}

func init() {
	RegisterAdapter("gemini-to-claude", &GeminiToClaudeAdapter{})
}

// AdaptRequest 将 Gemini 请求转换为 Claude 请求
func (a *GeminiToClaudeAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	claudeReq := make(map[string]interface{})

	// 设置模型
	if model != "" {
		claudeReq["model"] = model
	} else {
		claudeReq["model"] = "claude-3-sonnet-20240229"
	}

	// 转换 systemInstruction
	var systemContent string
	if systemInstruction, ok := reqData["systemInstruction"].(map[string]interface{}); ok {
		if parts, ok := systemInstruction["parts"].([]interface{}); ok {
			var systemParts []string
			for _, part := range parts {
				if partMap, ok := part.(map[string]interface{}); ok {
					if text, ok := partMap["text"].(string); ok {
						systemParts = append(systemParts, text)
					}
				}
			}
			systemContent = strings.Join(systemParts, "\n\n")
		}
	}

	// 转换 contents 为 messages
	claudeMessages := make([]interface{}, 0)
	if contents, ok := reqData["contents"].([]interface{}); ok {
		for _, content := range contents {
			if contentMap, ok := content.(map[string]interface{}); ok {
				role, _ := contentMap["role"].(string)
				parts, _ := contentMap["parts"].([]interface{})

				// 转换角色: Gemini 的 "model" -> Claude 的 "assistant"
				claudeRole := role
				if role == "model" {
					claudeRole = "assistant"
				}

				// 转换 parts
				var contentBlocks []interface{}
				var textContent string

				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						// 处理文本内容
						if text, ok := partMap["text"].(string); ok {
							textContent += text
						}

						// 处理函数调用
						if functionCall, ok := partMap["functionCall"].(map[string]interface{}); ok {
							name, _ := functionCall["name"].(string)
							args := functionCall["args"]

							var argsJSON []byte
							var err error
							if args != nil {
								argsJSON, err = json.Marshal(args)
								if err != nil {
									argsJSON = []byte("{}")
								}
							} else {
								argsJSON = []byte("{}")
							}

							var input map[string]interface{}
							json.Unmarshal(argsJSON, &input)

							contentBlocks = append(contentBlocks, map[string]interface{}{
								"type":  "tool_use",
								"id":    fmt.Sprintf("toolu_%s", name),
								"name":  name,
								"input": input,
							})
						}

						// 处理函数响应
						if functionResponse, ok := partMap["functionResponse"].(map[string]interface{}); ok {
							name, _ := functionResponse["name"].(string)
							response := functionResponse["response"]

							var responseStr string
							if respMap, ok := response.(map[string]interface{}); ok {
								if result, ok := respMap["result"].(string); ok {
									responseStr = result
								} else {
									respJSON, _ := json.Marshal(respMap)
									responseStr = string(respJSON)
								}
							}

							contentBlocks = append(contentBlocks, map[string]interface{}{
								"type":        "tool_result",
								"tool_use_id": fmt.Sprintf("toolu_%s", name),
								"content":     responseStr,
							})
						}
					}
				}

				// 构建消息
				claudeMsg := map[string]interface{}{
					"role": claudeRole,
				}

				if len(contentBlocks) > 0 {
					// 如果有工具调用或工具结果,添加文本内容
					if textContent != "" {
						contentBlocks = append([]interface{}{
							map[string]interface{}{
								"type": "text",
								"text": textContent,
							},
						}, contentBlocks...)
					}
					claudeMsg["content"] = contentBlocks
				} else {
					// 纯文本消息
					claudeMsg["content"] = textContent
				}

				claudeMessages = append(claudeMessages, claudeMsg)
			}
		}
	}

	claudeReq["messages"] = claudeMessages

	// 设置 system
	if systemContent != "" {
		claudeReq["system"] = systemContent
	}

	// 转换 generationConfig
	if generationConfig, ok := reqData["generationConfig"].(map[string]interface{}); ok {
		if maxOutputTokens, ok := generationConfig["maxOutputTokens"]; ok {
			claudeReq["max_tokens"] = maxOutputTokens
		}
		if temperature, ok := generationConfig["temperature"]; ok {
			claudeReq["temperature"] = temperature
		}
		if topP, ok := generationConfig["topP"]; ok {
			claudeReq["top_p"] = topP
		}
		if stopSequences, ok := generationConfig["stopSequences"]; ok {
			claudeReq["stop_sequences"] = stopSequences
		}
	}

	// 如果没有设置 max_tokens,使用默认值
	if _, ok := claudeReq["max_tokens"]; !ok {
		claudeReq["max_tokens"] = 4096
	}

	// 转换 tools
	if tools, ok := reqData["tools"].([]interface{}); ok && len(tools) > 0 {
		claudeTools := make([]interface{}, 0)
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				if functionDeclarations, ok := toolMap["functionDeclarations"].([]interface{}); ok {
					for _, funcDecl := range functionDeclarations {
						if funcDeclMap, ok := funcDecl.(map[string]interface{}); ok {
							name, _ := funcDeclMap["name"].(string)
							description, _ := funcDeclMap["description"].(string)
							parameters := funcDeclMap["parameters"]

							claudeTools = append(claudeTools, map[string]interface{}{
								"name":         name,
								"description":  description,
								"input_schema": parameters,
							})
						}
					}
				}
			}
		}
		if len(claudeTools) > 0 {
			claudeReq["tools"] = claudeTools
		}
	}

	// 转换 stream
	if stream, ok := reqData["stream"]; ok {
		claudeReq["stream"] = stream
	}

	return claudeReq, nil
}

// AdaptResponse 将 Claude 响应转换为 Gemini 响应
func (a *GeminiToClaudeAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	geminiResp := make(map[string]interface{})

	// 提取内容
	var textContent string
	var functionCalls []interface{}
	stopReason := "STOP"

	if content, ok := respData["content"].([]interface{}); ok {
		for _, block := range content {
			if blockMap, ok := block.(map[string]interface{}); ok {
				blockType, _ := blockMap["type"].(string)

				switch blockType {
				case "text":
					if text, ok := blockMap["text"].(string); ok {
						textContent += text
					}
				case "tool_use":
					name, _ := blockMap["name"].(string)
					input := blockMap["input"]

					functionCalls = append(functionCalls, map[string]interface{}{
						"name": name,
						"args": input,
					})
				}
			}
		}
	}

	// 转换 stop_reason
	if sr, ok := respData["stop_reason"].(string); ok {
		switch sr {
		case "end_turn":
			stopReason = "STOP"
		case "max_tokens":
			stopReason = "MAX_TOKENS"
		case "stop_sequence":
			stopReason = "STOP"
		case "tool_use":
			stopReason = "STOP"
		}
	}

	// 构建 parts
	parts := make([]interface{}, 0)
	if textContent != "" {
		parts = append(parts, map[string]interface{}{
			"text": textContent,
		})
	}
	for _, fc := range functionCalls {
		parts = append(parts, map[string]interface{}{
			"functionCall": fc,
		})
	}

	// 如果没有内容,添加空文本
	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{
			"text": "",
		})
	}

	// 构建 candidates
	geminiResp["candidates"] = []interface{}{
		map[string]interface{}{
			"content": map[string]interface{}{
				"role":  "model",
				"parts": parts,
			},
			"finishReason": stopReason,
		},
	}

	// 转换 usage
	if usage, ok := respData["usage"].(map[string]interface{}); ok {
		inputTokens := 0
		outputTokens := 0

		if it, ok := usage["input_tokens"].(float64); ok {
			inputTokens = int(it)
		}
		if ot, ok := usage["output_tokens"].(float64); ok {
			outputTokens = int(ot)
		}

		geminiResp["usageMetadata"] = map[string]interface{}{
			"promptTokenCount":     inputTokens,
			"candidatesTokenCount": outputTokens,
			"totalTokenCount":      inputTokens + outputTokens,
		}
	}

	return geminiResp, nil
}

// AdaptStreamChunk 转换流式响应块
func (a *GeminiToClaudeAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	chunkType, _ := chunk["type"].(string)

	switch chunkType {
	case "content_block_delta":
		// 提取文本内容
		var textContent string
		if delta, ok := chunk["delta"].(map[string]interface{}); ok {
			if deltaType, ok := delta["type"].(string); ok && deltaType == "text_delta" {
				if text, ok := delta["text"].(string); ok {
					textContent = text
				}
			}
		}

		if textContent != "" {
			return map[string]interface{}{
				"candidates": []interface{}{
					map[string]interface{}{
						"content": map[string]interface{}{
							"role": "model",
							"parts": []interface{}{
								map[string]interface{}{
									"text": textContent,
								},
							},
						},
					},
				},
			}, nil
		}

	case "message_stop":
		return map[string]interface{}{
			"candidates": []interface{}{
				map[string]interface{}{
					"finishReason": "STOP",
				},
			},
		}, nil
	}

	return nil, nil
}

// AdaptStreamStart 流式响应开始
func (a *GeminiToClaudeAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	// Gemini 流式响应不需要特殊的开始事件
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *GeminiToClaudeAdapter) AdaptStreamEnd() []map[string]interface{} {
	// Gemini 流式响应不需要特殊的结束事件
	return nil
}
