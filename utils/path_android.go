//utils/path_android.go
//go:build android

package utils

import (
	"os"
	"path/filepath"
	"strings"
)

// GetExecutablePath 获取 Android App 专属的可读写沙盒目录
func GetExecutablePath() string {
	// 1. 读取 /proc/self/cmdline 获取当前进程包名 (如 com.wails.app)
	if data, err := os.ReadFile("/proc/self/cmdline"); err == nil {
		pkgName := strings.Trim(string(data), "\x00\r\n\t ")
		if idx := strings.IndexByte(pkgName, 0); idx != -1 {
			pkgName = pkgName[:idx]
		}
		if pkgName != "" {
			// Android 私有存储目录：/data/data/<包名>/files
			appDir := filepath.Join("/data/data", pkgName, "files")
			if err := os.MkdirAll(appDir, 0755); err == nil {
				return appDir
			}
		}
	}

	// 2. 兜底方案
	if wd, err := os.Getwd(); err == nil && wd != "/" && wd != "" {
		return wd
	}

	return "."
}