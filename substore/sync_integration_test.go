package substore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// testSubStorePort 集成测试专用端口，避开常用端口
const testSubStorePort = ":18973"

// TestSyncSubStoreOfficialDefaultShape 端到端验证：
// 启动真实 Sub-Store 引擎 → 同步本地订阅 → 断言 sub-store.json 只比官方默认多出一条 sub，
// 且 sub 记录不含 display-name / remark / tag / subUserinfo 等非官方键。
// startTestSubStore 保存/恢复全局配置，并在独立输出目录里启动真实 Sub-Store 引擎
func startTestSubStore(t *testing.T) string {
	t.Helper()

	outDir := t.TempDir()

	prev := config.Get()
	t.Cleanup(func() { config.Set(prev) })

	config.Mutate(func(c *config.Config) {
		c.OutputDir = outDir
		c.SubStorePort = testSubStorePort
		c.SubStorePath = "/scp-it-test"
		c.ListenPort = ""
	})

	ctx, cancel := context.WithCancel(context.Background())
	go RunSubStoreService(ctx)
	t.Cleanup(func() {
		cancel()
		WaitSubStoreStopped(10 * time.Second)
	})

	waitSubStoreRunning(t, 30*time.Second)
	return outDir
}

func TestSyncSubStoreOfficialDefaultShape(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 Sub-Store 集成测试")
	}

	outDir := startTestSubStore(t)

	kvPath := filepath.Join(outDir, "sub-store", "sub-store.json")

	const testYAML = "proxies:\n  - name: t\n    type: http\n    server: 1.1.1.1\n    port: 8080\n"

	// 1) 首次同步：官方默认 + 最小字段 sub
	SyncSubStore([]byte(testYAML))
	first := assertOfficialKV(t, kvPath)
	t.Logf("顶层键顺序: %v", topKeys(t, first))

	// 2) 注入旧版本字段 → 再次同步应触发一次性重建并清干净
	injectLegacyFields(t)
	SyncSubStore([]byte(testYAML))
	assertOfficialKV(t, kvPath)

	// 3) 幂等：再次同步走普通 PATCH，不应回潮
	SyncSubStore([]byte(testYAML + "# updated\n"))
	assertOfficialKV(t, kvPath)

	// 4) 官方下载接口仍可用（最小字段载荷不应破坏转换输出）
	for _, target := range []string{"mihomo", "sing-box"} {
		resp, err := http.Get(fmt.Sprintf("%s/download/%s?target=%s", BaseURL, SubName, target))
		if err != nil {
			t.Fatalf("下载 %s 失败: %v", target, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("下载 %s 状态码 %d: %s", target, resp.StatusCode, raw)
			continue
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			t.Errorf("下载 %s 返回空内容", target)
		}
	}
}

func waitSubStoreRunning(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !IsSubStoreRunning.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("Sub-Store 服务未能在 %v 内启动", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// injectLegacyFields 通过官方 PATCH 的合并语义塞入旧版本字段
func injectLegacyFields(t *testing.T) {
	t.Helper()
	body := []byte(`{
		"display-name": "sub",
		"remark": "默认订阅 (无分流规则)",
		"tag": ["Subs-Check-Pro", "已检测"],
		"subUserinfo": "http://127.0.0.1:1/flow"
	}`)
	req, err := http.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("%s/api/sub/%s", BaseURL, SubName),
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("注入旧字段失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("注入旧字段失败，状态码: %d, body: %s", resp.StatusCode, raw)
	}
}

// assertOfficialKV 校验 sub-store.json 形态并返回文件内容
func assertOfficialKV(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	text := string(raw)

	if !json.Valid(raw) {
		t.Fatalf("sub-store.json 不是合法 JSON:\n%s", text)
	}

	// 官方格式：JSON.stringify(cache, null, 2)，即 2 空格缩进、无尾换行
	if !strings.Contains(text, "\n  \"") {
		t.Errorf("缺少官方 2 空格缩进格式")
	}
	if strings.HasSuffix(text, "\n") {
		t.Errorf("官方产物不应带尾换行")
	}

	// 官方插入顺序（迁移逻辑 → REST 模块初始化），绝不能退化成 Go Marshal 的字母序
	wantOrder := []string{
		"subs", "collections", "artifacts", "rules", "files", "tokens",
		"schemaVersion", "settings", "archives", "modules",
	}
	gotOrder := topKeys(t, text)
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("顶层键顺序不符（应为官方插入顺序）:\n got: %v\nwant: %v", gotOrder, wantOrder)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析顶层失败: %v", err)
	}
	for _, key := range []string{"schemaVersion", "subs", "settings"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("缺少官方键 %q", key)
		}
	}
	if sv := string(doc["schemaVersion"]); sv != `"2.0"` {
		t.Errorf("schemaVersion 应为 2.0，实际 %s", sv)
	}

	var subs []map[string]json.RawMessage
	if err := json.Unmarshal(doc["subs"], &subs); err != nil {
		t.Fatalf("解析 subs 失败: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subs 应只有 1 条，实际 %d", len(subs))
	}

	got := make([]string, 0, len(subs[0]))
	for k := range subs[0] {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{"content", "displayName", "name", "process", "source", "ua", "url"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sub 记录字段不符:\n got: %v\nwant: %v\n记录: %s", got, want, subs[0])
	}

	var metas []struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Source      string `json:"source"`
	}
	if err := json.Unmarshal(doc["subs"], &metas); err != nil {
		t.Fatalf("解析 meta 失败: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("meta 解析数量异常: %d", len(metas))
	}
	meta := metas[0]
	if meta.Name != SubName || meta.DisplayName != SubName || meta.Source != "local" {
		t.Errorf("sub 元信息不符: %+v", meta)
	}

	return text
}

// topKeys 取顶层键顺序（按出现顺序）
func topKeys(t *testing.T, text string) []string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// map 无序，改用文本扫描
	var keys []string
	dec := json.NewDecoder(strings.NewReader(text))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("顶层不是对象: %v", tok)
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("扫描键失败: %v", err)
		}
		key, ok := k.(string)
		if !ok {
			t.Fatalf("意外的 token: %v", k)
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("跳过值失败: %v", err)
		}
	}
	return keys
}

