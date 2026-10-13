package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/metacubex/mihomo/common/convert"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

func CheckIPRisk(ctx context.Context, httpClient *http.Client, ip string) (string, error) {
	// TODO: 增加 "https://www.abuseipdb.com/check/${LOCAL_IP}"
	req, err := http.NewRequestWithContext(ctx, "GET", utils.JoinURL("https://scamalytics.com/ip", ip), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", convert.RandUserAgent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		// 读取响应内容
		limitReader := io.LimitReader(resp.Body, 64*1024)
		body, err := io.ReadAll(limitReader)
		if err != nil && err != io.EOF {
			return "", err
		}
		bodyStr := string(body)
		_, after, ok := strings.Cut(bodyStr, "IP Fraud Risk API")
		if !ok {
			return "", fmt.Errorf("未找到IP Fraud Risk API")
		}
		// 从 "IP Fraud Risk API" 后的内容开始
		contentAfterAPI := after
		// 按行分割
		lines := strings.Split(contentAfterAPI, "\n")

		if len(lines) < 7 {
			return "", fmt.Errorf("IP Fraud Risk API响应格式不正确")
		}
		var score, rist string
		{
			score = strings.TrimSpace(lines[4])
			// 响应格式无 ":" 时不能直接取 tmp[1]——单条畸形数据会 panic 整个进程
			if _, v, ok := strings.Cut(score, ":"); ok {
				score = strings.ReplaceAll(v, "\"", "")
				score = strings.ReplaceAll(score, ",", "")
			} else {
				return "", fmt.Errorf("IP Fraud Risk API 响应第5行缺少冒号: %q", score)
			}

			rist = strings.TrimSpace(lines[5])
			if _, v, ok := strings.Cut(rist, ":"); ok {
				rist = strings.ReplaceAll(v, "\"", "")
				rist = strings.ReplaceAll(rist, ",", "")
			} else {
				return "", fmt.Errorf("IP Fraud Risk API 响应第6行缺少冒号: %q", rist)
			}
		}

		if score != "" && rist != "" {
			// return score + "%" + " " + rist, nil   // 如果要同时输出 rist
			return score + "%", nil
		}

	}
	return "", nil
}
