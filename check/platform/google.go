package platform

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// DefaultAliveURL 默认测活地址（必须返回 204）
const DefaultAliveURL = "https://gstatic.com/generate_204"

// CheckGstaticURL 用 HEAD 探测指定地址（地址必须返回 204）。
// 返回 (是否命中 204, 请求耗时ms, 错误)；传输失败时耗时为 0。
// 耗时含建连与响应首字节，作为测活延迟供日志与统计使用。
func CheckGstaticURL(httpClient *http.Client, ctx context.Context, url string) (bool, int, error) {
	if url == "" {
		url = DefaultAliveURL
	}
	return checkGoogleEndpoint(httpClient, ctx, url, 204)
}

// checkGoogleEndpoint 使用 HEAD 方法检查 URL 的状态码，并返回请求耗时(ms)。
func checkGoogleEndpoint(httpClient *http.Client, ctx context.Context, url string, statusCode int) (bool, int, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false, 0, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	// 释放连接池和内存
	req.Close = true

	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Debug(err.Error())
		return false, 0, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == statusCode, int(time.Since(start) / time.Millisecond), nil
}
