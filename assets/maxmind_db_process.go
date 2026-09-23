// Package assets 嵌入的资产文件
package assets

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/sinspired/subs-check-pro/v3/save/method"
)

// OpenMaxMindDB 使用指定路径或默认路径打开 MaxMind 数据库
func OpenMaxMindDB(dbPath string) (*maxminddb.Reader, error) {
	if dbPath != "" {
		return openDBWithArch(dbPath)
	}
	mmdbPath, err := resolveDBPath()
	if err != nil {
		return nil, err
	}

	// 确保磁盘数据库与内嵌版本一致（内嵌更新则覆盖，缺失则解压）
	if err := ensureGeoLite2(mmdbPath); err != nil {
		return nil, err
	}

	return openDBWithArch(mmdbPath)
}

// 根据 GOARCH 选择合适的打开方式
func openDBWithArch(path string) (*maxminddb.Reader, error) {
	if runtime.GOARCH == "386" {
		return openFromBytes(path)
	}
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("maxmind数据库打开失败: %w", err)
	}
	return db, nil
}

// ensureGeoLite2 确保磁盘上的 GeoLite2 数据库与内嵌版本一致：
// 内嵌版本更新（或本地无版本号/数据库文件缺失）时，重新解压覆盖。
func ensureGeoLite2(mmdbPath string) error {
	verPath := filepath.Join(filepath.Dir(mmdbPath), "geolite2.version")

	embeddedVer := strings.TrimSpace(string(EmbeddedGeoLite2Version))
	localVer := ""
	if b, err := os.ReadFile(verPath); err == nil {
		localVer = strings.TrimSpace(string(b))
	}

	// 内嵌版本更新则覆盖；否则仅在数据库文件缺失时解压
	if !geoLite2VersionNewer(embeddedVer, localVer) {
		if _, err := os.Stat(mmdbPath); err == nil {
			return nil
		}
	}

	if err := decompressEmbeddedMMDB(mmdbPath); err != nil {
		return err
	}

	if embeddedVer != "" {
		if err := os.WriteFile(verPath, []byte(embeddedVer), 0o644); err != nil {
			slog.Warn("写入 GeoLite2 版本号失败", "error", err)
		} else {
			slog.Info("GeoLite2 数据库已更新", "version", embeddedVer, "path", mmdbPath)
		}
	}
	return nil
}

// geoLite2VersionNewer 判断内嵌版本是否比本地新。
// 版本形如 YYYY.MM.DD，零填充，字典序即为时间序。
func geoLite2VersionNewer(embedded, local string) bool {
	if embedded == "" {
		return false
	}
	if local == "" {
		return true
	}
	return strings.Compare(embedded, local) > 0
}

// 解压内置的 MaxMind 数据库到指定路径
func decompressEmbeddedMMDB(targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("创建数据库目录失败: %w", err)
	}

	zstdDecoder, err := zstd.NewReader(nil)
	if err != nil {
		return fmt.Errorf("zstd解码器创建失败: %w", err)
	}
	defer zstdDecoder.Close()

	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("maxmind数据库文件创建失败: %w", err)
	}
	defer file.Close()

	if err := zstdDecoder.Reset(bytes.NewReader(EmbeddedMaxMindDB)); err != nil {
		return err
	}

	if _, err := io.Copy(file, zstdDecoder); err != nil {
		return fmt.Errorf("maxmind数据库文件解压失败: %w", err)
	}

	return nil
}

// 解析数据库存放路径
func resolveDBPath() (string, error) {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return "", err
	}

	if !filepath.IsAbs(saver.OutputPath) {
		saver.OutputPath = filepath.Join(saver.BasePath, saver.OutputPath)
	}

	if err := os.MkdirAll(saver.OutputPath, 0o755); err != nil {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("无法获取当前工作目录: %w", err)
		}
		saver.OutputPath = filepath.Join(cwd, "output")
		if err := os.MkdirAll(saver.OutputPath, 0o755); err != nil {
			return "", fmt.Errorf("无法创建输出目录: %w", err)
		}
	}

	maxminddbDir := filepath.Join(saver.OutputPath, "MaxMindData")

	if err := os.MkdirAll(maxminddbDir, 0o755); err != nil {
		return "", fmt.Errorf("无法创建 MaxMind 输出目录: %w", err)
	}
	return filepath.Join(maxminddbDir, "GeoLite2-Country.mmdb"), nil
}

// 32 位程序使用从内存读取的方式
func openFromBytes(path string) (*maxminddb.Reader, error) {
	runtime.GC() // 释放内存

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件到内存失败: %w", err)
	}

	reader, err := maxminddb.OpenBytes(data)
	if err != nil {
		return nil, fmt.Errorf("从字节数组创建reader失败: %w", err)
	}
	return reader, nil
}
