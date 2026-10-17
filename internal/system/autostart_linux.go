//go:build linux
// +build linux

package system

import (
	"fmt"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

const (
	desktopTemplate = `[Desktop Entry]
Type=Application
Name=AnyProxyAi
Comment=AnyProxyAi - API 代理管理器
Exec=%s
Icon=anyproxyai
Terminal=false
Categories=Utility;Network;
StartupNotify=false
X-GNOME-Autostart-enabled=true
`
	appName = "anyproxyai.desktop"
)

type AutoStart struct{}

func NewAutoStart() *AutoStart {
	return &AutoStart{}
}

// EnableAutoStart enables the application to start on Linux boot using autostart
func (a *AutoStart) EnableAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		log.Errorf("Failed to get executable path: %v", err)
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	exePath, err = filepath.Abs(exePath)
	if err != nil {
		log.Errorf("Failed to get absolute path: %v", err)
		return fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Get user's autostart directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Errorf("Failed to get home directory: %v", err)
		return fmt.Errorf("failed to get home directory: %v", err)
	}

	// Try XDG_CONFIG_HOME first, fallback to ~/.config
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(homeDir, ".config")
	}

	autostartDir := filepath.Join(configDir, "autostart")
	if err := os.MkdirAll(autostartDir, 0755); err != nil {
		log.Errorf("Failed to create autostart directory: %v", err)
		return fmt.Errorf("failed to create autostart directory: %v", err)
	}

	desktopFilePath := filepath.Join(autostartDir, appName)

	// Create desktop entry content
	desktopContent := fmt.Sprintf(desktopTemplate, exePath)

	// Write desktop entry file
	if err := os.WriteFile(desktopFilePath, []byte(desktopContent), 0644); err != nil {
		log.Errorf("Failed to write desktop entry file: %v", err)
		return fmt.Errorf("failed to write desktop entry file: %v", err)
	}

	log.Infof("Auto-start enabled: %s -> %s", appName, exePath)
	return nil
}

// DisableAutoStart disables the application from starting on Linux boot
func (a *AutoStart) DisableAutoStart() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Errorf("Failed to get home directory: %v", err)
		return fmt.Errorf("failed to get home directory: %v", err)
	}

	// Try XDG_CONFIG_HOME first, fallback to ~/.config
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(homeDir, ".config")
	}

	desktopFilePath := filepath.Join(configDir, "autostart", appName)

	// Remove the desktop entry file
	if err := os.Remove(desktopFilePath); err != nil && !os.IsNotExist(err) {
		log.Errorf("Failed to remove desktop entry file: %v", err)
		return fmt.Errorf("failed to remove desktop entry file: %v", err)
	}

	log.Infof("Auto-start disabled: %s", appName)
	return nil
}

// IsAutoStartEnabled checks if auto-start is currently enabled
func (a *AutoStart) IsAutoStartEnabled() bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(homeDir, ".config")
	}

	desktopFilePath := filepath.Join(configDir, "autostart", appName)
	_, err = os.Stat(desktopFilePath)
	return err == nil
}
