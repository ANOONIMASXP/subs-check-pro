// Package save 保存检测结果
package save

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/save/method"
	"github.com/sinspired/subs-check-pro/v3/substore"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

// ProxyCategory 定义代理分类
type ProxyCategory struct {
	Name    string
	Proxies []map[string]any
	Filter  func(result check.Result) bool
}

// ConfigSaver 处理配置保存的结构体
type ConfigSaver struct {
	methodName string
	results    []check.Result
	categories []ProxyCategory
	saveMethod func([]byte, string) error
}

// localClient 用于本地 SubStore 请求
var localClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,

		// 强制 HTTP/1.1，避免本地服务对 HTTP/2 支持不完整
		ForceAttemptHTTP2: false,

		// 大连接池，避免 TIME_WAIT 导致失败
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,

		// 更快的连接建立
		DisableKeepAlives:  false,
		DisableCompression: false,
	},
}

// NewConfigSaver 创建新的配置保存器，支持显式指定保存方法
func NewConfigSaver(results []check.Result, saveMethodName string) *ConfigSaver {
	return &ConfigSaver{
		methodName: saveMethodName,
		results:    results,
		saveMethod: getSaverFunc(),
		categories: []ProxyCategory{
			{Name: "all.yaml", Proxies: nil, Filter: func(r check.Result) bool { return true }},
			{Name: "mihomo.yaml", Proxies: nil, Filter: func(r check.Result) bool { return true }},
			{Name: "base64.txt", Proxies: nil, Filter: func(r check.Result) bool { return true }},
			{Name: "singbox.json", Proxies: nil, Filter: func(r check.Result) bool { return true }},
			{Name: "history.yaml", Proxies: nil, Filter: func(r check.Result) bool { return true }},
		},
	}
}

// SaveConfig 保存配置的入口函数（仅本地保存）
func SaveConfig(results []check.Result) {
	saver := NewConfigSaver(results, "local")
	if err := saver.Save(); err != nil {
		slog.Error("保存本地配置失败", "err", err)
	}
}

// Save 执行保存操作
func (cs *ConfigSaver) Save() error {
	cs.categorizeProxies()

	for _, category := range cs.categories {
		if len(category.Proxies) == 0 {
			slog.Warn("节点为空，跳过保存", "文件", category.Name, "保存方法", cs.methodName)
			continue
		}

		// 1. 生成内容 (解耦生成与保存)
		content, err := cs.generateContent(category)
		if err != nil {
			slog.Error("生成内容失败", "文件", category.Name, "err", err)
			continue
		}
		if len(content) == 0 { // 例如 base64 在没有运行 substore 时返回空
			continue
		}

		// 2. 写入存储
		if err := cs.saveMethod(content, category.Name); err != nil {
			slog.Error("保存失败", "文件", category.Name, "保存方法", cs.methodName, "err", err)
		}
	}

	return nil
}

// categorizeProxies 将代理按类别分类
func (cs *ConfigSaver) categorizeProxies() {
	for _, result := range cs.results {
		for i := range cs.categories {
			if cs.categories[i].Filter(result) {
				cs.categories[i].Proxies = append(cs.categories[i].Proxies, result.Proxy)
			}
		}
	}
}

// generateContent 根据文件类型生成对应的字节数据
func (cs *ConfigSaver) generateContent(category ProxyCategory) ([]byte, error) {
	switch category.Name {
	case "history.yaml":
		return cs.generateHistory(category.Proxies)
	case "all.yaml":
		return cs.generateAllYaml(category.Proxies)
	case "mihomo.yaml":
		return cs.generateMihomo()
	case "base64.txt":
		return cs.generateBase64()
	case "singbox.json":
		return cs.generateSingbox()
	default:
		return nil, fmt.Errorf("未知的文件类型: %s", category.Name)
	}
}

func (cs *ConfigSaver) generateHistory(newProxies []map[string]any) ([]byte, error) {
	localSubDir, err := getLocalSubDir()
	if err != nil {
		return nil, fmt.Errorf("无法获取本地存储路径: %w", err)
	}

	var existing []map[string]any
	filePath := filepath.Join(localSubDir, "history.yaml")

	if data, err := ReadFileIfExists(filePath); err == nil && len(data) > 0 {
		var parsed map[string][]map[string]any
		if err := yaml.Unmarshal(data, &parsed); err == nil {
			existing = parsed["proxies"]
		}
	}

	merged := mergeUniqueProxies(existing, newProxies)
	return yaml.Marshal(map[string]any{"proxies": merged})
}

