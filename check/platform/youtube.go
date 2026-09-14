package platform

import (
	"io"
	"net/http"
	"regexp"
	"strings"
)

// reYoutubeGL 提取 INNERTUBE_CONTEXT_GL 国家码（大写 ISO 3166-1 alpha-2）
var reYoutubeGL = regexp.MustCompile(`"INNERTUBE_CONTEXT_GL"\s*:\s*"([A-Z]{2})"`)

// cnSignals CN 封锁信号集，任意命中即判定为 CN
// www.google.cn  —— 送中跳转残留链接
// "gl":"CN"      —— JSON state 中的地区标记（小写 key，部分页面）
// youtube.com/unsupported_browser —— CN IP 常见落地页之一
var cnSignals = []string{
	"www.google.cn",
	`"gl":"CN"`,
	`youtube.com/unsupported_browser`,
}

// CheckYoutube 检测 YouTube 可访问性及所在地区
//
// 返回值：
//   - ISO 3166-1 alpha-2 国家码（如 "US"、"HK"）→ 可访问，Premium 可用
//   - "CN"   → CN IP，YouTube 不可用
//   - "BLOCKED" → 可访问但 Premium 明确不支持该地区
//   - ""     → 不可达或无法解析地区
func CheckYoutube(httpClient *http.Client) (string, error) {
	// 创建请求
	req, err := http.NewRequest("GET", "https://www.youtube.com/premium", nil)
	if err != nil {
		return "", err
	}

	// 模拟真实浏览器，避免触发简化页面逻辑
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")

	// 发送请求
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// CN IP 常见情形：302 跳转到非 YouTube 域，body 为空或极短
	// 直接在状态码层面识别，不依赖 body 内容
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "google.cn") || strings.Contains(loc, "sorry") {
			return "CN", nil
		}
	}

	// INNERTUBE_CONTEXT_GL 在页面前 256KB 内，超出无需读取
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil && err != io.EOF {
		return "", err
	}

	// 统一转换一次，避免重复 string(body)
	bodyStr := string(body)

	// CN 多信号检测（顺序：快速字符串匹配优先于正则）
	for _, sig := range cnSignals {
		if strings.Contains(bodyStr, sig) {
			return "CN", nil
		}
	}

	// 提取地区码
	region := ""
	if m := reYoutubeGL.FindStringSubmatch(bodyStr); len(m) > 1 {
		region = m[1]
	}

	// Premium 不支持：保留地区码，附加 ⁻ 后缀供调用方识别
	if strings.Contains(bodyStr, "Premium is not available in your country") {
		return region + "⁻", nil // region 为空时返回 "⁻"，调用方统一处理
	}

	return region, nil
}
