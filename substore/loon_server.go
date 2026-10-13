// sub-store\loon_server.go
package substore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/sinspired/subs-check-pro/v3/assets"
	"github.com/sinspired/subs-check-pro/v3/config"
)

// LoonServer 现在只持有单个 engine
// engine 用 atomic.Pointer 存放，支持资产更新后不重启进程即可热替换。
type LoonServer struct {
	engine      atomic.Pointer[LoonEngine]
	backendPath string
	httpServer  *http.Server

	// baseCtx 保证哪怕客户端连接断开了，脚本也一定在后台执行完毕并预热缓存
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// inflight 用于并发请求合并：多个相同的订阅请求只会触发一次底层的脚本执行
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	resp *LoonHTTPResponse
	err  error
}

func NewLoonServer(addr string, engine *LoonEngine, backendPath string) *LoonServer {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s := &LoonServer{
		backendPath: backendPath,
		baseCtx:     baseCtx,
		baseCancel:  baseCancel,
		inflight:    make(map[string]*inflightCall),
	}
	s.engine.Store(engine)
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.httpServer = &http.Server{Addr: addr, Handler: mux}
	return s
}

func (s *LoonServer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
	// 预检请求里声明了什么头就放行什么头：通过官方前端 https://sub-store.vercel.app
	// 填写本后端地址使用时，浏览器可能携带额外的自定义请求头，写死白名单会被预检拒绝。
	allowHeaders := "Origin, X-Requested-With, Content-Type, Accept, Authorization"
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		allowHeaders = reqHeaders
	}
	w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
	if r.Method == http.MethodOptions {
		// Chrome 的私有网络访问(PNA)：公网页面(官方前端)访问局域网/本机后端时，
		// 预检会带 Access-Control-Request-Private-Network，必须明确回应才会放行。
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 本程序自带的后端图标：官方前端读取 SUB_STORE_BACKEND_CUSTOM_ICON（/scp/scp-app.svg），
	// 本 fork 无本地前端，图标直接由该后端路由提供。
	if r.URL.Path == "/scp/scp-app.svg" {
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(assets.EmbeddedScpAppLogo)
		return
	}

	host := strings.Split(r.Host, ":")[0]
	path := r.URL.Path

	isBackend := false

	// 规则 A：匹配配置的安全后端前缀
	if s.backendPath != "" && strings.HasPrefix(path, s.backendPath) {
		isBackend = true
	}
	// 规则 B：匹配 sub.store 局域网/代理环境劫持
	if host == "sub.store" {
		if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/download/") {
			isBackend = true
		}
	}

	if !isBackend {
		http.NotFound(w, r)
		return
	}

	s.handleBackend(w, r)
}

