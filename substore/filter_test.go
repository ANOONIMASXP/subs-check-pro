package substore

import (
	"encoding/json"
	"testing"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// setFilterConfig 覆盖筛选配置，测试结束后恢复
func setFilterConfig(t *testing.T, regex []string, keep bool) {
	t.Helper()
	prev := config.Get()
	t.Cleanup(func() { config.Set(prev) })

	config.Mutate(func(c *config.Config) {
		c.FilterRegex = regex
		c.FilterRegexKeep = keep
	})
}

func mustRaw(t *testing.T, v string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(v)) {
		t.Fatalf("测试夹具不是合法 JSON: %s", v)
	}
	return json.RawMessage(v)
}

// TestRegexFilterOpEmptyConfig 未配置时不注入
func TestRegexFilterOpEmptyConfig(t *testing.T) {
	setFilterConfig(t, nil, true)
	if _, has := regexFilterOp(); has {
		t.Error("空配置不应构造操作")
	}

	setFilterConfig(t, []string{}, true)
	if _, has := regexFilterOp(); has {
		t.Error("空列表不应构造操作")
	}
}

// TestRegexFilterOpShape op 形状必须与官方 Regex Filter 完全对齐
func TestRegexFilterOpShape(t *testing.T) {
	setFilterConfig(t, []string{"(?i)HK|JP", "刀"}, true)

	op, has := regexFilterOp()
	if !has {
		t.Fatal("应构造操作")
	}
	m, ok := op.(map[string]any)
	if !ok {
		t.Fatalf("op 类型错误: %T", op)
	}
	if m["id"] != "SCP.regex-filter" {
		t.Errorf("id = %v, want SCP.regex-filter", m["id"])
	}
	if m["type"] != "Regex Filter" {
		t.Errorf("type = %v, want Regex Filter", m["type"])
	}
	args, ok := m["args"].(map[string]any)
	if !ok {
		t.Fatalf("args 类型错误: %T", m["args"])
	}
	regex, ok := args["regex"].([]any)
	if !ok || len(regex) != 2 || regex[0] != "(?i)HK|JP" || regex[1] != "刀" {
		t.Errorf("regex 不正确: %#v", args["regex"])
	}
	if keep, ok := args["keep"].(bool); !ok || !keep {
		t.Errorf("keep = %v, want true", args["keep"])
	}

	// keep=false 必须透传
	setFilterConfig(t, []string{"x"}, false)
	op, _ = regexFilterOp()
	args = op.(map[string]any)["args"].(map[string]any)
	if keep, _ := args["keep"].(bool); keep {
		t.Error("keep=false 未透传")
	}
}

// TestDesiredProcessInjectFirst 正则筛选插在首位，用户操作原样保留在后
func TestDesiredProcessInjectFirst(t *testing.T) {
	setFilterConfig(t, []string{"keep-me"}, true)

	userOp := mustRaw(t, `{"id":"user-1","type":"Flag Operator","args":{"mode":"add"}}`)
	desired, changed, ok := desiredProcess([]json.RawMessage{userOp})
	if !ok {
		t.Fatal("ok 应为 true")
	}
	if !changed {
		t.Error("注入后应有变化")
	}
	if len(desired) != 2 {
		t.Fatalf("操作数 = %d, want 2", len(desired))
	}
	first, _ := desired[0].(map[string]any)
	if first["id"] != "SCP.regex-filter" || first["type"] != "Regex Filter" {
		t.Errorf("首位不是注入的操作: %#v", first)
	}
	second, _ := desired[1].(map[string]any)
	if second["id"] != "user-1" || second["type"] != "Flag Operator" {
		t.Errorf("用户操作被改动: %#v", second)
	}
	if args, _ := second["args"].(map[string]any); args["mode"] != "add" {
		t.Errorf("用户操作参数被改动: %#v", args)
	}
}

// TestDesiredProcessIdempotent 已注入状态再计算不应产生变化（不重复插、不反复 PATCH）
func TestDesiredProcessIdempotent(t *testing.T) {
	setFilterConfig(t, []string{"keep-me"}, true)

	first, _, _ := desiredProcess(nil)
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var existing []json.RawMessage
	if err := json.Unmarshal(raw, &existing); err != nil {
		t.Fatal(err)
	}

	desired, changed, ok := desiredProcess(existing)
	if !ok {
		t.Fatal("ok 应为 true")
	}
	if changed {
		t.Errorf("二次计算不应有变化: %s", raw)
	}
	if len(desired) != 1 {
		t.Errorf("不应重复插入: %d", len(desired))
	}
}

// TestDesiredProcessClearsOwnOps 配置清空后自动移除本程序注入的操作
func TestDesiredProcessClearsOwnOps(t *testing.T) {
	setFilterConfig(t, []string{"keep-me"}, true)
	injected, _, _ := desiredProcess(nil)
	raw, err := json.Marshal(injected)
	if err != nil {
		t.Fatal(err)
	}
	var existing []json.RawMessage
	if err := json.Unmarshal(raw, &existing); err != nil {
		t.Fatal(err)
	}

	setFilterConfig(t, nil, true)
	desired, changed, ok := desiredProcess(existing)
	if !ok {
		t.Fatal("ok 应为 true")
	}
	if !changed {
		t.Error("清空配置后应移除操作")
	}
	if len(desired) != 0 {
		t.Errorf("残留操作: %#v", desired)
	}
}

// TestDesiredProcessClearsLegacySCPOps 历史版本注入的 SCP 操作会被清理，用户操作保留
func TestDesiredProcessClearsLegacySCPOps(t *testing.T) {
	setFilterConfig(t, nil, true)

	legacy := mustRaw(t, `{"id":"SCP.sub-info","type":"Sub Info"}`)
	userOp := mustRaw(t, `{"id":"user-9","type":"Sort Operator","args":{"order":"asc"}}`)

	desired, changed, ok := desiredProcess([]json.RawMessage{legacy, userOp})
	if !ok {
		t.Fatal("ok 应为 true")
	}
	if !changed {
		t.Error("清理历史操作应产生变化")
	}
	if len(desired) != 1 {
		t.Fatalf("残留操作数 = %d, want 1", len(desired))
	}
	kept, _ := desired[0].(map[string]any)
	if kept["id"] != "user-9" {
		t.Errorf("用户操作未保留: %#v", kept)
	}
}

// TestDesiredProcessUnparsable 放弃无法解析的既有操作，避免破坏用户配置
func TestDesiredProcessUnparsable(t *testing.T) {
	setFilterConfig(t, []string{"x"}, true)

	desired, changed, ok := desiredProcess([]json.RawMessage{json.RawMessage("{oops")})
	if ok {
		t.Error("无法解析时 ok 应为 false")
	}
	if changed || desired != nil {
		t.Error("无法解析时不应产生目标状态")
	}
}
