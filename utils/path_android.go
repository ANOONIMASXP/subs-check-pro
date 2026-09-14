// utils/path_android.go
//go:build android

package utils

import (
	"log/slog"
	"os"
	"path/filepath"
)

// GetExecutablePath 返回可执行文件所在目录。
//
// Android 下不再使用 App 沙盒（/data/data/<包名>/files），
// 而是让 config、output 等目录与程序可执行文件同级生成。
func GetExecutablePath() string {
	ex, err := os.Executable()
	if err != nil {
		slog.Error("获取程序路径失败", "error", err)
		return "."
	}
	return filepath.Dir(ex)
}
