package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sinspired/subs-check-pro/v3/save/method"
)

func GetLogPath() (string, error) {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return "", fmt.Errorf("创建本地保存器失败: %w", err)
	}

	// 确保 log 目录存在
	logDir := filepath.Join(saver.OutputPath, "log")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("创建日志目录失败: %w", err)
	}

	return filepath.Join(logDir, "subs-check-pro.log"), nil
}
