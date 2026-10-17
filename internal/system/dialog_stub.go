//go:build !windows
// +build !windows

package system

import (
	"fmt"
)

// ShowErrorDialog 显示错误对话框 (非 Windows 平台使用控制台输出)
func ShowErrorDialog(title, message string) {
	fmt.Printf("ERROR: %s\n%s\n", title, message)
}

// ShowWarningDialog 显示警告对话框 (非 Windows 平台使用控制台输出)
func ShowWarningDialog(title, message string) {
	fmt.Printf("WARNING: %s\n%s\n", title, message)
}
