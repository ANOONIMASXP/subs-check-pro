package platform

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/metacubex/mihomo/common/convert"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

func CheckIPRisk(httpClient *http.Client, ip string) (string, error) {
	// TODO: 增加 "https://www.abuseipdb.com/check/${LOCAL_IP}"
	req, err := http.NewRequest("GET", utils.JoinURL("https://scamalytics.com/ip", ip), nil)
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
			tmp := strings.Split(score, ":")
			score = strings.ReplaceAll(tmp[1], "\"", "")
			score = strings.ReplaceAll(score, ",", "")

			rist = strings.TrimSpace(lines[5])
			tmp = strings.Split(rist, ":")
			rist = strings.ReplaceAll(tmp[1], "\"", "")
			rist = strings.ReplaceAll(rist, ",", "")
		}

		if score != "" && rist != "" {
			// return score + "%" + " " + rist, nil   // 如果要同时输出 rist
			return score + "%", nil
		}

	}
	return "", nil
}
