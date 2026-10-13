package substore

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDefaultSubPayloadOfficialFields 断言创建订阅的载荷只含官方必要字段，
// 不再写入 display-name / remark / tag / subUserinfo 等非官方默认键。
func TestDefaultSubPayloadOfficialFields(t *testing.T) {
	b, err := json.Marshal(newDefaultSub([]byte("proxies: []")))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	got := string(b)

	for _, banned := range []string{
		`"display-name"`, `"remark"`, `"tag"`, `"subUserinfo"`,
		`"mergeSources"`, `"ignoreFailedRemoteSub"`, `"passThroughUA"`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("载荷不应包含 %s: %s", banned, got)
		}
	}
	for _, required := range []string{
		`"name":"sub"`, `"displayName":"sub"`, `"source":"local"`,
		`"content":`, `"process":`, `"url":`, `"ua":`,
	} {
		if !strings.Contains(got, required) {
			t.Errorf("载荷缺少 %s: %s", required, got)
		}
	}
}

// TestHasLegacyFields 遗留字段识别：只认程序自己的标记，不误伤用户手写内容。
func TestHasLegacyFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"clean", `{"name":"sub","displayName":"sub","source":"local","content":"x","process":[]}`, false},
		{"display-name", `{"name":"sub","display-name":"sub"}`, true},
		{"scp tag", `{"name":"sub","tag":["Subs-Check-Pro","已检测"]}`, true},
		{"user tag", `{"name":"sub","tag":["自定义"]}`, false},
		{"empty subUserinfo", `{"name":"sub","subUserinfo":""}`, false},
		{"legacy subUserinfo", `{"name":"sub","subUserinfo":"http://127.0.0.1:8080/flow"}`, true},
		{"invalid json", `{oops`, false},
	}
	for _, c := range cases {
		if got := hasLegacyFields(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%s: hasLegacyFields = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBuildRebuiltSub 重建产物：剔除遗留键，保留 icon 与用户自定义 process。
func TestBuildRebuiltSub(t *testing.T) {
	existing := json.RawMessage(`{
		"name": "sub",
		"display-name": "sub",
		"remark": "默认订阅 (无分流规则)",
		"tag": ["Subs-Check-Pro", "已检测"],
		"subUserinfo": "http://127.0.0.1:8080/flow",
		"icon": "shield.png",
		"isIconColor": true,
		"url": "http://legacy.example",
		"process": [
			{"id": "SCP.legacy", "type": "Keyword Filter"},
			{"id": "user-op", "type": "Flag Operator", "args": {"mode": "add"}}
		]
	}`)
	process := []json.RawMessage{
		json.RawMessage(`{"id": "SCP.legacy", "type": "Keyword Filter"}`),
		json.RawMessage(`{"id": "user-op", "type": "Flag Operator", "args": {"mode": "add"}}`),
	}

	rebuilt := buildRebuiltSub(existing, process, newDefaultSub([]byte("proxies: []")))

	b, err := json.Marshal(rebuilt)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	got := string(b)
	for _, banned := range []string{
		`"display-name"`, `"remark"`, `"tag"`, `"subUserinfo"`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("重建结果不应包含 %s: %s", banned, got)
		}
	}

	if rebuilt.Icon != "shield.png" || !rebuilt.IsIconColor {
		t.Errorf("icon 未保留: icon=%q isIconColor=%v", rebuilt.Icon, rebuilt.IsIconColor)
	}
	if len(rebuilt.Process) != 1 {
		t.Fatalf("process 数量应为 1（剔除 SCP、保留用户）: %v", rebuilt.Process)
	}
	op, ok := rebuilt.Process[0].(map[string]any)
	if !ok || op["id"] != "user-op" {
		t.Errorf("保留的 process 不正确: %v", rebuilt.Process[0])
	}
	if rebuilt.Content != "proxies: []" {
		t.Errorf("content 应为本次检测结果: %q", rebuilt.Content)
	}
}

// TestBuildRebuiltSubNoScpProcess 没有 SCP 操作时，用户 process 原样保留。
func TestBuildRebuiltSubNoScpProcess(t *testing.T) {
	existing := json.RawMessage(`{"name":"sub","display-name":"sub"}`)
	process := []json.RawMessage{
		json.RawMessage(`{"id":"user-op","type":"Flag Operator","args":{"mode":"remove"}}`),
	}

	rebuilt := buildRebuiltSub(existing, process, newDefaultSub([]byte("proxies: []")))
	if len(rebuilt.Process) != 1 {
		t.Fatalf("用户 process 应原样保留: %v", rebuilt.Process)
	}
	if rebuilt.Icon != "" || rebuilt.IsIconColor {
		t.Errorf("无 icon 时不应凭空产生: %q %v", rebuilt.Icon, rebuilt.IsIconColor)
	}
}
