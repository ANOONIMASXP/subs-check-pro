// utils/path_android.go
//go:build android

package utils

import (
	"log/slog"
	"os"
	"path/filepath"
)

// GetPrivateStorageDir 返回内部存储目录。
//
// Android 下不再使用 App 沙盒（/data/data/<包名>/files），
// 而是让 config、output 等目录与程序可执行文件同级生成。
func GetPrivateStorageDir() string {
	ex, err := os.Executable()
	if err != nil {
		slog.Error("获取程序路径失败", "error", err)
		return "."
	}
	return filepath.Dir(ex)
}

// GetExternalStorageDir 返回外部存储目录（Android CLI 场景与可执行文件同级）。
func GetExternalStorageDir() string {
	return GetPrivateStorageDir()
}
