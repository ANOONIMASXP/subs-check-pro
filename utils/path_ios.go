//go:build ios

package utils

import (
	"log/slog"
	"os"
	"path/filepath"
)

// GetPrivateStorageDir 返回内部存储目录（与可执行文件同级）。
func GetPrivateStorageDir() string {
	ex, err := os.Executable()
	if err != nil {
		slog.Error("获取程序路径失败", "error", err)
		return "."
	}
	return filepath.Dir(ex)
}

// GetExternalStorageDir 返回外部存储目录（iOS 无外部存储 → 与内部一致）。
func GetExternalStorageDir() string {
	return GetPrivateStorageDir()
}
