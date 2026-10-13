package utils

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/goccy/go-json"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// 定义通用的 HTTP 客户端接口
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// API 响应的结构体
type versionResponse struct {
	Version string `json:"version"`
}

type providersResponse struct {
	Providers map[string]struct {
		VehicleType string `json:"vehicleType"`
	} `json:"providers"`
}

// makeRequest 处理通用的 HTTP 请求逻辑
func makeRequest(client httpClient, method, url string) ([]byte, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.Get().MihomoAPISecret)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("执行请求失败: %w", err)
	}
	// 非 200 提前返回的路径也必须 drain，否则连接无法复用（且旧实现在此无超时挂死）
	defer func() {
		DrainBody(resp)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNoContent {
			return nil, nil
		}
		return nil, fmt.Errorf("API 返回非 200 状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}

	return body, nil
}

func UpdateSubs() {
	if config.Get().MihomoAPIURL == "" {
		// slog.Warn("未配置 MihomoApiUrl，跳过更新")
		return
	}

	// 版本信息只用于日志展示：拿不到不应该中断整个更新流程
	if version, err := getVersion(LocalHTTPClient); err == nil {
		slog.Info("当前Mihomo版本", "version", version)
	} else {
		slog.Debug("获取 Mihomo 版本失败（仅影响日志展示）", "error", err)
	}

	names, err := getNeedUpdateNames(LocalHTTPClient)
	if err != nil {
		slog.Error("获取需要更新的订阅失败", "error", err)
		return
	}

	if err := updateSubs(LocalHTTPClient, names); err != nil {
		slog.Error("订阅更新存在失败项", "error", err)
		return
	}
	slog.Info("订阅更新完成")
}

func getVersion(client httpClient) (string, error) {
	url := config.Get().MihomoAPIURL + "/version"
	body, err := makeRequest(client, http.MethodGet, url)
	if err != nil {
		return "", err
	}

	var version versionResponse
	if err := json.Unmarshal(body, &version); err != nil {
		return "", fmt.Errorf("解析版本信息失败: %w", err)
	}
	return version.Version, nil
}

func getNeedUpdateNames(client httpClient) ([]string, error) {
	url := config.Get().MihomoAPIURL + "/providers/proxies"
	body, err := makeRequest(client, http.MethodGet, url)
	if err != nil {
		return nil, err
	}

	var response providersResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("解析提供者信息失败: %w", err)
	}

	var names []string
	for name, provider := range response.Providers {
		if provider.VehicleType == "HTTP" {
			names = append(names, name)
		}
	}
	return names, nil
}

func updateSubs(client httpClient, names []string) error {
	var failed int
	for _, name := range names {
		url := config.Get().MihomoAPIURL + "/providers/proxies/" + name
		if _, err := makeRequest(client, http.MethodPut, url); err != nil {
			slog.Error("更新订阅失败", "name", name, "error", err)
			failed++
			continue
		}
		slog.Info("成功更新订阅", "name", name)
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d 个订阅更新失败", failed, len(names))
	}
	return nil
}
