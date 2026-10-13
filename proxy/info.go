package proxies

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/sinspired/checkip/pkg/ipinfo"
	"github.com/sinspired/subs-check-pro/v3/config"
)

var ipAPIs = []string{
	// 可尝试在api后加 /cdn-cgi/trace, 如能返回loc则不可使用.
	"https://check.torproject.org/api/ip",
	"https://qifu-api.baidubce.com/ip/local/geo/v1/district",
	"https://r.inews.qq.com/api/ip2city",
	"https://g3.letv.com/r?format=1",
	"https://cdid.c-ctrip.com/model-poc2/h",
	"https://whois.pconline.com.cn/ipJson.jsp",
	"https://api.live.bilibili.com/xlive/web-room/v1/index/getIpInfo",
	// "https://6.ipw.cn/",                  // IPv4使用了 CFCDN, IPv6 位置准确
	"https://api6.ipify.org?format=json", // IPv4使用了 CFCDN, IPv6 位置准确
}

var geoAPIs = []string{
	"https://ip.122911.xyz/api/ipinfo",
	"https://ident.me/json",
	"https://tnedi.me/json",
	"https://api.seeip.org/geoip",
	"https://checkip.info/json",
}

var ipAPIsMe = []string{}

var geoAPIsMe = []string{
	"https://ip.122911.xyz/api/ipinfo",
}

// NewIPInfoClient 创建 ipinfo 检测客户端
func NewIPInfoClient(httpClient *http.Client, db *maxminddb.Reader, ipList, geoList []string) (*ipinfo.Client, error) {
	// 从当前项目 (subs-check-pro) 的全局配置中读取 ISP 设置
	// 注意：快照只读，钳制值放局部变量——旧代码写回指针会把用户配置原地改掉
	cfg := config.Get()

	ispTimeout := cfg.ISPTimeout
	if ispTimeout <= 0 {
		ispTimeout = 5 // 默认 5 秒，最高 15 秒
	} else if ispTimeout > 15 {
		ispTimeout = 15 // 最大 15 秒
	}

	// 映射为 ipinfo 工具包所需的配置结构
	ispCfg := ipinfo.ISPConfig{
		ISPCheck:                 cfg.ISPCheck,
		ISPTimeout:               time.Duration(ispTimeout) * time.Second,
		ISPCheckAPIKeyIPAPI:      cfg.ISPCheckAPIKeyIPAPI,
		ISPCheckAPIKeyProxyCheck: cfg.ISPCheckAPIKeyProxyCheck,
		ISPCheckAPIKeyIPLocate:   cfg.ISPCheckAPIKeyIPLocate,
		ISPCheckAPIKeyIPData:     cfg.ISPCheckAPIKeyIPData,
	}

	// 初始化客户端并注入配置
	return ipinfo.New(
		ipinfo.WithHTTPClient(httpClient),
		ipinfo.WithDBReader(db),
		ipinfo.WithIPAPIs(ipList...),
		ipinfo.WithGeoAPIs(geoList...),
		ipinfo.WithISPConfig(ispCfg),
	)
}

// GetProxyCountry 获取代理节点的位置代码，使用 github.com/sinspired/checkip/pkg/ipinfo API 获取 Analyzed 结果：
//
// - BadCFNode: HK⁻¹
//
// - CFNodeWithSameCountry: HK¹⁺
//
// - CFNodeWithDifferentCountry: HK¹-US⁰
//
// - NodeWithoutCF: HK²
//
// - 前两位字母是实际浏览网站识别的位置, -US⁰为使用CF CDN服务的网站识别的位置, 比如GPT, X等
func GetProxyCountry(httpClient *http.Client, db *maxminddb.Reader, getAnalyzedCtx context.Context, cfLoc string, cfIP string) (loc, ip, countryCodeTag, ispTag string, err error) {
	cliMe, err := NewIPInfoClient(httpClient, db, ipAPIsMe, geoAPIsMe)
	if err != nil || cliMe == nil {
		slog.Debug("创建 MeAPI 客户端失败", "error", err)
		goto NextClient
	} else {
		defer func() {
			if cerr := cliMe.Close(); cerr != nil {
				slog.Warn("关闭 ipinfo 客户端失败", "err", cerr)
			}
		}()
	}

	for range config.Get().SubUrlsReTry {
		loc, ip, countryCodeTag, ispTag, err = cliMe.GetAnalyzed(getAnalyzedCtx, cfLoc, cfIP)
		if err != nil {
			slog.Debug("MeAPI 获取节点位置失败", "error", err)
		}
		if loc != "" && countryCodeTag != "" {
			slog.Debug("MeAPI 获取节点位置成功", "ip", ip, "loc", loc, "code", countryCodeTag, "isp", ispTag)
			return loc, ip, countryCodeTag, ispTag, nil
		} else {
			slog.Debug("MeAPI 获取节点位置失败", "ip", ip, "loc", loc, "code", countryCodeTag)
		}
	}

NextClient:
	// 如失败，使用混合检测，不需要多次重试
	cli, err := NewIPInfoClient(httpClient, db, ipAPIs, geoAPIs)
	if err != nil || cli == nil {
		slog.Debug("创建 ipinfo 主客户端失败", "error", err)
	} else {
		defer func() {
			if cerr := cli.Close(); cerr != nil {
				slog.Warn("关闭 ipinfo 客户端失败", "err", cerr)
			}
		}()
		loc, ip, countryCodeTag, ispTag, err = cli.GetAnalyzed(getAnalyzedCtx, cfLoc, cfIP)
		if err != nil {
			slog.Debug("Analyzed 获取节点位置失败", "error", err)
			return "", "", "", "", err
		}
		if loc != "" && countryCodeTag != "" {
			slog.Debug("Analyzed 获取节点位置成功", "ip", ip, "loc", loc, "code", countryCodeTag, "isp", ispTag)
			return loc, ip, countryCodeTag, ispTag, nil
		} else {
			slog.Debug("Analyzed 获取节点位置空白", "ip", ip, "loc", loc, "code", countryCodeTag)
		}
	}
	return "", "", "", "", err
}
