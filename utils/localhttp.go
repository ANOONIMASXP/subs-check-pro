package utils

import (
	"io"
	"net/http"
	"time"
)

// LocalHTTPClient 面向本机服务（Sub-Store 后端、mihomo external-controller）的共享客户端。
//
// 三个关键点：
//   - 30s 总超时：旧实现直接用 http.Get / http.DefaultClient（零超时），
//     本机服务卡死时会连带把保存流水线无限期挂住，且持有 subStoreMu；
//   - Proxy: nil：本机回环请求不吃系统代理，避免代理故障放大成本机服务不可用；
//   - ForceAttemptHTTP2: false：内置 Sub-Store(Loon) 对 HTTP/2 支持不完整，走 HTTP/1.1。
var LocalHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,

		// 强制 HTTP/1.1，避免本地服务对 HTTP/2 支持不完整
		ForceAttemptHTTP2: false,

		// 大连接池，避免 TIME_WAIT 导致失败
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,

		DisableKeepAlives:  false,
		DisableCompression: false,
	},
}

// DrainBody 读空并丢弃响应体残余（上限 32KB），让连接可以复用。
// 非 2xx 或提前返回的错误路径必须调用，否则连接只能等超时关闭。
func DrainBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
}