// TestRegexFilterInjectionE2E 筛选端到端：
// 注入官方 Regex Filter → 下载产出只含命中节点 → 清空配置自动移除并恢复全量
func TestRegexFilterInjectionE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 Sub-Store 集成测试")
	}
	startTestSubStore(t)

	prev := config.Get()
	t.Cleanup(func() { config.Set(prev) })
	config.Mutate(func(c *config.Config) {
		c.FilterRegex = []string{"keep-me"}
		c.FilterRegexKeep = true
	})

	content := "proxies:\n" +
		"  - name: keep-me\n    type: http\n    server: 127.0.0.1\n    port: 8080\n" +
		"  - name: drop-me\n    type: http\n    server: 127.0.0.1\n    port: 8081\n"

	SyncSubStore([]byte(content))

	// 1) process 首位是注入的官方 Regex Filter
	proc, err := fetchProcess(context.Background(), "sub", SubName)
	if err != nil {
		t.Fatalf("读取 process 失败: %v", err)
	}
	if len(proc) != 1 {
		t.Fatalf("操作数 = %d, want 1: %s", len(proc), proc)
	}
	var op struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Args struct {
			Regex []string `json:"regex"`
			Keep  bool     `json:"keep"`
		} `json:"args"`
	}
	if err := json.Unmarshal(proc[0], &op); err != nil {
		t.Fatalf("解析操作失败: %v", err)
	}
	if op.ID != "SCP.regex-filter" || op.Type != "Regex Filter" {
		t.Errorf("注入的操作不符: %+v", op)
	}
	if len(op.Args.Regex) != 1 || op.Args.Regex[0] != "keep-me" || !op.Args.Keep {
		t.Errorf("args 不符: %+v", op.Args)
	}

	// 2) 端到端：官方 RegexFilter 在下载链路生效，产出只含命中节点
	body := downloadText(t, "mihomo")
	if !strings.Contains(body, "keep-me") {
		t.Errorf("产出应保留命中节点:\n%s", body)
	}
	if strings.Contains(body, "drop-me") {
		t.Errorf("RegexFilter 未生效：未命中节点仍在产出中:\n%s", body)
	}

	// 3) 清空配置 → 下轮同步自动移除操作 → 产出恢复全量
	config.Mutate(func(c *config.Config) { c.FilterRegex = nil })
	SyncSubStore([]byte(content))
	proc, err = fetchProcess(context.Background(), "sub", SubName)
	if err != nil {
		t.Fatalf("读取 process 失败: %v", err)
	}
	if len(proc) != 0 {
		t.Errorf("清空配置后操作应被移除: %s", proc)
	}
	body = downloadText(t, "mihomo")
	if !strings.Contains(body, "drop-me") {
		t.Errorf("清空配置后产出应恢复全量:\n%s", body)
	}
}

// downloadText 拉取官方下载产出
func downloadText(t *testing.T, target string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/download/%s?target=%s", BaseURL, SubName, target))
	if err != nil {
		t.Fatalf("下载 %s 失败: %v", target, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载 %s 状态码 %d: %s", target, resp.StatusCode, raw)
	}
	return string(raw)
}
