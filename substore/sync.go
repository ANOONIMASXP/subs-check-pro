package substore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

// Sub-Store 资源结构体

// sub 单条订阅。
// 只保留官方必要字段，不向 sub-store.json 写入官方默认之外的键
// （display-name / remark / tag 均为旧版本注入，官方 2.x 迁移逻辑本就会清除 display-name）。
type sub struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Icon        string `json:"icon,omitempty"`
	IsIconColor bool   `json:"isIconColor,omitempty"`
	Process     []any  `json:"process"`
	Source      string `json:"source"`
	URL         string `json:"url"`
	Content     string `json:"content"`
	UA          string `json:"ua"`
}

// 常量
const (
	SubName = "sub"

	// scpIDPrefix 标识本程序历史注入的操作（仅用于同步时识别并清理）
	scpIDPrefix = "SCP."

	// scpTag 本程序创建订阅时打的标记（仅用于识别旧记录以便一次性重建）
	scpTag = "Subs-Check-Pro"
)

// 全局锁防止 save 包并发推送和前端修改并发写冲突
var subStoreMu sync.Mutex

// 全局运行时变量
var BaseURL string // 基础api地址

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

// regexFilterOp 按配置构造官方 Regex Filter 节点操作（注入 sub.process 首位）。
// id 带 scpIDPrefix 前缀，复用 isScpOperator 的生命周期：清空 filter-regex 后
// 下轮同步会自动剔除该操作。正则本身在 Sub-Store 的 JS 侧执行，Go 不做解析。
func regexFilterOp() (any, bool) {
	regex := config.Get().FilterRegex
	if len(regex) == 0 {
		return nil, false
	}
	// 用 []any 承载，保证与“解析既有 JSON”得到的类型一致（reflect.DeepEqual 可比）
	list := make([]any, len(regex))
	for i, r := range regex {
		list[i] = r
	}
	return map[string]any{
		"id":   scpIDPrefix + "regex-filter",
		"type": "Regex Filter",
		"args": map[string]any{
			"regex": list,
			"keep":  config.Get().FilterRegexKeep,
		},
	}, true
}

// desiredProcess 计算 sub.process 的目标状态：
//  1. 剔除本程序历史注入的操作（id 带 SCP. 前缀），用户自定义操作原样保留；
//  2. 配置了 filter-regex 时，把官方 Regex Filter 插到数组首位；
//  3. 与现状做结构比较，changed=false 表示无需下发。
//
// ok=false 表示既有操作里有无法解析的条目，为避免破坏用户配置应放弃本次 process 下发。
func desiredProcess(existing []json.RawMessage) (desired []any, changed bool, ok bool) {
	all := make([]any, 0, len(existing))
	kept := make([]any, 0, len(existing))
	for _, raw := range existing {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false, false
		}
		all = append(all, v)
		if !isScpOperator(raw) {
			kept = append(kept, v)
		}
	}
	desired = kept
	if op, has := regexFilterOp(); has {
		desired = append([]any{op}, kept...)
	}
	return desired, !reflect.DeepEqual(all, desired), true
}

// 资源构建

func newDefaultSub(data []byte) sub {
	proc, _, _ := desiredProcess(nil) // 创建路径即带上按配置注入的正则筛选
	return sub{
		Name:        SubName,
		DisplayName: SubName,
		Source:      "local",
		Content:     string(data),
		Process:     proc,
	}
}

// HTTP 辅助
//
// 统一走 utils.LocalHTTPClient（30s 超时、不走代理、HTTP/1.1）并接受 ctx。
// 旧实现用 http.Get / http.DefaultClient（零超时），Sub-Store 卡住时会把
// 持有 subStoreMu 的保存流水线无限期挂死；错误路径统一 drain 响应体以复用连接。

// fetchSubscription 获取指定资源的原始对象及其 process 列表（原始 JSON 用于差量合并/重建）
func fetchSubscription(ctx context.Context, endpoint, name string) (json.RawMessage, []json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, utils.JoinURL(BaseURL, "api", endpoint, name), nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("获取 %s/%s 失败，状态码: %d", endpoint, name, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil, fmt.Errorf("解析 %s/%s 响应失败: %w", endpoint, name, err)
	}
	if envelope.Status != "success" {
		return nil, nil, fmt.Errorf("获取 %s/%s 失败", endpoint, name)
	}
	var proc struct {
		Process []json.RawMessage `json:"process"`
	}
	_ = json.Unmarshal(envelope.Data, &proc)
	return envelope.Data, proc.Process, nil
}