func (s *LoonServer) handleBackend(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 30<<20))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}

	// 剥离安全前缀，构造供脚本内部路由匹配的路径 (如 /api/sync)
	strippedPath := strings.TrimPrefix(r.URL.Path, s.backendPath)
	if strippedPath == "" || !strings.HasPrefix(strippedPath, "/") {
		strippedPath = "/" + strippedPath
	}

	// 动态判断 Scheme (兼容 TLS 和 Nginx 等反向代理)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	// 3. 构造完整 URL：依赖真实请求的 r.Host
	fullURL := scheme + "://" + r.Host + strippedPath
	if r.URL.RawQuery != "" {
		fullURL += "?" + r.URL.RawQuery
	}

	req := &LoonHTTPRequest{
		URL:     fullURL,
		Method:  r.Method,
		Headers: headers,
		Body:    string(bodyBytes),
	}

	// 4. 处理跨域 Origin 标识
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = scheme + "://" + r.Host
	}
	argument := "cors=" + url.QueryEscape(origin)

	// /download/* 是幂等的只读请求：脚本执行与客户端连接解耦并合并并发请求。
	// /api/utils/backup 和 /api/sync/artifacts 等云同步操作也解耦，保证即使前端超时断开，后台也能完整跑完备份逻辑。
	detach := (r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodPost) &&
		(strings.HasPrefix(strippedPath, "/download/") ||
			strings.HasPrefix(strippedPath, "/api/utils/backup") ||
			strings.HasPrefix(strippedPath, "/api/sync/artifacts"))

	var resp *LoonHTTPResponse
	if detach {
		// 加上 UA 做特征，确保生成不同客户端对应的订阅时不冲突
		key := r.Method + "|" + fullURL + "|" + r.Header.Get("User-Agent") + "|" + argument
		resp, err = s.executeDetached(r.Context(), key, req, argument)
	} else {
		resp, err = s.engine.Load().Execute(r.Context(), req, argument)
	}
	if err != nil {
		// 客户端已经断开：不是服务端错误，连接也已不可写，安静返回即可。
		if r.Context().Err() != nil {
			slog.Debug("Sub-Store 请求被客户端取消", "url", fullURL, "reason", r.Context().Err())
			return
		}
		if errors.Is(err, context.Canceled) {
			slog.Debug("Sub-Store 脚本执行被取消（服务停止中）", "url", fullURL)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		slog.Error("Sub-Store 脚本执行失败", "error", err, "url", fullURL)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}

	// 环境变量注入
	if strings.Contains(req.URL, "/api/utils/env") {
		var envData map[string]any
		if err := json.Unmarshal([]byte(resp.Body), &envData); err == nil {
			if data, ok := envData["data"].(map[string]any); ok {
				meta, _ := data["meta"].(map[string]any)
				if meta == nil {
					meta = make(map[string]any)
					data["meta"] = meta
				}
				node, _ := meta["node"].(map[string]any)
				if node == nil {
					node = make(map[string]any)
					meta["node"] = node
				}
				envMap, _ := node["env"].(map[string]any)
				if envMap == nil {
					envMap = make(map[string]any)
					node["env"] = envMap
				}

				// 设置后端名称和图标
				envMap["SUB_STORE_BACKEND_CUSTOM_NAME"] = "Subs Check Pro"
				envMap["SUB_STORE_BACKEND_CUSTOM_ICON"] = "/scp/scp-app.svg"

				// 注入配置变量
				envMap["SUB_STORE_FRONTEND_BACKEND_PATH"] = config.Get().SubStorePath
				envMap["SUB_STORE_BODY_JSON_LIMIT"] = "30mb"
				envMap["SUB_STORE_CORS_ALLOWED_ORIGINS"] = "*"

				if modifiedBody, err := json.Marshal(envData); err == nil {
					resp.Body = string(modifiedBody)
				}
			}
		}
	}

	for k, v := range resp.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		w.Header().Set(k, v)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(resp.Body))
}

func (s *LoonServer) Start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("Sub-Store(SCP) 服务异常退出", "error", err)
		}
	}()

	// 自己给自己发个 OPTIONS 请求，收到回复才算真正的完全启动
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if ok {
		client := &http.Client{Timeout: 100 * time.Millisecond}
		probeURL := fmt.Sprintf("http://127.0.0.1:%d/", tcpAddr.Port)

		started := false
		for i := 0; i < 20 && !started; i++ {
			req, _ := http.NewRequest(http.MethodOptions, probeURL, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				started = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !started {
			s.httpServer.Close()
			return fmt.Errorf("HTTP 引擎探活超时，服务无法响应")
		}
	}
	return nil // 返回 nil 说明端口已通
}

func (s *LoonServer) Shutdown(ctx context.Context) error {
	s.baseCancel() // 取消所有脱离客户端连接运行的脚本
	return s.httpServer.Shutdown(ctx)
}

// executeDetached 在与客户端连接无关的 Context 中执行脚本，并合并相同 key 的并发请求。
// 调用方等待结果时仍会响应自身的 clientCtx：客户端断开只会让它提前返回，
// 后台脚本继续跑完，结果由 Sub-Store 自身缓存，供下一次请求（或定时缓存任务）命中。
func (s *LoonServer) executeDetached(clientCtx context.Context, key string, req *LoonHTTPRequest, argument string) (*LoonHTTPResponse, error) {
	s.inflightMu.Lock()
	call, ok := s.inflight[key]
	if !ok {
		call = &inflightCall{done: make(chan struct{})}
		s.inflight[key] = call
		engine := s.engine.Load()
		go func() {
			defer func() {
				if r := recover(); r != nil {
					call.err = fmt.Errorf("脚本执行异常: %v", r)
				}
				s.inflightMu.Lock()
				delete(s.inflight, key)
				s.inflightMu.Unlock()
				close(call.done)
			}()
			// 使用与连接脱钩的 baseCtx 执行
			call.resp, call.err = engine.Execute(s.baseCtx, req, argument)
			// 记录由于客户端断开导致的后台执行错误，避免错误被静默吞噬
			if call.err != nil {
				slog.Debug("后台脱机任务执行失败", "url", req.URL, "err", call.err)
			}
		}()
	}
	s.inflightMu.Unlock()

	select {
	case <-call.done:
		return call.resp, call.err
	case <-clientCtx.Done():
		return nil, clientCtx.Err()
	}
}
