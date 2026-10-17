package adapters

import (
	"encoding/json"
	"fmt"
	"time"
)

// GeminiToOpenAIAdapter 将 Gemini 格式转换为 OpenAI 格式
type GeminiToOpenAIAdapter struct{}

func init() {
	RegisterAdapter("gemini-to-openai", &GeminiToOpenAIAdapter{})
}

// AdaptRequest 将 Gemini 请求转换为 OpenAI 请求
func (a *GeminiToOpenAIAdapter) AdaptRequest(reqData map[string]interface{}, model string) (map[string]interface{}, error) {
	openaiReq := make(map[string]interface{})

	// 设置模型
	openaiReq["model"] = model

	// 转换消息
	messages := make([]interface{}, 0)

	// 处理 systemInstruction
	if systemInstruction, ok := reqData["systemInstruction"].(map[string]interface{}); ok {
		if parts, ok := systemInstruction["parts"].([]interface{}); ok {
			var systemText string
			for _, part := range parts {
				if partMap, ok := part.(map[string]interface{}); ok {
					if text, ok := partMap["text"].(string); ok {
						systemText += text
					}
				}
			}
			if systemText != "" {
				messages = append(messages, map[string]interface{}{
					"role":    "system",
					"content": systemText,
				})
			}
		}
	}

	// 转换 contents
	if contents, ok := reqData["contents"].([]interface{}); ok {
		for _, content := range contents {
			if contentMap, ok := content.(map[string]interface{}); ok {
				role, _ := contentMap["role"].(string)
				parts, _ := contentMap["parts"].([]interface{})

				// 转换角色
				openaiRole := role
				if role == "model" {
					openaiRole = "assistant"
				}

				// 检查是否包含 functionCall 或 functionResponse
				var textContent string
				var toolCalls []interface{}
				var functionResponse *map[string]interface{}

				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						// 文本内容
						if text, ok := partMap["text"].(string); ok {
							textContent += text
						}

						// 函数调用
						if fc, ok := partMap["functionCall"].(map[string]interface{}); ok {
							name, _ := fc["name"].(string)
							args := fc["args"]

							var arguments string
							if argsBytes, err := json.Marshal(args); err == nil {
								arguments = string(argsBytes)
							}

							toolCalls = append(toolCalls, map[string]interface{}{
								"id":   fmt.Sprintf("call_%d_%s", time.Now().UnixNano(), name),
								"type": "function",
								"function": map[string]interface{}{
									"name":      name,
									"arguments": arguments,
								},
							})
						}

						// 函数响应
						if fr, ok := partMap["functionResponse"].(map[string]interface{}); ok {
							functionResponse = &fr
						}
					}
				}

				// 处理函数响应 - 转换为 tool 消息
				if functionResponse != nil {
					name, _ := (*functionResponse)["name"].(string)
					response := (*functionResponse)["response"]

					var contentStr string
					if respMap, ok := response.(map[string]interface{}); ok {
						if result, ok := respMap["result"].(string); ok {
							contentStr = result
						} else {
							if respBytes, err := json.Marshal(respMap); err == nil {
								contentStr = string(respBytes)
							}
						}
					}

					messages = append(messages, map[string]interface{}{
						"role":         "tool",
						"tool_call_id": fmt.Sprintf("call_%s", name),
						"content":      contentStr,
					})
					continue
				}

				// 构建消息
				msg := map[string]interface{}{
					"role": openaiRole,
				}

				if len(toolCalls) > 0 {
					msg["content"] = textContent
					msg["tool_calls"] = toolCalls
				} else {
					msg["content"] = textContent
				}

				messages = append(messages, msg)
			}
		}
	}

	openaiReq["messages"] = messages

	// 转换 tools
	if tools, ok := reqData["tools"].([]interface{}); ok {
		openaiTools := make([]interface{}, 0)
		for _, tool := range tools {
			if toolMap, ok := tool.(map[string]interface{}); ok {
				if functionDeclarations, ok := toolMap["functionDeclarations"].([]interface{}); ok {
					for _, fd := range functionDeclarations {
						if fdMap, ok := fd.(map[string]interface{}); ok {
							name, _ := fdMap["name"].(string)
							description, _ := fdMap["description"].(string)
							parameters := fdMap["parameters"]

							openaiTools = append(openaiTools, map[string]interface{}{
								"type": "function",
								"function": map[string]interface{}{
									"name":        name,
									"description": description,
									"parameters":  parameters,
								},
							})
						}
					}
				}
			}
		}
		if len(openaiTools) > 0 {
			openaiReq["tools"] = openaiTools
		}
	}

	// 转换 generationConfig
	if generationConfig, ok := reqData["generationConfig"].(map[string]interface{}); ok {
		if maxOutputTokens, ok := generationConfig["maxOutputTokens"]; ok {
			openaiReq["max_tokens"] = maxOutputTokens
		}
		if temperature, ok := generationConfig["temperature"]; ok {
			openaiReq["temperature"] = temperature
		}
		if topP, ok := generationConfig["topP"]; ok {
			openaiReq["top_p"] = topP
		}
		if stopSequences, ok := generationConfig["stopSequences"]; ok {
			openaiReq["stop"] = stopSequences
		}
	}

	// 处理 stream
	if stream, ok := reqData["stream"]; ok {
		openaiReq["stream"] = stream
		if streamBool, ok := stream.(bool); ok && streamBool {
			openaiReq["stream_options"] = map[string]interface{}{
				"include_usage": true,
			}
		}
	}

	return openaiReq, nil
}

