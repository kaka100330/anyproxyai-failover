package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// SignatureEntry 签名存储条目
type SignatureEntry struct {
	Signature string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// SessionSignatureStore 会话级签名存储，支持并发安全
type SessionSignatureStore struct {
	store map[string]*SignatureEntry
	mu    sync.RWMutex
	ttl   time.Duration
}

// 全局签名存储实例
var globalSessionStore = &SessionSignatureStore{
	store: make(map[string]*SignatureEntry),
	ttl:   1 * time.Hour, // 默认1小时过期
}

// 后台清理任务
func init() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			globalSessionStore.cleanup()
		}
	}()
}

// GenerateSessionID 生成会话ID（基于消息内容的哈希）
func GenerateSessionID(messages []interface{}) string {
	if len(messages) == 0 {
		return ""
	}

	// 使用前几条消息生成稳定的会话ID
	// 这样同一对话的请求会有相同的session_id
	var contentParts []string
	maxMessages := 3 // 只使用前3条消息
	if len(messages) < maxMessages {
		maxMessages = len(messages)
	}

	for i := 0; i < maxMessages; i++ {
		if msgMap, ok := messages[i].(map[string]interface{}); ok {
			role, _ := msgMap["role"].(string)
			content := extractContentForHash(msgMap["content"])
			contentParts = append(contentParts, role+":"+content)
		}
	}

	// 生成哈希
	hash := sha256.Sum256([]byte(strings.Join(contentParts, "|")))
	return hex.EncodeToString(hash[:])[:16] // 取前16个字符
}

// extractContentForHash 提取内容用于哈希（辅助函数）
func extractContentForHash(content interface{}) string {
	switch c := content.(type) {
	case string:
		// 限制长度避免哈希过长内容
		if len(c) > 200 {
			return c[:200]
		}
		return c
	case []interface{}:
		// 提取第一个文本块
		for _, item := range c {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemMap["type"] == "text" {
					if text, ok := itemMap["text"].(string); ok {
						if len(text) > 200 {
							return text[:200]
						}
						return text
					}
				}
			}
		}
	}
	// 序列化为JSON作为备选
	if jsonBytes, err := json.Marshal(content); err == nil {
		s := string(jsonBytes)
		if len(s) > 200 {
			return s[:200]
		}
		return s
	}
	return ""
}

// StoreSignatureForSession 为会话存储签名
func StoreSignatureForSession(sessionID string, signature string) {
	if sessionID == "" || signature == "" {
		return
	}

	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()

	entry := globalSessionStore.store[sessionID]
	// 只有新签名更长时才更新
	if entry == nil || len(signature) > len(entry.Signature) {
		globalSessionStore.store[sessionID] = &SignatureEntry{
			Signature: signature,
			ExpiresAt: time.Now().Add(globalSessionStore.ttl),
			CreatedAt: time.Now(),
		}
		log.Debugf("[SigStore] Stored signature for session %s (len=%d)", sessionID[:8], len(signature))
	}
}

// GetSignatureForSession 获取会话的签名
func GetSignatureForSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}

	globalSessionStore.mu.RLock()
	defer globalSessionStore.mu.RUnlock()

	entry, ok := globalSessionStore.store[sessionID]
	if !ok {
		return ""
	}

	// 检查是否过期
	if time.Now().After(entry.ExpiresAt) {
		return ""
	}

	return entry.Signature
}

// ClearSignatureForSession 清除会话的签名
func ClearSignatureForSession(sessionID string) {
	if sessionID == "" {
		return
	}

	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()

	delete(globalSessionStore.store, sessionID)
	log.Debugf("[SigStore] Cleared signature for session %s", sessionID[:8])
}

// cleanup 清理过期的签名条目
func (s *SessionSignatureStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	expired := make([]string, 0)

	for sessionID, entry := range s.store {
		if now.After(entry.ExpiresAt) {
			expired = append(expired, sessionID)
		}
	}

	for _, sessionID := range expired {
		delete(s.store, sessionID)
	}

	if len(expired) > 0 {
		log.Debugf("[SigStore] Cleaned %d expired signature(s)", len(expired))
	}
}

// GetStoreStats 获取存储统计信息（用于调试）
func GetStoreStats() map[string]interface{} {
	globalSessionStore.mu.RLock()
	defer globalSessionStore.mu.RUnlock()

	return map[string]interface{}{
		"total_sessions": len(globalSessionStore.store),
		"ttl_seconds":    globalSessionStore.ttl.Seconds(),
	}
}

// 兼容性函数 - 保持向后兼容旧的全局函数
// 但内部使用会话存储（使用空字符串作为默认会话ID）

const defaultSessionID = "default"

// StoreThoughtSignature 存储 thought signature（向后兼容）
func StoreThoughtSignature(sig string) {
	StoreSignatureForSession(defaultSessionID, sig)
}

// GetThoughtSignature 获取存储的 thought signature（向后兼容）
func GetThoughtSignature() string {
	return GetSignatureForSession(defaultSessionID)
}

// ClearThoughtSignature 清除存储的 thought signature（向后兼容）
func ClearThoughtSignature() {
	ClearSignatureForSession(defaultSessionID)
}
