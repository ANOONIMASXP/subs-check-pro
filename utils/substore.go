// Package utils 工具类包
package utils

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/sinspired/subs-check-pro/v3/config"
)

// Sub-Store 资源结构体

// sub 单条订阅
type sub struct {
	Name                  string   `json:"name"`
	DisplayName           string   `json:"displayName"`
	DisplayNameAlt        string   `json:"display-name"`
	Remark                string   `json:"remark"`
	MergeSources          string   `json:"mergeSources"`
	IgnoreFailedRemoteSub bool     `json:"ignoreFailedRemoteSub"`
	PassThroughUA         bool     `json:"passThroughUA"`
	Icon                  string   `json:"icon,omitempty"`
	IsIconColor           bool     `json:"isIconColor,omitempty"`
	Process               []any    `json:"process"`
	Source                string   `json:"source"`
	URL                   string   `json:"url"`
	Content               string   `json:"content"`
	UA                    string   `json:"ua"`
	Tag                   []string `json:"tag,omitempty"`
	SubUserInfo           string   `json:"subUserinfo,omitempty"`
}

// 常量
const (
	SubName     = "sub"
	SubInfoPath = "/sub-info"

	// scpIDPrefix 标识本程序历史注入的操作（仅用于同步时识别并清理）
	scpIDPrefix = "SCP."
)

// 全局锁防止 save 包并发推送和前端修改并发写冲突
var subStoreMu sync.Mutex

// 全局运行时变量
var (
	BaseURL        string // 基础api地址
	SubUserInfoURL string // SubUserInfoURL 订阅流量信息 URL
)

// 操作识别工具

// isScpOperator 判断操作是否由本程序管理（ID 带 SCP 前缀）
func isScpOperator(raw json.RawMessage) bool {
	var op struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return false
	}
	return strings.HasPrefix(op.ID, scpIDPrefix)
}

// 资源构建

func newDefaultSub(data []byte) sub {
	return sub{
		Name:           SubName,
		DisplayName:    SubName,
		DisplayNameAlt: SubName,
		Remark:         "默认订阅 (无分流规则)",
		Tag:            []string{"Subs-Check-Pro", "已检测"},
		SubUserInfo:    SubUserInfoURL,
		Source:         "local",
		Content:        string(data),
		Process:        []any{},
	}
}

// HTTP 辅助

