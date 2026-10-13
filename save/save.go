// Package save 保存检测结果
package save

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"

	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/save/method"
	"github.com/sinspired/subs-check-pro/v3/substore"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

// ConfigSaver 处理配置保存的结构体
type ConfigSaver struct {
	results    []check.Result
	saveMethod func([]byte, string) error
}

// localClient 已统一为 utils.LocalHTTPClient（30s 超时、不走代理、HTTP/1.1），
// 与 Sub-Store 同步、mihomo API 共用同一套本机请求策略

// NewConfigSaver 创建新的配置保存器（保存方式固定为 local）
func NewConfigSaver(results []check.Result) *ConfigSaver {
	return &ConfigSaver{
		results:    results,
		saveMethod: getSaverFunc(),
	}
}

// SaveConfig 保存配置的入口函数（仅本地保存）
func SaveConfig(results []check.Result) {
	// 保存前确保本次检测的所有节点名称完全唯一（节点裂变/同参数多来源可能重名）
	resultProxies := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if r.Proxy != nil {
			resultProxies = append(resultProxies, r.Proxy)
		}
	}
	utils.RenameDuplicateNodes(resultProxies)

	saver := NewConfigSaver(results)
	if err := saver.Save(); err != nil {
		slog.Error("保存本地配置失败", "err", err)
	}
}

// Save 执行保存操作。
// 顺序有依赖：① all.yaml 内含 SyncSubStore 副作用，必须先推送新内容到
// Sub-Store，② 的 mihomo/base64/singbox 产出才会基于最新节点。
func (cs *ConfigSaver) Save() error {
	proxies := cs.collectProxies()
	if len(proxies) == 0 {
		slog.Warn("节点为空，跳过保存")
		return nil
	}

	// ① all.yaml（推送 Sub-Store + 写盘）
	cs.saveAllYaml(proxies)

	// ② Sub-Store 转换产出（mihomo.yaml / base64.txt / singbox.json）
	cs.saveSubStoreOutputs()

	// ③ history.yaml（与历史文件 merge 去重）
	cs.saveHistory(proxies)

	return nil
}

// collectProxies 收集全部检测结果的代理（五个产物共用同一份，不再按类别拷贝）
func (cs *ConfigSaver) collectProxies() []map[string]any {
	proxies := make([]map[string]any, 0, len(cs.results))
	for _, r := range cs.results {
		proxies = append(proxies, r.Proxy)
	}
	return proxies
}

// saveAllYaml 生成并写入 all.yaml，同时把同一份 YAML 推送到内置 Sub-Store
func (cs *ConfigSaver) saveAllYaml(proxies []map[string]any) {
	yamlData, err := yaml.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		slog.Error("生成内容失败", "文件", "all.yaml", "err", err)
		return
	}

	// SyncSubStore 副作用：Sub-Store 运行时把检测结果推送到默认 sub 订阅，
	// 后续 mihomo/base64/singbox 转换产出依赖这一步先完成
	if subStoreRunning() {
		substore.SyncSubStore(yamlData)
	}

	cs.write(yamlData, "all.yaml")
}

// saveSubStoreOutputs 通过 Sub-Store 的转换接口产出 mihomo.yaml / base64.txt / singbox.json。
// Sub-Store 未运行时三项全部静默跳过（与旧行为一致：返回空内容即跳过）。
func (cs *ConfigSaver) saveSubStoreOutputs() {
	if !subStoreRunning() {
		return
	}

	for _, out := range []struct{ target, file string }{
		{target: "mihomo", file: "mihomo.yaml"},
		{target: "V2Ray", file: "base64.txt"},
		{target: "sing-box", file: "singbox.json"},
	} {
		content, err := fetchSubStoreTarget(out.target)
		if err != nil {
			slog.Error("生成内容失败", "文件", out.file, "err", err)
			continue
		}
		if len(content) == 0 {
			continue
		}
		cs.write(content, out.file)
	}
}

// saveHistory 合并历史可用节点并写入 history.yaml
func (cs *ConfigSaver) saveHistory(proxies []map[string]any) {
	content, err := cs.generateHistory(proxies)
	if err != nil {
		slog.Error("生成内容失败", "文件", "history.yaml", "err", err)
		return
	}
	cs.write(content, "history.yaml")
}

// write 写入存储；错误只记日志（保持 Save 固定返回 nil 的旧行为）
func (cs *ConfigSaver) write(content []byte, name string) {
	if err := cs.saveMethod(content, name); err != nil {
		slog.Error("保存失败", "文件", name, "err", err)
	}
}

// subStoreRunning 判断内置 Sub-Store 是否可用来做格式转换
func subStoreRunning() bool {
	return config.Get().SubStorePort != "" && substore.IsSubStoreRunning.Load()
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

// fetchSubStoreTarget 从 Sub-Store 拉取指定 target 的转换产出。
// 统一：共享 30s 超时客户端 + 状态码先行判定 + 错误路径 drain；
// 调用方前置判断 Sub-Store 是否运行，未运行时不会调用本函数。
func fetchSubStoreTarget(target string) ([]byte, error) {
	targetURL := substore.BaseURL + "/download/" + substore.SubName + "?target=" + target
	resp, err := utils.LocalHTTPClient.Get(targetURL)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 产出失败: %w", target, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		utils.DrainBody(resp)
		return nil, fmt.Errorf("获取 %s 产出失败，状态码: %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 产出失败: %w", target, err)
	}
	return body, nil
}

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
