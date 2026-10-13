package save

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
)

// withTempOutputDir 把输出目录指到临时目录，测试结束恢复原配置。
func withTempOutputDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	prev := config.Get()
	cp := *prev
	cp.OutputDir = dir
	cp.SubStorePort = "" // 无 Sub-Store：mihomo/base64/singbox 三项应静默跳过
	config.Set(&cp)
	t.Cleanup(func() { config.Set(prev) })

	return dir
}

func makeResults(names ...string) []check.Result {
	results := make([]check.Result, 0, len(names))
	for _, n := range names {
		results = append(results, check.Result{Proxy: map[string]any{
			// GenerateProxyKey 按 server/port 指纹去重（不含 name），
			// 测试节点必须用与名字绑定的不同 server
			"name": n, "type": "http", "server": n + ".example", "port": 8080,
		}})
	}
	return results
}

func readProxies(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var parsed map[string][]map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return parsed["proxies"]
}

// TestSaveConfigLocalOnly Sub-Store 未运行时只产出 all.yaml + history.yaml，
// mihomo/base64/singbox 三项静默跳过。
func TestSaveConfigLocalOnly(t *testing.T) {
	dir := withTempOutputDir(t)
	subDir := filepath.Join(dir, "sub")

	SaveConfig(makeResults("node-a", "node-b"))

	all := readProxies(t, filepath.Join(subDir, "all.yaml"))
	if len(all) != 2 {
		t.Errorf("all.yaml 节点数 = %d, want 2", len(all))
	}
	hist := readProxies(t, filepath.Join(subDir, "history.yaml"))
	if len(hist) != 2 {
		t.Errorf("history.yaml 节点数 = %d, want 2", len(hist))
	}

	for _, f := range []string{"mihomo.yaml", "base64.txt", "singbox.json"} {
		if _, err := os.Stat(filepath.Join(subDir, f)); !os.IsNotExist(err) {
			t.Errorf("Sub-Store 未运行时不应产出 %s (err=%v)", f, err)
		}
	}
}

// TestSaveConfigHistoryMerge 二次保存合并去重：同名同参数节点不膨胀。
func TestSaveConfigHistoryMerge(t *testing.T) {
	dir := withTempOutputDir(t)
	subDir := filepath.Join(dir, "sub")

	SaveConfig(makeResults("node-a", "node-b"))
	SaveConfig(makeResults("node-a", "node-c"))

	hist := readProxies(t, filepath.Join(subDir, "history.yaml"))
	if len(hist) != 3 {
		t.Errorf("history.yaml 合并后节点数 = %d, want 3（a/b/c 去重）", len(hist))
	}
}

// TestSaveConfigEmptyResults 空结果不落盘。
func TestSaveConfigEmptyResults(t *testing.T) {
	dir := withTempOutputDir(t)

	SaveConfig(nil)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取输出目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("空结果不应落盘，输出目录有 %d 项", len(entries))
	}
}
