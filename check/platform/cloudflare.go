// Package platform 解锁检测平台
package platform

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/convert"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

var CfCdnApis = []string{
	"https://4.ipw.cn",
	"https://www.cloudflare.com",
	"https://api.ipify.org",
	"https://iplark.com",
	"https://ifconfig.co",
	"https://api.ip2location.io",
	"https://api.ip.sb",
	"https://realip.cc",
	"https://ipapi.co",
	"https://free.freeipapi.com",
	"https://api.myip.com",
	"https://api.ipbase.com",
	"https://api.ipquery.io",
}

// cfCommonHeaders 请求头，避免被 ban
func cfCommonHeaders() map[string]string {
	return map[string]string{
		"User-Agent":      convert.RandUserAgent(),
		"Accept-Language": "en-US,en;q=0.5",
		// "Accept":             "*/*",
		"Sec-Ch-Ua":          "\"Chromium\";v=\"122\", \"Google Chrome\";v=\"122\", \"Not A(Brand\";v=\"99\"",
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": "\"Windows\"",
		"Accept":             "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	}
}

// CheckCloudflare 检测当前客户端是否可以访问 Cloudflare CDN。
// ctx 为调用方（测活 stage）上下文：取消后立即停止重试/回退，
// 否则 ForceClose 后每个在途节点还要白烧最多 ~20s。
func CheckCloudflare(ctx context.Context, httpClient *http.Client) (cloudflare bool, cfRelayLoc string, cfRelayIP string) {
	const retries = 2
	var ok bool
	var err error

	for i := range retries {
		if ctx.Err() != nil {
			return false, "", ""
		}
		ok, err = checkCFEndpoint(ctx, httpClient, "http://cp.cloudflare.com/generate_204", 204)
		if ok {
			break
		}
		if err == nil && !ok {
			break
		}
		if i < retries-1 {
			select {
			case <-ctx.Done():
				return false, "", ""
			case <-time.After(time.Duration(i+1) * 300 * time.Millisecond):
			}
		}
	}

	if err != nil || !ok {
		slog.Debug("Cloudflare 204 连通性预检失败", "error", err, "ok", ok)
		cfRelayLoc, cfRelayIP = GetCFTrace(ctx, httpClient)

		if cfRelayLoc != "" && cfRelayIP != "" {
			slog.Debug("Cloudflare CDN 检测成功", "loc", cfRelayLoc, "ip", cfRelayIP)
			return true, cfRelayLoc, cfRelayIP
		}

		return false, "", ""
	}
	slog.Debug("Cloudflare 204 连通性 OK")
	return true, "", ""
}

// GetCFTrace 获取 Cloudflare Trace 的 loc 和 ip,并设置 10s 超时
// （超时基于父 ctx 派生，父取消时立即返回）
func GetCFTrace(ctx context.Context, httpClient *http.Client) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return FetchCFTraceFirstConcurrent(httpClient, ctx, cancel)
}

// shuffle 返回一个新切片，元素是原切片的随机乱序版本
func shuffle(in []string) []string {
	out := append([]string(nil), in...)
	rand.Shuffle(len(out), func(i, j int) {
		out[i], out[j] = out[j], out[i]
	})

	return out
}

// FetchCFTraceFirstConcurrent 并发处理 FetchCFCDNTrace
func FetchCFTraceFirstConcurrent(httpClient *http.Client, ctx context.Context, cancel context.CancelFunc) (string, string) {
	type result struct {
		loc string
		ip  string
	}

	// 乱序 + 截取前1, 减轻网络负载
	apis := shuffle(CfCdnApis)
	if len(apis) > 1 {
		apis = apis[:1]
	}

	resultChan := make(chan result, 1)
	var once sync.Once
	var wg sync.WaitGroup

	// 下限 1：sub-urls-retry: 0 会让整个 CF-trace 零次尝试，
	// 导致 drop-bad-cf-nodes 把所有走不通 generate_204 的节点全部误杀
	retries := max(1, config.Get().SubUrlsReTry)

	for _, baseURL := range apis {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			for range retries {
				select {
				case <-ctx.Done():
					return
				default:
				}
				loc, ip := FetchCFTrace(httpClient, ctx, url)
				if loc != "" && ip != "" {
					once.Do(func() {
						resultChan <- result{loc, ip}
						cancel()
					})
					return
				}
				// 失败后短暂退避再重试
				select {
				case <-ctx.Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
		}(baseURL)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	select {
	case r := <-resultChan:
		return r.loc, r.ip
	case <-ctx.Done():
		return "", ""
	}
}

// FetchCFTrace 从cloudflare 的cdn-cgi/trace API获取CDN节点位置
func FetchCFTrace(httpClient *http.Client, ctx context.Context, baseURL string) (string, string) {
	url := utils.JoinURL(baseURL, "cdn-cgi/trace")

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", ""
	}

	for key, value := range cfCommonHeaders() {
		req.Header.Set(key, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", ""
	}
	// 修复：非 200 时原先直接 return，响应体未关闭导致连接泄漏
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	// 增加 LimitReader，防止罕见的恶意节点返回无限数据
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024))
	if err != nil && err != io.EOF {
		return "", ""
	}

	var loc, ip string
	for line := range strings.SplitSeq(string(body), "\n") {
		if after, ok := strings.CutPrefix(line, "loc="); ok {
			loc = after
		}
		if after, ok := strings.CutPrefix(line, "ip="); ok {
			ip = after
		}
	}
	return loc, ip
}

// checkCFEndpoint 检查指定的 Cloudflare 端点是否可达，并返回是否成功和错误信息
func checkCFEndpoint(parent context.Context, httpClient *http.Client, url string, expectedStatus int) (bool, error) {
	timeout := 3 * time.Second
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false, err
	}

	for key, value := range cfCommonHeaders() {
		req.Header.Set(key, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case expectedStatus:
		slog.Debug("正常访问 CF 204")
		return true, nil
	case 403:
		slog.Debug("CF 代理访问自身返回 403，剔除")
		return false, nil
	default:
		slog.Debug("CF 返回非预期状态码", "code", resp.StatusCode)
		return false, nil
	}
}
