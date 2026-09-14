package platform

import (
	"io"
	"net/http"
	"regexp"

	"github.com/biter777/countries"
)

// 匹配 policies.google.com 响应中的国家码（alpha-3）
// 对应片段: 2,1,200,"CHN",null
var rePoliciesCountry = regexp.MustCompile(`\d,\d,200,"([A-Z]{3})"`)

// GetGoogleCountry 通过 policies.google.com 获取 Google 识别的国家（ISO 3166-1 alpha-2）
// 不依赖 Cookie，不触发 bot 检测
// 用途：CheckGemini 触发 bot 检测时的降级来源；YouTube 检测失败时的兜底
func GetGoogleCountry(client *http.Client) (string, error) {
	req, err := http.NewRequest("GET", "https://policies.google.com/terms?hl=en-US", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil && err != io.EOF {
		return "", err
	}

	m := rePoliciesCountry.FindSubmatch(body)
	if m == nil {
		return "", nil
	}

	a3 := string(m[1])
	c := countries.ByName(a3)
	if c == countries.Unknown {
		return a3, nil
	}
	return c.Alpha2(), nil
}
