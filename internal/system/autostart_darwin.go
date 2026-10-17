//go:build darwin
// +build darwin

package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

const (
	plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>`
	appLabel = "com.anyproxyai.app"
)

type AutoStart struct{}

func NewAutoStart() *AutoStart {
	return &AutoStart{}
}

// EnableAutoStart enables the application to start on macOS boot using LaunchAgents
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

	// Get user's LaunchAgents directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Errorf("Failed to get home directory: %v", err)
		return fmt.Errorf("failed to get home directory: %v", err)
	}

	launchAgentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		log.Errorf("Failed to create LaunchAgents directory: %v", err)
		return fmt.Errorf("failed to create LaunchAgents directory: %v", err)
	}

	plistPath := filepath.Join(launchAgentsDir, appLabel+".plist")

	// Create plist content
	plistContent := fmt.Sprintf(plistTemplate, appLabel, exePath)

	// Write plist file
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		log.Errorf("Failed to write plist file: %v", err)
		return fmt.Errorf("failed to write plist file: %v", err)
	}

	// Load the plist using launchctl
	cmd := exec.Command("launchctl", "load", plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Errorf("Failed to load plist: %v, output: %s", err, string(output))
		// Don't return error if it's already loaded
		if !strings.Contains(string(output), "Already loaded") {
			return fmt.Errorf("failed to load plist: %v", err)
		}
	}

	log.Infof("Auto-start enabled: %s -> %s", appLabel, exePath)
	return nil
}

// DisableAutoStart disables the application from starting on macOS boot
func (a *AutoStart) DisableAutoStart() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Errorf("Failed to get home directory: %v", err)
		return fmt.Errorf("failed to get home directory: %v", err)
	}

	launchAgentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	plistPath := filepath.Join(launchAgentsDir, appLabel+".plist")

	// Unload the plist using launchctl
	cmd := exec.Command("launchctl", "unload", plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Errorf("Failed to unload plist: %v, output: %s", err, string(output))
		// Don't return error if it's not loaded
		if !strings.Contains(string(output), "Could not find") {
			return fmt.Errorf("failed to unload plist: %v", err)
		}
	}

	// Remove the plist file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		log.Errorf("Failed to remove plist file: %v", err)
		return fmt.Errorf("failed to remove plist file: %v", err)
	}

	log.Infof("Auto-start disabled: %s", appLabel)
	return nil
}

// IsAutoStartEnabled checks if auto-start is currently enabled
func (a *AutoStart) IsAutoStartEnabled() bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", appLabel+".plist")
	_, err = os.Stat(plistPath)
	return err == nil
}
