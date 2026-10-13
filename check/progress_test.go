package check

import "testing"

// TestGetCheckWeight 测速阶段移除后，进度权重只剩两档，
// 且必须等于历史「无测速」分支的取值（80/20、100/0），保证进度百分比不变。
func TestGetCheckWeight(t *testing.T) {
	on := getCheckWeight(true)
	if on.alive != 80 || on.media != 20 {
		t.Errorf("mediaON 权重 = %+v, want {alive:80 media:20}", on)
	}
	if on.alive+on.media != 100 {
		t.Errorf("mediaON 权重和 = %v, want 100", on.alive+on.media)
	}

	off := getCheckWeight(false)
	if off.alive != 100 || off.media != 0 {
		t.Errorf("!mediaON 权重 = %+v, want {alive:100 media:0}", off)
	}
	if off.alive+off.media != 100 {
		t.Errorf("!mediaON 权重和 = %v, want 100", off.alive+off.media)
	}
}
