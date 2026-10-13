package parse

import (
	"encoding/base64"
	"sort"
	"strings"
	"testing"

	"github.com/sinspired/subs-check-pro/v3/utils"
)

// 特征化测试：锁定 ParseSubscriptionDataStream 在优化前的真实输出。
// 开发顺序约定：本文件先于双解析优化编写并在旧代码上跑绿，
// 优化（跳过步骤②重复的标准转换）落地后必须原样通过——
// 任何 golden 变化都意味着解析行为被改变，必须人工确认是无损的。
//
// 比较口径：每个产出节点的 utils.NodeKey（类型+地址+端口+凭证+传输层指纹）
// 排序后与 golden 逐一比对，节点顺序/重复都不影响。
//
// golden 填充方式：先以 want=nil 跑旧代码，从失败输出里复制实际指纹。

// collectStreamNodes 跑流式解析并返回排序后的节点指纹与错误
func collectStreamNodes(t *testing.T, data []byte, subURL string) ([]string, error) {
	t.Helper()
	var keys []string
	_, err := ParseSubscriptionDataStream(data, subURL, func(n map[string]any) bool {
		keys = append(keys, utils.NodeKey(n))
		return true
	})
	sort.Strings(keys)
	return keys, err
}

func runCharacterCase(t *testing.T, name string, data []byte, subURL string, want []string) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		got, err := collectStreamNodes(t, data, subURL)
		if err != nil {
			t.Fatalf("解析报错（特征化基线应成功）: %v", err)
		}
		wantSorted := append([]string(nil), want...)
		sort.Strings(wantSorted)
		if len(got) != len(wantSorted) {
			t.Fatalf("节点数 = %d, want %d\ngot:\n  %s\nwant:\n  %s",
				len(got), len(wantSorted),
				strings.Join(got, "\n  "), strings.Join(wantSorted, "\n  "))
		}
		for i := range got {
			if got[i] != wantSorted[i] {
				t.Errorf("节点指纹不匹配（第 %d 个）\ngot:  %s\nwant: %s", i, got[i], wantSorted[i])
			}
		}
	})
}

const (
	lnVless = "vless://11111111-2222-3333-4444-555555555555@v.example.com:443?type=tcp&security=tls#V-Node"
	lnSS    = "ss://YWVzLTEyOC1nY206cGFzc3dvcmQ@ss.example.com:8388#SS-Node"
	lnTro   = "trojan://trojan-pass@tr.example.com:443?security=tls#TR-Node"
	lnHy    = "hy://hy-pass@hy.example.com:443?sni=hy.example.com#HY-Node"
)

// TestStreamPlaintextStandard 纯标准链接明文：走步骤①整文件转换
func TestStreamPlaintextStandard(t *testing.T) {
	data := []byte(strings.Join([]string{lnVless, lnSS, lnTro}, "\n"))
	runCharacterCase(t, "三条标准链接", data, "list.txt", []string{
		"ss|ss.example.com|8388|pw:password|tls:0|",
		"trojan|tr.example.com|443|pw:trojan-pass|tls:1|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamPlaintextWithHy hy:// 修复路径：步骤①不认 hy://，
// 需要步骤② FixupProxyLink 改写为 hysteria:// 后再转换
func TestStreamPlaintextWithHy(t *testing.T) {
	data := []byte(strings.Join([]string{lnVless, lnHy}, "\n"))
	runCharacterCase(t, "标准链接加hy修复", data, "list.txt", []string{
		"hysteria|hy.example.com|443|tls:0|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamPlaintextWithMieru mieru:// 非标格式：只由 ConvertsV2RayExtra 恢复
func TestStreamPlaintextWithMieru(t *testing.T) {
	mieruBody := base64.StdEncoding.EncodeToString([]byte("mieru.example?server=m.example.com&port=1234"))
	data := []byte(strings.Join([]string{lnVless, "mieru://" + mieruBody}, "\n"))
	runCharacterCase(t, "标准链接加mieru", data, "list.txt", []string{
		"mieru|mieru.example|1234|net:tcp|tls:0|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamBase64 标准链接 base64 体：步骤①解码后转换
func TestStreamBase64(t *testing.T) {
	plain := strings.Join([]string{lnVless, lnTro}, "\n")
	data := []byte(base64.StdEncoding.EncodeToString([]byte(plain)))
	runCharacterCase(t, "base64体", data, "list.txt", []string{
		"trojan|tr.example.com|443|pw:trojan-pass|tls:1|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamCommentsAndLinks 注释行混排
func TestStreamCommentsAndLinks(t *testing.T) {
	data := []byte(strings.Join([]string{
		"# subscriber: test",
		lnVless,
		"# total: 1",
		lnSS,
	}, "\n"))
	runCharacterCase(t, "注释混排", data, "list.txt", []string{
		"ss|ss.example.com|8388|pw:password|tls:0|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamBareHostPort 裸 host:port：走 ② 的智能扩展（未知 scheme 时展开三协议）
func TestStreamBareHostPort(t *testing.T) {
	data := []byte("1.2.3.4:443\n5.6.7.8:8080\n")
	runCharacterCase(t, "裸hostport", data, "list.txt", []string{
		"http|1.2.3.4|443|tls:0|",
		"http|1.2.3.4|443|tls:1|",
		"http|5.6.7.8|8080|tls:0|",
		"http|5.6.7.8|8080|tls:1|",
		"socks5|1.2.3.4|443|tls:0|",
		"socks5|5.6.7.8|8080|tls:0|",
	})
}

// TestStreamWGAndSSR wireguard/ssr：mihomo 不处理，由步骤②手动解析分支恢复
func TestStreamWGAndSSR(t *testing.T) {
	// ssr://body：body = base64("host:port:protocol:method:obfs:password_base64")
	ssrInner := "ssr.example.com:8388:origin:aes-256-cfb:plain:" +
		base64.StdEncoding.EncodeToString([]byte("pass"))
	ssrLink := "ssr://" + base64.StdEncoding.EncodeToString([]byte(ssrInner))
	wgLink := "wireguard://cHJpdmF0ZS1rZXk=@wg.example.com:51820?publickey=QEV4YW1wbGUA&address=10.0.0.2/32#WG-Node"

	data := []byte(strings.Join([]string{lnVless, ssrLink, wgLink}, "\n"))
	runCharacterCase(t, "标准加ssr加wg", data, "list.txt", []string{
		"ssr|ssr.example.com|8388|pw:pass|tls:0|",
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
		"wireguard|wg.example.com|51820|pk:chjpdmf0zs1rzxk=|tls:0|",
	})
}

// TestStreamDuplicateLinks 同一链接出现两次：跨解析器去重后只保留一个
func TestStreamDuplicateLinks(t *testing.T) {
	data := []byte(lnVless + "\n" + lnVless + "\n")
	runCharacterCase(t, "重复链接", data, "list.txt", []string{
		"vless|v.example.com|443|id:11111111-2222-3333-4444-555555555555|net:tcp|tls:1|",
	})
}

// TestStreamClashYAML clash yaml：早期 YAML 分支返回，不进入行级解析
func TestStreamClashYAML(t *testing.T) {
	data := []byte(`proxies:
  - name: Y-Node
    type: http
    server: y.example.com
    port: 8080
`)
	runCharacterCase(t, "clashyaml", data, "list.txt", []string{
		"http|y.example.com|8080|tls:0|",
	})
}
