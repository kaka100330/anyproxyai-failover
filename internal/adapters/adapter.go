package adapters

// Adapter 接口定义
type Adapter interface {
	AdaptRequest(request map[string]interface{}, targetModel string) (map[string]interface{}, error)
	AdaptResponse(response map[string]interface{}) (map[string]interface{}, error)
	AdaptStreamChunk(chunk map[string]interface{}) (map[string]interface{}, error)
	AdaptStreamStart(model string) []map[string]interface{}
	AdaptStreamEnd() []map[string]interface{}
}

// 适配器注册表
var adapterRegistry = make(map[string]Adapter)

// RegisterAdapter 注册适配器
func RegisterAdapter(name string, adapter Adapter) {
	adapterRegistry[name] = adapter
}

// GetAdapter 获取适配器
func GetAdapter(name string) Adapter {
	if adapter, ok := adapterRegistry[name]; ok {
		return adapter
	}
	return nil
}

func init() {
	// 注册所有适配器
	RegisterAdapter("anthropic", &AnthropicAdapter{})
	RegisterAdapter("gemini", &GeminiAdapter{})
	RegisterAdapter("deepseek", &DeepSeekAdapter{})
	RegisterAdapter("openai-to-claude", &OpenAIToClaudeAdapter{})
}
