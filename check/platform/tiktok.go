package platform

import (
	"context"
	"io"
	"net/http"
	"regexp"
)

// reTikTokRegion 匹配 "region":"XX"（每节点请求都要用，包级预编译）
var reTikTokRegion = regexp.MustCompile(`"region"\s*:\s*"([A-Z]{2})"`)

func CheckTikTok(ctx context.Context, httpClient *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.tiktok.com/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil
	}

	limitReader := io.LimitReader(resp.Body, 128*1024)

	body, err := io.ReadAll(limitReader)
	if err != nil && err != io.EOF {
		return "", err
	}

	// 使用正则匹配 "region":"XX"
	matches := reTikTokRegion.FindSubmatch(body)
	if len(matches) >= 2 {
		return string(matches[1]), nil
	}
	return "", nil
}
