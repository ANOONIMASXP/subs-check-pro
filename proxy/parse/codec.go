package parse

import (
	"encoding/base64"
	"fmt"
	"net"
	"path/filepath"
	"strings"
)

// TryDecodeBase64 尝试 Base64 解码，失败则返回原数据
func TryDecodeBase64(data []byte) []byte {
	decoded, err := decodeBase64(cleanBase64(string(data)))
	if err != nil {
		return data
	}
	return decoded
}

// TryDecodeBase64WithError 解码 Base64 字符串，失败返回错误
func TryDecodeBase64WithError(s string) ([]byte, error) {
	return decodeBase64(cleanBase64(s))
}

// cleanBase64 清洗 Base64 字符串中的空白字符
func cleanBase64(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, s)
}

// decodeBase64 核心解码逻辑，按长度分支选择解码器
func decodeBase64(s string) ([]byte, error) {
	if len(s) == 0 {
		return nil, fmt.Errorf("empty input")
	}

	if len(s)%4 == 0 {
		// 有 Padding 或完整块
		if b, err := base64.StdEncoding.DecodeString(s); err == nil {
			return b, nil
		}
		if b, err := base64.URLEncoding.DecodeString(s); err == nil {
			return b, nil
		}
	} else {
		// 缺失 Padding，优先 Raw 解码器
		if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
			return b, nil
		}
		if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
			return b, nil
		}
		// 兜底：手动补齐 Padding 再试
		padded := s + strings.Repeat("=", 4-len(s)%4)
		if b, err := base64.StdEncoding.DecodeString(padded); err == nil {
			return b, nil
		}
		if b, err := base64.URLEncoding.DecodeString(padded); err == nil {
			return b, nil
		}
	}

	return nil, fmt.Errorf("invalid base64 input")
}

// guessSchemeByURL 根据 URL 文件名猜测协议
func guessSchemeByURL(raw string) string {
	pathStr := raw
	if idx := strings.Index(pathStr, "://"); idx >= 0 {
		pathStr = pathStr[idx+3:]
	}
	if idx := strings.IndexAny(pathStr, "?#"); idx >= 0 {
		pathStr = pathStr[:idx]
	}
	filename := strings.ToLower(filepath.Base(pathStr))
	if idx := strings.LastIndexByte(filename, '.'); idx > 0 {
		filename = filename[:idx]
	}

	for _, key := range sortedProtocolKeys {
		if strings.Contains(filename, key) {
			if _, ok := protocolSchemes[key]; ok {
				return key
			}
			if key == "http2" {
				return "https"
			}
		}
	}
	if strings.Contains(filename, "all") {
		return "all"
	}
	return ""
}

// SplitHostPortLoose 宽容的 host:port 分割，兼容 IPv6
func SplitHostPortLoose(hp string) (string, string) {
	if hp == "" {
		return "", ""
	}
	if host, port, err := net.SplitHostPort(hp); err == nil {
		return host, port
	}
	lastColon := strings.LastIndexByte(hp, ':')
	if lastColon > 0 && lastColon < len(hp)-1 {
		if hp[len(hp)-1] == ']' {
			return hp, "" // 纯 IPv6，无端口
		}
		return hp[:lastColon], hp[lastColon+1:]
	}
	return hp, ""
}
