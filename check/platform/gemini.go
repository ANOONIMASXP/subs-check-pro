package platform

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/biter777/countries"
	"github.com/metacubex/mihomo/common/convert"
)

// GeminiAccess Gemini 地区访问状态
type GeminiAccess uint8

const (
	AccessNormal  GeminiAccess = iota // 正常可访问
	AccessBlocked                     // 封锁名单内
	AccessSuspect                     // 封锁名单内但功能标记异常，可能已解封
)

// GeminiStatus Gemini 检测结果
type GeminiStatus struct {
	Region string // ISO 3166-1 alpha-2，空 = 不可访问
	IsEU   bool   // 受欧盟法规约束
	Access GeminiAccess
}

var (
	// viewer session 策略层: [1,null,null,<id>,<num>,"<ALPHA3>","<lang>",...]
	reRegion = regexp.MustCompile(`\[1,null,null,\d+,\d+,\\?"([A-Z]{3})\\?"`)
	// bw5YWe: 货币政策
	// reCurrency = regexp.MustCompile(`\[45615224,null,null,null,\\?"([^"\\]+)\\?",null,\\?"bw5YWe\\?"\]`)
	// 非 EU/封锁地区的完整功能标记；若封锁地区此二者为 true，封锁状态可能已变化
	reEdHLke = regexp.MustCompile(`\[45700351,null,(true|false),null,null,null,\\?"edHLke\\?"\]`)
	reXgpeRd = regexp.MustCompile(`\[45631641,null,(true|false),null,null,null,\\?"XgpeRd\\?"\]`)
)

// ErrGeminiBotDetected IP 被 Google 风控，不代表地区不可访问
var ErrGeminiBotDetected = errors.New("gemini: bot detection triggered (/sorry/)")

// euMembers 受欧盟法规（GDPR / EU AI Act）约束的地区（ISO 3166-1 alpha-2）
// 包含全部 27 个 EU 成员国 + EEA（挪威、冰岛、列支敦士登）
var euMembers = map[string]bool{
	// EU 27
	"AT": true, "BE": true, "BG": true, "CY": true, "CZ": true,
	"DE": true, "DK": true, "EE": true, "ES": true, "FI": true,
	"FR": true, "GR": true, "HR": true, "HU": true, "IE": true,
	"IT": true, "LT": true, "LU": true, "LV": true, "MT": true,
	"NL": true, "PL": true, "PT": true, "RO": true, "SE": true,
	"SI": true, "SK": true,
	// EEA
	"NO": true, "IS": true, "LI": true,
}

// blockedRegions Gemini 不运营的地区（ISO 3166-1 alpha-3）
// Google 于 2026 年 3 月 16-17 日左右 开始逐步向香港所有用户开放 Gemini Web 应用，随后会开放移动 App。
var blockedRegions = map[string]bool{
	"CHN": true, "RUS": true, "BLR": true,
	"CUB": true, "IRN": true, "PRK": true,
	"SYR": true, "HKG": false, "MAC": false,
}

// https://github.com/clash-verge-rev/clash-verge-rev/blob/main/src-tauri/src/cmd/media_unlock_checker/gemini.rs

// CheckGemini 检测 Gemini 访问状态，含特征码判断
//
// 返回值说明：
//   - (status, nil)              → 成功，status.Region 有值
//   - (empty, ErrGeminiBotDetected) → IP 被风控，非地区封锁，调用方应降级到 CheckGeminiByCountry
//   - (empty, otherErr)          → 网络不可达或其他错误
//   - (empty, nil)               → 请求成功但未能解析地区（页面结构变化）
func CheckGemini(client *http.Client) (GeminiStatus, error) {
	localClient := *client

	var botDetected bool // 闭包捕获，CheckRedirect 触发时标记
	localClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// 精确匹配 path，避免 query param 中含 /sorry/ 误判
		if strings.Contains(req.URL.Path, "/sorry/") {
			botDetected = true
			return http.ErrUseLastResponse
		}
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		return nil
	}

	req, err := http.NewRequest("GET", "https://gemini.google.com/", nil)
	if err != nil {
		return GeminiStatus{}, err
	}

	// 模拟浏览器请求头，避免被 Google 识别为爬虫触发重定向
	req.Header.Set("User-Agent", convert.RandUserAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := localClient.Do(req)
	if err != nil {
		// 真正的网络错误（超时、连接拒绝等）
		return GeminiStatus{}, err
	}
	defer resp.Body.Close()

	// bot 检测：被重定向到 /sorry/ 后停止，最终落在非 200 页面
	if botDetected {
		return GeminiStatus{}, ErrGeminiBotDetected
	}

	if resp.StatusCode != http.StatusOK {
		// 非 200 且非 sorry，视为地区不可访问（如 403/451）
		return GeminiStatus{}, fmt.Errorf("gemini: unexpected status %d", resp.StatusCode)
	}

	limitReader := io.LimitReader(resp.Body, 1024*1024)

	body, err := io.ReadAll(limitReader)
	if err != nil && err != io.EOF {
		return GeminiStatus{}, err
	}

	m := reRegion.FindSubmatch(body)
	if m == nil {
		// 页面正常返回但未找到地区特征码
		return GeminiStatus{}, nil
	}

	a3 := string(m[1])
	region := toAlpha2(a3)

	// 直接由账户/IP 归属地区判断，不依赖货币标记
	isEU := euMembers[region]

	blocked, inList := blockedRegions[a3]
	if !inList || !blocked {
		return GeminiStatus{Region: region, IsEU: isEU, Access: AccessNormal}, nil
	}

	// 封锁名单内，交叉验证功能标记是否矛盾
	access := AccessBlocked
	if matchTrue(reEdHLke, body) && matchTrue(reXgpeRd, body) {
		access = AccessSuspect
	}
	return GeminiStatus{Region: region, IsEU: isEU, Access: access}, nil
}

// CheckGeminiByCountry 降级判断：无特征码，仅根据 alpha-2 国家码判断封锁状态
// 只能区分 AccessNormal / AccessBlocked，无法识别 AccessSuspect
// 用于 CheckGemini 触发 bot 检测时的兜底
func CheckGeminiByCountry(alpha2 string) GeminiStatus {
	if alpha2 == "" {
		return GeminiStatus{}
	}
	isEU := euMembers[alpha2]
	// 需要 alpha-3 查 blockedRegions，转换一次
	c := countries.ByName(alpha2)
	a3 := c.Alpha3()
	if blocked, inList := blockedRegions[a3]; inList && blocked {
		return GeminiStatus{Region: alpha2, IsEU: isEU, Access: AccessBlocked}
	}
	return GeminiStatus{Region: alpha2, IsEU: isEU, Access: AccessNormal}
}

func toAlpha2(a3 string) string {
	c := countries.ByName(a3)
	if c == countries.Unknown {
		return a3 // 未知时原样返回
	}
	return c.Alpha2()
}

func matchTrue(re *regexp.Regexp, body []byte) bool {
	m := re.FindSubmatch(body)
	return m != nil && string(m[1]) == "true"
}