// fetchProcess 获取指定资源的现有 process 列表（保留原始 JSON 用于差量合并）
func fetchProcess(endpoint, name string) ([]json.RawMessage, error) {
	resp, err := http.Get(JoinURL(BaseURL, "api", endpoint, name))

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Process []json.RawMessage `json:"process"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析 %s/%s 响应失败: %w", endpoint, name, err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("获取 %s/%s 失败", endpoint, name)
	}
	return envelope.Data.Process, nil
}

// createResource 创建资源
func createResource(endpoint string, data any, name string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	resp, err := http.Post(
		fmt.Sprintf("%s/api/%ss", BaseURL, endpoint),
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("创建 %s 失败，状态码: %d", name, resp.StatusCode)
	}
	return nil
}

// syncResource 同步资源（PATCH）
func syncResource(endpoint string, data any, name string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("%s/api/%s/%s", BaseURL, endpoint, name),
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("同步 %s 失败，状态码: %d", name, resp.StatusCode)
	}
	return nil
}

// deleteResource 删除资源（DELETE）
func deleteResource(endpoint, name string) error {
	req, err := http.NewRequest(
		http.MethodDelete,
		fmt.Sprintf("%s/api/%s/%s", BaseURL, endpoint, name),
		nil,
	)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("删除 %s/%s 失败，状态码: %d", endpoint, name, resp.StatusCode)
	}
	return nil
}

// sub 同步

// syncSub 创建或更新 sub 订阅。
//
// 本程序不再管理 sub 的 process（节点操作）；已存在时仅在需要清理历史注入时
// 下发 process，其余情况完全不触碰 process，避免覆盖用户在 Sub-Store 的配置。
func syncSub(s sub) error {
	endpoint := "sub"

	existing, err := fetchProcess(endpoint, s.Name)
	if err != nil {
		// 首次创建
		slog.Debug(fmt.Sprintf("检查 %s 失败: %v，正在创建...", s.Name, err))
		return createResource(endpoint, s, s.Name)
	}

	patch := struct {
		Icon        string `json:"icon,omitempty"`
		Content     string `json:"content,omitempty"`
		SubUserInfo string `json:"subUserinfo,omitempty"`
		Process     []any  `json:"process,omitempty"`
	}{
		Icon:        s.Icon,
		Content:     s.Content,
		SubUserInfo: s.SubUserInfo,
	}

	// 清理历史版本注入的 SCP 操作，保留用户自定义操作
	if cleaned, removed := stripScpOperators(existing); removed {
		patch.Process = cleaned
	}

	return syncResource(endpoint, patch, SubName)
}

// stripScpOperators 过滤掉历史版本由本程序注入的 SCP 操作，保留用户自定义操作。
// 返回剩余操作列表以及是否发生过过滤；若存在无法解析的操作则放弃清理。
func stripScpOperators(existing []json.RawMessage) (remaining []any, removed bool) {
	result := make([]any, 0, len(existing))
	hit := false
	for _, raw := range existing {
		if isScpOperator(raw) {
			hit = true
			continue
		}
		var op any
		if err := json.Unmarshal(raw, &op); err != nil {
			// 无法解析：放弃清理，避免破坏用户配置
			return nil, false
		}
		result = append(result, op)
	}
	if !hit {
		return nil, false
	}
	return result, true
}

// 入口

// SyncSubStore 同步 Sub-Store 订阅
// 执行检测完毕后如果有新节点，把检测结果推送到默认 sub 订阅
func SyncSubStore(yamlData []byte) {
	subStoreMu.Lock()
	defer subStoreMu.Unlock()

	// 调试时等待 node 启动
	if os.Getenv("SUB_CHECK_SKIP") != "" && config.GlobalConfig.SubStorePort != "" {
		time.Sleep(time.Second * 1)
	}

	// 构建订阅流量信息 URL
	listenPort := strings.TrimSpace(config.GlobalConfig.ListenPort)
	if listenPort == "" {
		listenPort = "8199"
	}
	listenPort = strings.TrimPrefix(listenPort, ":")
	SubUserInfoURL = fmt.Sprintf("http://127.0.0.1:%s%s#noCache", listenPort, SubInfoPath)

	config.GlobalConfig.SubStorePort = formatPort(config.GlobalConfig.SubStorePort)
	BaseURL = fmt.Sprintf("http://127.0.0.1%s", config.GlobalConfig.SubStorePort)
	if p := config.GlobalConfig.SubStorePath; p != "" {
		if !strings.HasPrefix(p, "/") {
			config.GlobalConfig.SubStorePath = "/" + p
		}
		BaseURL += config.GlobalConfig.SubStorePath
	}

	defaultSub := newDefaultSub(yamlData)
	if err := syncSub(defaultSub); err != nil {
		slog.Error("同步订阅失败", "name", defaultSub.Name, "error", err)
		return
	}
	slog.Info("Sub-Store 订阅已同步", "name", defaultSub.Name)

	// 清理历史版本遗留的 mihomo / singbox file 资源（仅删除带 SCP 标记的）
	cleanupLegacyFileResources()

	slog.Info("Sub-Store 同步完成")
}

// cleanupLegacyFileResources 删除历史版本由本程序创建的 file 资源。
//
// 仅当资源 process 中包含带 scpIDPrefix 的操作时才删除，避免误删用户自建同名资源。
func cleanupLegacyFileResources() {
	for _, name := range []string{"mihomo", "singbox", "singbox-1.14", "singbox-1.11"} {
		process, err := fetchProcess("wholeFile", name)
		if err != nil {
			continue // 资源不存在或不可访问
		}
		managed := false
		for _, raw := range process {
			if isScpOperator(raw) {
				managed = true
				break
			}
		}
		if !managed {
			continue
		}
		if err := deleteResource("file", name); err != nil {
			slog.Debug("清理遗留 Sub-Store 资源失败", "name", name, "error", err)
			continue
		}
		slog.Info("已清理遗留 Sub-Store 资源", "name", name)
	}
}

// 工具函数

// formatPort 统一端口格式为 ":PORT"，兼容用户输入 IP:PORT 的情况
func formatPort(port string) string {
	if strings.Contains(port, ":") {
		parts := strings.Split(port, ":")
		return ":" + parts[len(parts)-1]
	}
	return ":" + port
}
