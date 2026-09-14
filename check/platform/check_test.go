package platform

import (
	"net/http"
	"testing"
)

func TestCheckGemini(t *testing.T) {
	client := &http.Client{}

	status, err := CheckGemini(client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Region: %s | IsEU: %v | Access: %v", status.Region, status.IsEU, status.Access)

	if status.Region == "" {
		t.Error("expected non-empty Region")
		return
	}

	// 模拟 check.go 标签生成逻辑，nodeCountry 为节点 IP 归属（留空模拟一致/不一致两种场景）
	for _, nodeCountry := range []string{status.Region, "XX"} {
		tag := geminiTag(status, nodeCountry)
		t.Logf("nodeCountry=%-4s → tag=%q", nodeCountry, tag)
	}
}

// geminiTag 与 check.go 中标签生成逻辑保持一致
func geminiTag(g GeminiStatus, nodeCountry string) string {
	if g.Region == "" {
		return ""
	}
	switch g.Access {
	case AccessBlocked:
		return ""
	case AccessSuspect:
		return "GMˀ"
	default: // AccessNormal
		tag := "GM"
		if g.IsEU {
			tag = "GM⁻"
		}
		if g.Region != nodeCountry {
			tag = tag + "-" + g.Region
		}
		return tag
	}
}

func TestCheckCopilot_Success(t *testing.T) {
	// 使用默认 http.Client
	client := &http.Client{}

	Copilot, CopilotAPI, err := CheckCopilot(client)

	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Copilot API: %v", CopilotAPI)

	if !Copilot {
		t.Errorf("expected true, got false")
	}
}
