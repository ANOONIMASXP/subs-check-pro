// Package app: server.go
package app

import (
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/save/method"
)

const (
	DefaultPort     = ":8199"
	SubPath         = "/sub"
	HeaderFromCheck = "X-From-Subs-Check-pro"
	QueryFromCheck  = "from_subs_check_pro"
)

// publicStaticFileList 公共规则文件入口，无需鉴权
var publicStaticFileList = []struct {
	Route string // HTTP 路由路径
	File  string // 对应文件名
}{
	{"/bdg.yaml", "bdg.yaml"},
}

func init() {
	// 全局注册扩展名的 MIME 类型并强制指定 utf-8 编码，解决浏览器直接打开时的乱码问题
	// 使用 text/plain 可以让浏览器直接展示 yaml 而不是触发下载
	mime.AddExtensionType(".yaml", "text/plain; charset=utf-8")
	mime.AddExtensionType(".yml", "text/plain; charset=utf-8")
	mime.AddExtensionType(".txt", "text/plain; charset=utf-8")
	mime.AddExtensionType(".md", "text/plain; charset=utf-8")
	mime.AddExtensionType(".js", "application/javascript; charset=utf-8")
	mime.AddExtensionType(".json", "application/json; charset=utf-8")
}

// initHTTPServer 初始化并启动HTTP服务器
func (app *App) initHTTPServer() error {
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(app.accessLogMiddleware())

	saver, err := method.NewLocalSaver()
	if err != nil {
		return fmt.Errorf("获取http监听目录失败: %w", err)
	}

	app.registerStaticRoutes(router, saver.OutputPath)

	listenAddr := normalizeListenAddr(config.Get().ListenPort)
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: router,
	}
	app.httpServer = srv

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP服务器启动失败", "error", err)
		}
	}()

	slog.Info("HTTP 服务器启动", "port", strings.TrimPrefix(listenAddr, ":"))
	return nil
}

// accessLogMiddleware 访问日志走 slog（tint → lineClearWriter，写入前带
// \r\x1b[K 清行，遵守 AGENTS §7 终端约定）。
// 替代旧的 gin.Logger()(c)：它直写 stdout 会与进度条行粘连（AGENTS §7 点名
// 防止复发的 bug），且每请求都新建一个 logger 实例。
// 级别策略：5xx=Warn、4xx=Info、其余=Debug（默认级别下成功请求不再刷屏，
// 排查时开 Debug 可见全量）；软件自身请求依旧静默。
func (app *App) accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Query().Get(QueryFromCheck) == "true" ||
			strings.EqualFold(c.GetHeader(HeaderFromCheck), "true") {
			c.Next()
			return
		}

		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		args := []any{
			"status", status,
			"method", c.Request.Method,
			"path", c.Request.URL.RequestURI(),
			"latency", time.Since(start).Round(time.Millisecond),
			"client", c.ClientIP(),
		}
		switch {
		case status >= 500:
			slog.Warn("HTTP", args...)
		case status >= 400:
			slog.Info("HTTP", args...)
		default:
			slog.Debug("HTTP", args...)
		}
	}
}

// registerStaticRoutes 注册静态路由
//
// 规则文件与订阅结果文件均为公开访问，无需鉴权。
func (app *App) registerStaticRoutes(router *gin.Engine, outputPath string) {
	rulesDir := outputPath
	subDir := filepath.Join(outputPath, "sub")

	// 公共静态文件映射（无需鉴权），从包级变量读取
	for _, f := range publicStaticFileList {
		router.StaticFile(f.Route, filepath.Join(rulesDir, f.File))
		router.StaticFile(SubPath+f.Route, filepath.Join(rulesDir, f.File))
	}

	// 订阅结果文件（公开）
	resultFiles := map[string]string{
		"/all.yaml":     "all.yaml",     // 最新节点
		"/history.yaml": "history.yaml", // 历史节点
		// 保存侧实际写的是 base64.txt（save 包），旧路由 /base64.yaml 曾恒 404；
		// 路由名按 AGENTS §5 保留并指向真实文件，同时补同名别名
		"/base64.yaml":  "base64.txt",   // Base64 格式（历史路由名）
		"/base64.txt":   "base64.txt",   // Base64 格式（文件同名别名）
		"/mihomo.yaml":  "mihomo.yaml",  // Mihomo 格式
		"/singbox.json": "singbox.json", // Sing-Box 配置（由 Sub-Store 转换）
	}
	for routePath, fileName := range resultFiles {
		// 映射到 outputPath/sub 下的文件
		router.StaticFile(routePath, filepath.Join(subDir, fileName))
		// 同时提供 /sub 路径访问
		router.StaticFile(SubPath+routePath, filepath.Join(subDir, fileName))
	}
}

// checkPortFree 在启动服务前检测端口是否可用。
// 返回 true 表示端口空闲，可以绑定；返回 false 表示已被其他进程占用。
func checkPortFree(listenAddr string) bool {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// normalizeListenAddr 处理监听端口
func normalizeListenAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultPort
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 65535 {
		return ":" + s
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
			return net.JoinHostPort(host, port)
		}
		return DefaultPort
	}
	return DefaultPort
}

// AnalysisReportPath 返回分析报告路径
func AnalysisReportPath() (string, error) {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return "", fmt.Errorf("获取http监听目录失败: %w", err)
	}
	return filepath.Join(saver.OutputPath, "stats", "subs-analysis.yaml"), nil
}

func loadHistoricalCheckRate() {
	reportPath, err := AnalysisReportPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return
	}

	var report struct {
		CheckInfo struct {
			CheckCountRaw     string `yaml:"check_count_raw"`
			CheckDurationRaw  int64  `yaml:"check_duration_raw"`
			CheckSuccessLimit int64  `yaml:"check_success_limit"`
		} `yaml:"check_info"`
	}
	if err := yaml.Unmarshal(data, &report); err != nil {
		return
	}

	count := parseHistNodeCount(report.CheckInfo.CheckCountRaw)
	durSec := float64(report.CheckInfo.CheckDurationRaw)
	if count > 0 && durSec > 0 {
		rate := count / durSec
		if report.CheckInfo.CheckSuccessLimit > 0 && config.Get().SuccessLimit == 0 {
			rate *= 0.85
		}
		check.SetHistoricalRate(rate)
		slog.Debug("历史检测速率加载", "rate", fmt.Sprintf("%.1f 节点/秒", rate))
	}
}

func parseHistNodeCount(s string) float64 {
	s = strings.NewReplacer(",", "", "，", "").Replace(strings.TrimSpace(s))
	if strings.Contains(s, "万") {
		if n, err := strconv.ParseFloat(strings.ReplaceAll(s, "万", ""), 64); err == nil {
			return n * 10000
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err == nil {
		return n
	}
	return 0
}
