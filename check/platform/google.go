package platform

import (
	"context"
	"log/slog"
	"net/http"
)

func CheckGoogle(httpClient *http.Client, ctx context.Context) (bool, error) {
	if success, err := checkGoogleEndpoint(httpClient, ctx, "https://gstatic.com/generate_204", 204); err == nil && success {
		return checkGoogleEndpoint(httpClient, ctx, "https://www.google.com/generate_204", 204)
	}
	return false, nil
}

func CheckGstatic(httpClient *http.Client, ctx context.Context) (bool, error) {
	return checkGoogleEndpoint(httpClient, ctx, "https://gstatic.com/generate_204", 204)
}

// checkGoogleEndpoint 使用 HEAD 方法检查 URL 的状态码。
func checkGoogleEndpoint(httpClient *http.Client, ctx context.Context, url string, statusCode int) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	// 释放连接池和内存
	req.Close = true

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Debug(err.Error())
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == statusCode, nil
}
