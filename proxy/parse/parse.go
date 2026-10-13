package parse

import (
	"bufio"
	"bytes"
	"log/slog"
	"strings"
)

// 协议映射表：Key 为常见的缩写或别名，Value 为标准协议头
var protocolSchemes = map[string]string{
	// Hysteria
	"hysteria2": "hysteria2://", "hy2": "hysteria2://",
	"hysteria": "hysteria://", "hy": "hysteria://",
	// Standard
	"http": "http://", "https": "https://",
	"socks5": "socks5://", "socks5h": "socks5h://", "socks4": "socks4://", "socks": "socks://",
	// V2Ray / Others
	"vmess": "vmess://", "vless": "vless://",
	"trojan":      "trojan://",
	"shadowsocks": "ss://", "ss": "ss://", "ssr": "ssr://",
	"tuic": "tuic://", "tuic5": "tuic://",
	"juicity":   "juicity://",
	"wireguard": "wireguard://", "wg": "wg://",
	"mieru":       "mierus://",
	"anytls":      "anytls://",
	"openvpn":     "openvpn://",
	"snell":       "snell://",
	"ssh":         "ssh://",
	"sudoku":      "sudoku://",
	"masque":      "masque://",
	"trusttunnel": "trusttunnel://",
	// tailscale 无标准 URI 格式，仅靠 YAML 配置，不加入映射
}

// 越长的关键字越靠前，防止短前缀错误匹配（如 "ss" 匹配到 "ssr"）
var sortedProtocolKeys = []string{
	"shadowsocks", "trusttunnel", "hysteria2", "wireguard", "juicity",
	"hysteria", "socks5h", "socks4", "socks5", "openvpn", "sudoku",
	"masque", "vless", "vmess", "trojan", "https", "http2", "tuic3",
	"mieru", "anytls", "snell",
	"http", "tuic", "ssr", "ssh", "hy2", "ss", "wg", "hy",
}

// newLineScanner 构造行扫描器，token 上限 2MB。
// bufio.Scanner 默认 64KB：单行超限后 Scan() 直接停止，
// 该行之后的所有内容被静默丢弃（minified/单行订阅实踩过）。
func newLineScanner(data []byte) *bufio.Scanner {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	return scanner
}

// warnScanErr 扫描循环结束后必须检查 Err：截断是静默的，只有这里能暴露
func warnScanErr(scanner *bufio.Scanner, stage string) {
	if err := scanner.Err(); err != nil {
		slog.Warn("读取订阅行中断（超长行之后的内容可能被截断）", "阶段", stage, "error", err)
	}
}

// coveredByStdConvert 判断一行是否已被 convert.ConvertsV2Ray(整份数据) 处理过，
// 且在 ParseProxyLinksAndConvert 中也只会走同一个 convert.ConvertsV2Ray：
//
//   - 必须以字母开头、紧跟合法的 scheme:// —— 带前导空白/短横线（"- vless://…"）的行，
//     整体转换认不出来，只有逐行路径会去掉前缀，所以不算；
//   - wireguard/wg/ssr 由专用解析器处理，mieru 由 ConvertsV2RayExtra 处理，
//     hy/hy2 会被 FixupProxyLink 改写头部，这些都不算。
func coveredByStdConvert(raw string) bool {
	if raw == "" {
		return false
	}
	if c := raw[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return false
	}
	i := strings.Index(raw, "://")
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := raw[j]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	switch strings.ToLower(raw[:i]) {
	case "wireguard", "wg", "ssr", "mieru", "hy", "hy2":
		return false
	}
	return true
}

// parseRawLinesOpt 读取纯文本行并交给统一解析器。
//
// skipStd=true 表示调用方刚刚已对「整份数据」成功执行过 convert.ConvertsV2Ray：
// 其中标准 scheme:// 的行已经转换过了，这里再转换一遍只会产出相同的节点，
// 被调用方的跨解析器去重丢弃，纯属重复劳动（对逐行链接订阅，CPU 与内存开销都翻倍）。
// 因此只保留 ConvertsV2Ray 处理不了的行：无协议头（ip:port、base64 片段）、
// 带前导前缀、wg/ssr/mieru/hy 等非标写法。
func parseRawLinesOpt(data []byte, subURL string, skipStd bool) ([]map[string]any, int) {
	scanner := newLineScanner(data)
	var lines []string
	for scanner.Scan() {
		raw := scanner.Text()
		if skipStd && coveredByStdConvert(raw) {
			continue
		}
		line := strings.TrimSpace(raw)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, strings.TrimLeft(line, "- "))
		}
	}
	warnScanErr(scanner, "行级解析")
	if len(lines) == 0 {
		return nil, 0
	}
	return ParseProxyLinksAndConvert(lines, subURL, skipStd)
}

// FallbackExtractV2Ray 正则提取兜底
func FallbackExtractV2Ray(data []byte, subURL string) []map[string]any {
	decodedData := TryDecodeBase64(data)
	// 只记长度：string(decodedData) 会在参数求值时无条件复制整份解码结果
	// （最大 100MB），即使 Debug 级别被关闭也照付代价
	slog.Debug("base64解码", "bytes", len(decodedData))
	links := ExtractV2RayLinks(decodedData)
	if len(links) == 0 {
		return nil
	}
	slog.Debug("正则提取链接", "数量", len(links), "URL", subURL)
	nodes, _ := ParseProxyLinksAndConvert(links, subURL, false) // ← 兜底路径，忽略去重数
	return nodes
}

// ExtractClashProviderURLs 从 Clash/Mihomo 配置中提取 proxy-providers 的 url
func ExtractClashProviderURLs(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := []string{"proxy-providers", "proxy_providers", "proxyproviders"}
	out := make([]string, 0, 8)
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		providers, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for _, prov := range providers {
			pm, ok := prov.(map[string]any)
			if !ok {
				continue
			}
			if u, ok := pm["url"].(string); ok {
				u = strings.TrimSpace(u)
				if u != "" {
					out = append(out, u)
				}
			}
		}
	}
	return out
}
