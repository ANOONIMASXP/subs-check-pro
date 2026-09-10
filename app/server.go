// Package app: server.go
package app

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"github.com/sinspired/subs-check-pro-webui/webui"
	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/save/method"
	"github.com/sinspired/subs-check-pro/v3/substore"
)

const (
	DefaultPort     = ":8199"
	LogTimeFormat   = "2006-01-02 15:04:05"
	ShareDirName    = "more"
	TemplatePattern = "templates/*.html"
	StaticPrefix    = "/static"
	SubPath         = "/sub"
	SharePath       = "/share"
	PublicPath      = "/more"
	FilesPath       = "/files"
	SubInfoPath     = substore.SubInfoPath
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
	router.Use(app.silentLoggerMiddleware())

	// 加载模板（share/files 页面依赖）
	router.SetHTMLTemplate(template.Must(template.New("").ParseFS(webui.TemplatesFS, TemplatePattern)))

	// 注册静态资源，share/files 页面依赖它们
	staticSub, _ := fs.Sub(webui.StaticFS, "static")
	router.StaticFS(StaticPrefix, http.FS(staticSub))

	saver, err := method.NewLocalSaver()
	if err != nil {
		return fmt.Errorf("获取http监听目录失败: %w", err)
	}

	app.registerStaticRoutes(router, saver.OutputPath)
	// 注册订阅流量信息路由
	app.registerSubscriptionInfoRoute(router)

	if err := app.registerShareRoutes(router, saver.OutputPath); err != nil {
		slog.Error("注册分享路由失败", "error", err)
	}

	listenAddr := normalizeListenAddr(config.GlobalConfig.ListenPort)
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

// silentLoggerMiddleware 通过软件自身发出的部分请求，不显示日志
func (app *App) silentLoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Query().Get(QueryFromCheck) == "true" ||
			strings.EqualFold(c.GetHeader(HeaderFromCheck), "true") {
			c.Next()
		} else {
			gin.Logger()(c)
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
		"/base64.yaml":  "base64.yaml",  // Base64 格式
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

// registerShareRoutes 注册分享路由
func (app *App) registerShareRoutes(router *gin.Engine, outputPath string) error {
	publicShareDir := outputPath
	encryptedShareDir := filepath.Join(outputPath, "sub") // 加密分享

	// 1. 加密分享路由 (/sub/...)
	// 匹配 /sub/分享码/文件名
	router.GET(SubPath+"/:code/*filepath", app.handleEncryptedShare(encryptedShareDir))
	// 匹配 /sub 和 /sub/（处理未输入分享码的情况）
	router.GET(SubPath, app.handleEncryptedShare(encryptedShareDir))
	router.GET(SubPath+"/", app.handleEncryptedShare(encryptedShareDir))
	router.GET(SharePath, app.handleEncryptedShare(encryptedShareDir))
	router.GET(SharePath+"/", app.handleEncryptedShare(encryptedShareDir))

	// 2. 公开分享路由 (/more/...)
	moreDirPath := filepath.Join(publicShareDir, ShareDirName)
	if _, err := os.Stat(moreDirPath); os.IsNotExist(err) {
		if err := os.MkdirAll(moreDirPath, 0o755); err != nil {
			return err
		}
	}
	router.GET(PublicPath+"/*filepath", app.handleFileShare(moreDirPath, false))

	// 分享索引页：展示所有分享入口
	router.GET(FilesPath, app.handleFilesIndex)

	return nil
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
		if report.CheckInfo.CheckSuccessLimit > 0 && config.GlobalConfig.SuccessLimit == 0 {
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