func (cs *ConfigSaver) generateAllYaml(proxies []map[string]any) ([]byte, error) {
	yamlData, err := yaml.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		return nil, fmt.Errorf("序列化 %w", err)
	}

	// 仅在执行本地保存，且 SubStore 运行时触发 SubStore 更新
	if cs.methodName == "local" && config.GlobalConfig.SubStorePort != "" && substore.IsSubStoreRunning.Load() {
		utils.SyncSubStore(yamlData)
	}
	return yamlData, nil
}

// generateMihomo 通过 Sub-Store 的 mihomo target 转换输出（仅 proxies，无分组/规则）
func (cs *ConfigSaver) generateMihomo() ([]byte, error) {
	if config.GlobalConfig.SubStorePort == "" || !substore.IsSubStoreRunning.Load() {
		return nil, nil // 不满足条件直接跳过，不报错
	}

	// http://127.0.0.1:8299/download/sub?target=mihomo
	targetURL := utils.BaseURL + "/download/" + utils.SubName + "?target=mihomo"
	resp, err := localClient.Get(targetURL)
	if err != nil {
		return nil, fmt.Errorf("请求 mihomo 配置失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 mihomo 配置失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 mihomo 配置失败，状态码: %d", resp.StatusCode)
	}
	return body, nil
}

func (cs *ConfigSaver) generateBase64() ([]byte, error) {
	if config.GlobalConfig.SubStorePort == "" || !substore.IsSubStoreRunning.Load() {
		return nil, nil // 不满足条件直接跳过，不报错
	}

	// http://127.0.0.1:8299/download/sub?target=V2Ray
	targetURL := utils.BaseURL + "/download/" + utils.SubName + "?target=V2Ray"
	resp, err := localClient.Get(targetURL)
	if err != nil {
		return nil, fmt.Errorf("请求 base64 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 base64 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 base64 失败，状态码: %d", resp.StatusCode)
	}
	return body, nil
}

// generateSingbox 通过 Sub-Store 将 sub 转换为 sing-box 配置
func (cs *ConfigSaver) generateSingbox() ([]byte, error) {
	if config.GlobalConfig.SubStorePort == "" || !substore.IsSubStoreRunning.Load() {
		return nil, nil // 不满足条件直接跳过，不报错
	}

	// http://127.0.0.1:8299/download/sub?target=sing-box
	targetURL := utils.BaseURL + "/download/" + utils.SubName + "?target=sing-box"
	resp, err := localClient.Get(targetURL)
	if err != nil {
		return nil, fmt.Errorf("请求 sing-box 配置失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 sing-box 配置失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 sing-box 配置失败，状态码: %d", resp.StatusCode)
	}
	return body, nil
}

// 为辅助与配置
// getSaverFunc 返回本地保存方法
func getSaverFunc() func([]byte, string) error {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return func(b []byte, s string) error { return fmt.Errorf("本地保存器创建失败: %w", err) }
	}
	saver.OutputPath = filepath.Join(saver.OutputPath, "sub")
	return saver.Save
}

// getLocalSubDir 获取本地 sub 文件夹的绝对路径（供 history 等读取使用）
func getLocalSubDir() (string, error) {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return "", err
	}
	outPath := filepath.Join(saver.OutputPath, "sub")
	if !filepath.IsAbs(outPath) {
		outPath = filepath.Join(saver.BasePath, outPath)
	}
	return outPath, nil
}

// mergeUniqueProxies 使用可变参数重构，支持合并多个代理列表并去重
func mergeUniqueProxies(proxyLists ...[]map[string]any) []map[string]any {
	seen := make(map[string]bool)
	var result []map[string]any

	for _, list := range proxyLists {
		for _, p := range list {
			delete(p, "sub_was_succeed")
			delete(p, "sub_from_history")
			key := utils.GenerateProxyKey(p)
			if !seen[key] {
				seen[key] = true
				result = append(result, p)
			}
		}
	}
	return result
}

func ReadFileIfExists(path string) ([]byte, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return os.ReadFile(path)
}
