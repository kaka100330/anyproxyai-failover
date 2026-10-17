//go:build windows
// +build windows

package system

import (
	"fmt"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows/registry"
)

const (
	registryPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	appName      = "AnyProxyAi"
)

type AutoStart struct{}

func NewAutoStart() *AutoStart {
	return &AutoStart{}
}

// EnableAutoStart 启用开机自启动
func (a *AutoStart) EnableAutoStart() error {
	exePath, err := os.Executable()
	if err != nil {
		log.Errorf("Failed to get executable path: %v", err)
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	absPath, err := filepath.Abs(exePath)
	if err != nil {
		log.Errorf("Failed to get absolute path: %v", err)
		return fmt.Errorf("failed to get absolute path: %v", err)
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, registryPath, registry.SET_VALUE)
	if err != nil {
		log.Errorf("Failed to open registry key: %v", err)
		return fmt.Errorf("failed to open registry key: %v", err)
	}
	defer key.Close()

	// 设置注册表值
	err = key.SetStringValue(appName, absPath)
	if err != nil {
		log.Errorf("Failed to set registry value: %v", err)
		return fmt.Errorf("failed to set registry value: %v", err)
	}

	log.Infof("Auto-start enabled: %s -> %s", appName, absPath)
	return nil
}

// DisableAutoStart 禁用开机自启动
func (a *AutoStart) DisableAutoStart() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, registryPath, registry.SET_VALUE)
	if err != nil {
		log.Errorf("Failed to open registry key: %v", err)
		return fmt.Errorf("failed to open registry key: %v", err)
	}
	defer key.Close()

	// 删除注册表值
	err = key.DeleteValue(appName)
	if err != nil {
		// 如果值不存在，也认为是成功的
		if err == registry.ErrNotExist {
			log.Info("Auto-start was not enabled")
			return nil
		}
		log.Errorf("Failed to delete registry value: %v", err)
		return fmt.Errorf("failed to delete registry value: %v", err)
	}

	log.Infof("Auto-start disabled: %s", appName)
	return nil
}

// IsAutoStartEnabled 检查是否已启用开机自启动
func (a *AutoStart) IsAutoStartEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, registryPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()

	// 读取注册表值
	_, _, err = key.GetStringValue(appName)
	return err == nil
}