// fetchProcess 获取指定资源的现有 process 列表（保留原始 JSON 用于差量合并）
func fetchProcess(ctx context.Context, endpoint, name string) ([]json.RawMessage, error) {
	_, process, err := fetchSubscription(ctx, endpoint, name)
	return process, err
}

// fetchSubs 获取全量订阅的原始对象列表
func fetchSubs(ctx context.Context) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/subs", BaseURL), nil)
	if err != nil {
		return nil, err
	}
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取订阅列表失败，状态码: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Status string            `json:"status"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析订阅列表响应失败: %w", err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("获取订阅列表失败: status=%s", envelope.Status)
	}
	return envelope.Data, nil
}

// createResource 创建资源
func createResource(ctx context.Context, endpoint string, data any, name string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/%ss", BaseURL, endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("创建 %s 失败，状态码: %d", name, resp.StatusCode)
	}
	return nil
}

// syncResource 同步资源（PATCH）
func syncResource(ctx context.Context, endpoint string, data any, name string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/api/%s/%s", BaseURL, endpoint, name), bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("同步 %s 失败，状态码: %d", name, resp.StatusCode)
	}
	return nil
}

// deleteResource 删除资源（DELETE）
func deleteResource(ctx context.Context, endpoint, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/api/%s/%s", BaseURL, endpoint, name), nil)
	if err != nil {
		return err
	}
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("删除 %s/%s 失败，状态码: %d", endpoint, name, resp.StatusCode)
	}
	return nil
}

// replaceSubs 用官方 PUT /api/subs 整体替换订阅列表（仅用于一次性重建旧字段）
func replaceSubs(ctx context.Context, subs []json.RawMessage) error {
	body, err := json.Marshal(subs)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/subs", BaseURL), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := utils.LocalHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		utils.DrainBody(resp)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("替换订阅失败，状态码: %d", resp.StatusCode)
	}
	return nil
}

// sub 同步

// syncSub 创建或更新 sub 订阅。
//
// 本程序不管理用户在 Sub-Store 里配置的 process（节点操作）：仅会
// ① 剔除历史版本由本程序注入的 SCP 操作、② 按 filter-regex 注入官方
// Regex Filter 到数组首位，且只在状态有变化时下发。
// 若检测到旧版本注入的非官方字段（display-name / 程序 tag / subUserinfo），
// 走官方 PUT /api/subs 做一次性重建——PATCH 是合并语义，删不掉这些键。
func syncSub(ctx context.Context, s sub) error {
	endpoint := "sub"

	existing, process, err := fetchSubscription(ctx, endpoint, s.Name)
	if err != nil {
		// 首次创建
		slog.Debug(fmt.Sprintf("检查 %s 失败: %v，正在创建...", s.Name, err))
		return createResource(ctx, endpoint, s, s.Name)
	}

	if hasLegacyFields(existing) {
		if err := rebuildSub(ctx, existing, process, s); err != nil {
			slog.Warn("Sub-Store 订阅旧字段清理失败，降级为普通同步", "name", s.Name, "error", err)
		} else {
			slog.Info("Sub-Store 订阅已重建为官方默认字段", "name", s.Name)
			return nil
		}
	}

	patch := struct {
		Icon    string `json:"icon,omitempty"`
		Content string `json:"content,omitempty"`
		// 指针 + omitempty：nil = 本次不触碰 process；非 nil 时即便空数组也会下发
		// `"process": []`（普通 slice 的 omitempty 会把空数组也省掉，导致清不掉操作）
		Process *[]any `json:"process,omitempty"`
	}{
		Icon:    s.Icon,
		Content: s.Content,
	}

	// 按配置注入正则筛选 + 清理历史 SCP 操作；仅在状态有变化时下发 process
	if desired, changed, ok := desiredProcess(process); ok && changed {
		patch.Process = &desired
	}

	return syncResource(ctx, endpoint, patch, SubName)
}

// 旧字段清理与一次性重建

// hasLegacyFields 判断既有 sub 是否带旧版本注入的非官方字段。
// 触发条件只认程序自己的标记（display-name、SCP tag、残留 subUserinfo），
// 避免误删用户在 UI 里手写的 remark / tag。
func hasLegacyFields(existing json.RawMessage) bool {
	var probe struct {
		DisplayNameAlt *string  `json:"display-name"`
		Tag            []string `json:"tag"`
		SubUserinfo    *string  `json:"subUserinfo"`
	}
	if err := json.Unmarshal(existing, &probe); err != nil {
		return false
	}
	if probe.DisplayNameAlt != nil {
		return true
	}
	if probe.SubUserinfo != nil && *probe.SubUserinfo != "" {
		return true
	}
	for _, t := range probe.Tag {
		if t == scpTag {
			return true
		}
	}
	return false
}

// buildRebuiltSub 用官方最小字段重建 sub，保留用户配置的 icon 与自定义 process
func buildRebuiltSub(existing json.RawMessage, process []json.RawMessage, s sub) sub {
	var meta struct {
		Icon        string `json:"icon"`
		IsIconColor bool   `json:"isIconColor"`
	}
	_ = json.Unmarshal(existing, &meta)
	s.Icon = meta.Icon
	s.IsIconColor = meta.IsIconColor

	// 重建路径同样走目标状态计算：剔除历史 SCP 操作、注入正则筛选
	if desired, _, ok := desiredProcess(process); ok {
		s.Process = desired
	} else if len(process) > 0 {
		s.Process = rawsToAny(process) // 解析失败兜底：原样保留用户操作
	}
	return s
}

func rawsToAny(raws []json.RawMessage) []any {
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// rebuildSub 一次性把旧版本注入的字段清掉：
// 读全量订阅 → 只替换本程序托管的 sub → 官方 PUT /api/subs 写回。
// 其余订阅以 json.RawMessage 原样透传，字节与键序不变。
func rebuildSub(ctx context.Context, existing json.RawMessage, process []json.RawMessage, s sub) error {
	allRaw, err := fetchSubs(ctx)
	if err != nil {
		return err
	}

	rebuilt, err := json.Marshal(buildRebuiltSub(existing, process, s))
	if err != nil {
		return fmt.Errorf("序列化订阅失败: %w", err)
	}

	subs := make([]json.RawMessage, 0, len(allRaw))
	replaced := false
	for _, raw := range allRaw {
		var meta struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &meta) == nil && meta.Name == s.Name {
			subs = append(subs, rebuilt)
			replaced = true
			continue
		}
		subs = append(subs, raw)
	}
	if !replaced {
		return fmt.Errorf("未找到订阅 %q，无法重建", s.Name)
	}
	return replaceSubs(ctx, subs)
}

// 入口

// SyncSubStore 同步 Sub-Store 订阅
// 执行检测完毕后如果有新节点，把检测结果推送到默认 sub 订阅
func SyncSubStore(yamlData []byte) {
	subStoreMu.Lock()
	defer subStoreMu.Unlock()

	// 调试时等待 node 启动
	if os.Getenv("SUB_CHECK_SKIP") != "" && config.Get().SubStorePort != "" {
		time.Sleep(time.Second * 1)
	}

	// 整体 90s 上限：本机 API 正常毫秒级返回；即便逐请求 30s 超时，
	// 全部调用点加起来也不能把持有 subStoreMu 的保存流水线拖过一分半
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 只读快照推导基址，禁止回写 config（热重载的端口/路径比对依赖稳定原值，
	// 历史上这里的 formatPort 归一化回写会造成与文件无关的误判重启）
	BaseURL = deriveBaseURL(config.Get())

	defaultSub := newDefaultSub(yamlData)
	if err := syncSub(ctx, defaultSub); err != nil {
		slog.Error("同步订阅失败", "name", defaultSub.Name, "error", err)
		return
	}
	slog.Info("Sub-Store 订阅已同步", "name", defaultSub.Name)

	// 清理历史版本遗留的 mihomo / singbox file 资源（仅删除带 SCP 标记的）
	cleanupLegacyFileResources(ctx)

	slog.Info("Sub-Store 同步完成")
}

// deriveBaseURL 由配置快照推导 Sub-Store API 基址（纯函数，不修改配置）。
// BaseURL 仍是包级变量：唯一写者为 SyncSubStore（subStoreMu 保护），
// 读者（save 包下载、集成测试）都在同步调用完成后顺序读取，不存在并发窗口。
func deriveBaseURL(cfg *config.Config) string {
	base := "http://127.0.0.1" + formatPort(cfg.SubStorePort)
	if p := cfg.SubStorePath; p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		base += p
	}
	return base
}

// cleanupLegacyFileResources 删除历史版本由本程序创建的 file 资源。
//
// 仅当资源 process 中包含带 scpIDPrefix 的操作时才删除，避免误删用户自建同名资源。
func cleanupLegacyFileResources(ctx context.Context) {
	for _, name := range []string{"mihomo", "singbox", "singbox-1.14", "singbox-1.11"} {
		process, err := fetchProcess(ctx, "wholeFile", name)
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
		if err := deleteResource(ctx, "file", name); err != nil {
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