// AdaptResponse 将 OpenAI 响应转换为 Gemini 响应
func (a *GeminiToOpenAIAdapter) AdaptResponse(respData map[string]interface{}) (map[string]interface{}, error) {
	geminiResp := make(map[string]interface{})

	// 转换 choices 为 candidates
	candidates := make([]interface{}, 0)

	if choices, ok := respData["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			candidate := make(map[string]interface{})

			// 提取内容
			parts := make([]interface{}, 0)

			if message, ok := choice["message"].(map[string]interface{}); ok {
				// 文本内容
				if content, ok := message["content"].(string); ok && content != "" {
					parts = append(parts, map[string]interface{}{
						"text": content,
					})
				}

				// 工具调用
				if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
					for _, tc := range toolCalls {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if function, ok := tcMap["function"].(map[string]interface{}); ok {
								name, _ := function["name"].(string)
								arguments, _ := function["arguments"].(string)

								var args map[string]interface{}
								if err := json.Unmarshal([]byte(arguments), &args); err != nil {
									args = map[string]interface{}{}
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
			}

			candidate["content"] = map[string]interface{}{
				"role":  "model",
				"parts": parts,
			}

			// 转换 finish_reason
			if finishReason, ok := choice["finish_reason"].(string); ok {
				switch finishReason {
				case "stop":
					candidate["finishReason"] = "STOP"
				case "length":
					candidate["finishReason"] = "MAX_TOKENS"
				case "tool_calls":
					candidate["finishReason"] = "STOP"
				case "content_filter":
					candidate["finishReason"] = "SAFETY"
				default:
					candidate["finishReason"] = "STOP"
				}
			}

			candidate["index"] = 0
			candidates = append(candidates, candidate)
		}
	}

	geminiResp["candidates"] = candidates

	// 转换 usage
	if usage, ok := respData["usage"].(map[string]interface{}); ok {
		promptTokens := 0
		completionTokens := 0
		if pt, ok := usage["prompt_tokens"].(float64); ok {
			promptTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok {
			completionTokens = int(ct)
		}
		geminiResp["usageMetadata"] = map[string]interface{}{
			"promptTokenCount":     promptTokens,
			"candidatesTokenCount": completionTokens,
			"totalTokenCount":      promptTokens + completionTokens,
		}
	}

	return geminiResp, nil
}

// AdaptStreamChunk 转换流式响应块 - Gemini SSE → OpenAI SSE
func (a *GeminiToOpenAIAdapter) AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error) {
	// Gemini 流式响应格式: {"candidates": [{"content": {"parts": [{"text": "..."}]}}]}

	if candidates, ok := chunk["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			// 提取文本内容
			var textContent string
			var finishReason interface{} = nil

			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if text, ok := partMap["text"].(string); ok {
								textContent += text
							}
						}
					}
				}
			}

			// 检查 finishReason
			if fr, ok := candidate["finishReason"].(string); ok && fr != "" {
				switch fr {
				case "STOP":
					finishReason = "stop"
				case "MAX_TOKENS":
					finishReason = "length"
				case "SAFETY", "RECITATION":
					finishReason = "content_filter"
				default:
					finishReason = "stop"
				}
			}

			// 构建 OpenAI 格式的流式响应
			openaiChunk := map[string]interface{}{
				"id":      "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   "gemini",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{},
						"finish_reason": finishReason,
					},
				},
			}

			// 只有当有文本内容时才添加到 delta
			if textContent != "" {
				choices := openaiChunk["choices"].([]interface{})
				choice := choices[0].(map[string]interface{})
				choice["delta"] = map[string]interface{}{
					"content": textContent,
				}
			}

			return openaiChunk, nil
		}
	}

	return nil, nil
}

// AdaptStreamStart 流式响应开始
func (a *GeminiToOpenAIAdapter) AdaptStreamStart(model string) []map[string]interface{} {
	return nil
}

// AdaptStreamEnd 流式响应结束
func (a *GeminiToOpenAIAdapter) AdaptStreamEnd() []map[string]interface{} {
	return nil
}
