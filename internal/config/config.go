package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	Host                  string `json:"host"`
	Port                  int    `json:"port"`
	DatabasePath          string `json:"database_path"`
	LocalAPIKey           string `json:"local_api_key"`
	RedirectEnabled       bool   `json:"redirect_enabled"`
	RedirectKeyword       string `json:"redirect_keyword"`
	RedirectTargetModel   string `json:"redirect_target_model"`
	RedirectTargetName    string `json:"redirect_target_name"`
	RedirectTargetRouteID int64  `json:"redirect_target_route_id"`
	RedirectTargetGroupID int64  `json:"redirect_target_group_id"`
	MinimizeToTray        bool   `json:"minimize_to_tray"`
	AutoStart             bool   `json:"auto_start"`
	EnableFileLog         bool   `json:"enable_file_log"`
	SanitizeThinking      bool   `json:"sanitize_thinking"`
	Language              string `json:"language"`

	// Web 管理界面（前后端分离部署时使用）
	WebRoot       string `json:"web_root"`
	PublicBaseURL string `json:"public_base_url"`
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
	SessionSecret string `json:"session_secret"`

	configPath string
}

func envString(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func envBool(key string, dst *bool) {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func LoadConfig() *Config {
	configPath := "config.json"

	cfg := &Config{
		Host:                  "localhost",
		Port:                  5642,
		DatabasePath:          "routes.db",
		LocalAPIKey:           "sk-local-default-key",
		RedirectEnabled:       false,
		RedirectKeyword:       "proxy_auto",
		RedirectTargetModel:   "",
		RedirectTargetName:    "",
		RedirectTargetRouteID: 0,
		MinimizeToTray:        true,
		AutoStart:             false,
		EnableFileLog:         false,
		SanitizeThinking:      true,
		Language:              "en-US",
		WebRoot:               "web",
		AdminUsername:         "admin",
		configPath:            configPath,
	}

	// 尝试从文件加载配置
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Warnf("Failed to parse config file: %v", err)
		} else {
			log.Info("Configuration loaded from config.json")
		}
	} else {
		log.Info("Config file not found, using default configuration")
	}

	// 环境变量优先（便于 Docker / systemd 部署）
	envString("ANYPROXY_HOST", &cfg.Host)
	envInt("ANYPROXY_PORT", &cfg.Port)
	envString("ANYPROXY_DB", &cfg.DatabasePath)
	envString("ANYPROXY_API_KEY", &cfg.LocalAPIKey)
	envString("ANYPROXY_WEB_ROOT", &cfg.WebRoot)
	envString("ANYPROXY_PUBLIC_BASE_URL", &cfg.PublicBaseURL)
	envString("ANYPROXY_ADMIN_USER", &cfg.AdminUsername)
	envString("ANYPROXY_ADMIN_PASSWORD", &cfg.AdminPassword)
	envString("ANYPROXY_SESSION_SECRET", &cfg.SessionSecret)
	envBool("ANYPROXY_SANITIZE_THINKING", &cfg.SanitizeThinking)
	envBool("ANYPROXY_ENABLE_FILE_LOG", &cfg.EnableFileLog)

	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "admin"
	}
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = randomHex(32)
	}
	generatedPassword := false
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = randomHex(8)
		generatedPassword = true
	}
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = "http://127.0.0.1:" + strconv.Itoa(cfg.Port)
	}

	// 持久化（首次运行会写出默认配置；环境变量覆盖也会落盘，方便下次启动）
	if err := cfg.Save(); err != nil {
		log.Warnf("Failed to save config: %v", err)
	}
	if generatedPassword {
		log.Warnf("============================================================")
		log.Warnf("Web admin credentials generated: username=%s password=%s", cfg.AdminUsername, cfg.AdminPassword)
		log.Warnf("Change them in config.json or via ANYPROXY_ADMIN_PASSWORD.")
		log.Warnf("============================================================")
	}

	return cfg
}

func (c *Config) Save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.configPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return os.WriteFile(c.configPath, data, 0600)
}
