// Package substore 用来处理 Sub-Store js引擎、运行、资源等
// substore\sub_run.go
package substore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/sinspired/subs-check-pro/v3/assets"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/save/method"
	"github.com/sinspired/subs-check-pro/v3/utils"
	"gopkg.in/natefinch/lumberjack.v2"
)

// initSubStorePath 存引擎实际生效的后端路径前缀。
// 写方：引擎启动 goroutine（resolveBackendPath）；读方：app 配置热重载 goroutine
// （onConfigChange）→ 必须原子访问。
var initSubStorePath atomic.Pointer[string]

// GetInitSubStorePath 返回引擎已解析的后端路径前缀；从未启动过时返回空串。
func GetInitSubStorePath() string {
	if p := initSubStorePath.Load(); p != nil {
		return *p
	}
	return ""
}

func setInitSubStorePath(p string) {
	initSubStorePath.Store(&p)
}

// 当前 RunSubStoreService 实例的生命周期句柄。
// 每个实例独享子 ctx + done：Stop/ReStart 通过 cancel 收敛旧实例并等待其
// 真正退出（startSubStore 的 defer 会关日志句柄、释放端口），替代旧的
// 固定 sleep + 在同一 app ctx 上无限叠加 goroutine 的做法（每次配置变更
// 泄漏一个 parked goroutine 和一个打开的 lumberjack 文件句柄）。
var (
	serviceMu     sync.Mutex
	serviceCancel context.CancelFunc
	serviceDone   chan struct{}
)

// stopCurrentService 收敛当前实例（如存在）并等待其完全退出。
// 返回 false 表示等待超时（调用方可记录告警）。
func stopCurrentService(wait time.Duration) bool {
	serviceMu.Lock()
	cancel := serviceCancel
	done := serviceDone
	serviceMu.Unlock()

	if cancel == nil {
		return true
	}
	cancel()

	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

var IsSubStoreRunning atomic.Bool

// currentLoonServer 持有当前运行中的 Loon 模拟服务
var currentLoonServer atomic.Pointer[LoonServer]

type subStorePaths struct {
	substoreDir    string
	jsPath         string
	backendVerPath string
	kvStorePath    string
	logPath        string
}

func parseVersion(v string) *semver.Version {
	ver, _ := semver.NewVersion(strings.TrimSpace(v))
	return ver
}

// getSubStorePaths 获取 Sub-Store 相关路径
func getSubStorePaths() (*subStorePaths, error) {
	saver, err := method.NewLocalSaver()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(saver.OutputPath) {
		saver.OutputPath = filepath.Join(saver.BasePath, saver.OutputPath)
	}

	substoreDir := filepath.Join(saver.OutputPath, "sub-store")

	return &subStorePaths{
		substoreDir:    substoreDir,
		jsPath:         filepath.Join(substoreDir, "sub-store.min.js"),
		backendVerPath: filepath.Join(substoreDir, "backend.version"),
		kvStorePath:    filepath.Join(substoreDir, "sub-store.json"),
		logPath:        filepath.Join(substoreDir, "sub-store.log"),
	}, nil
}

// 2. 添加一个纯文本日志 Handler
// 拦截 slog 输出，输出 中文日期 + [sub-store] 的完美格式
type plainLogHandler struct {
	writer io.Writer
}

func (h *plainLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *plainLogHandler) Handle(ctx context.Context, r slog.Record) error {
	msg := strings.TrimSpace(r.Message)
	if msg == "" {
		return nil
	}

	// 收集结构化字段：之前这里完全没调用 r.Attrs，所有 e.logger.Info("msg", "k", v)
	// 里附带的 key=value（包括我们埋的耗时诊断数据）全部被无声丢弃，只剩裸消息文本。
	var attrBuf strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		attrBuf.WriteByte(' ')
		attrBuf.WriteString(a.Key)
		attrBuf.WriteByte('=')
		attrBuf.WriteString(fmt.Sprint(a.Value.Any()))
		return true
	})
	attrSuffix := attrBuf.String()

	// 1. 横幅直接原样输出，不带时间戳
	if strings.Contains(msg, "┅┅┅┅") || strings.Contains(msg, "Sub-Store -- v") {
		fmt.Fprintln(h.writer, msg)
		return nil
	}

	// 2. 拼接中文日期时间格式（例如：2026年9月8日 下午6:51:36）
	t := r.Time
	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	ampm := "上午"
	if hour >= 12 {
		ampm = "下午"
		if hour > 12 {
			hour -= 12
		}
	}
	if hour == 0 {
		hour = 12
	}
	timeStr := fmt.Sprintf("%d年%d月%d日 %s%d:%02d:%02d", year, int(month), day, ampm, hour, min, sec)

	// 3. 补充前缀
	if !strings.HasPrefix(msg, "[sub-store]") && !strings.HasPrefix(msg, "使用") && !strings.HasPrefix(msg, "请求") {
		levelStr := "INFO"
		switch r.Level {
		case slog.LevelError:
			levelStr = "ERROR"
		case slog.LevelWarn:
			levelStr = "WARN"
		}
		msg = fmt.Sprintf("[sub-store] %s: %s", levelStr, msg)
	}

	// 最终写入日志文件（附带结构化字段，诊断数据现在能看到了）
	fmt.Fprintf(h.writer, "%s %s%s\n", timeStr, msg, attrSuffix)
	return nil
}

func (h *plainLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *plainLogHandler) WithGroup(name string) slog.Handler       { return h }

func newSubStoreLogger(logPath string) (*slog.Logger, io.Closer) {
	writer := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    10, // 10MB
		MaxBackups: 3,  // 3 files
		MaxAge:     14, // 14 days
	}
	// 挂载完美复刻的日志拦截器
	return slog.New(&plainLogHandler{writer: writer}), writer
}

func logStop(port string) {
	if port != "" {
		slog.Warn("Sub-Store 服务停止", "port", port)
	} else {
		slog.Warn("Sub-Store 服务禁用", "port", "未设置")
	}
}

// RunSubStoreService 运行 Sub-Store 服务（Loon API 模拟方案，无 Node 依赖），支持 ctx，可被外部取消
func RunSubStoreService(ctx context.Context) {
	listenPort := strings.TrimPrefix(config.Get().ListenPort, ":")
	subStorePort := strings.TrimPrefix(config.Get().SubStorePort, ":")

	if subStorePort == "" {
		IsSubStoreRunning.Store(false)
		return
	}

	port, err := strconv.Atoi(subStorePort)
	if err != nil || port < 1 || port > 65535 {
		slog.Error("SubStore 端口不合法，请检查配置", "port", subStorePort)
		return
	}

	if subStorePort == listenPort {
		slog.Error("SubStore 服务因端口冲突禁用，请修改端口配置")
		return
	}

	// 实例独享子 ctx：父 ctx（app 生命周期）或 stopCurrentService 都能收敛它
	instanceCtx, instanceCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 防御性接管：调用方理应先 Stop，若仍有旧实例在跑则先等它退出，
	// 避免两个实例同时抢同一端口/日志文件
	if !stopCurrentService(10 * time.Second) {
		slog.Warn("旧 Sub-Store 实例退出超时，强制接管")
	}

	serviceMu.Lock()
	serviceCancel = instanceCancel
	serviceDone = done
	serviceMu.Unlock()

	defer func() {
		instanceCancel()
		close(done)
		serviceMu.Lock()
		if serviceDone == done {
			serviceCancel = nil
			serviceDone = nil
		}
		serviceMu.Unlock()
	}()

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Sub-Store 服务发生未捕获的 panic，已拦截",
						"panic", r, "stack", string(debug.Stack()))
				}
			}()
			if err := startSubStore(instanceCtx); err != nil {
				slog.Error("Sub-Store 服务崩溃, 正在重启...", "error", err)
				IsSubStoreRunning.Store(false)
			}
		}()

		select {
		case <-instanceCtx.Done():
			logStop(strings.TrimPrefix(config.Get().SubStorePort, ":"))
			IsSubStoreRunning.Store(false)
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func startSubStore(ctx context.Context) error {
	paths, err := getSubStorePaths()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.substoreDir, 0o755); err != nil {
		return fmt.Errorf("创建 Sub-Store 目录失败: %w", err)
	}

	// 移除旧规则文件
	removeOldSinspiredFiles()

	// 释放 Sub-Store 相关资源（后端脚本等文本资源，直接写出）
	if err := extractAssets(paths); err != nil {
		return err
	}

	// 初始化持久化存储（替代 Node 版本里 sub-store.json 的角色），并做一次性迁移
	store, err := NewLoonKVStore(paths.kvStorePath)
	if err != nil {
		return fmt.Errorf("初始化 Sub-Store 持久化存储失败: %w", err)
	}

	scriptSrc, err := os.ReadFile(paths.jsPath)
	if err != nil {
		return fmt.Errorf("读取 Sub-Store 脚本失败: %w", err)
	}

	subLogger, logCloser := newSubStoreLogger(paths.logPath)
	defer logCloser.Close()

	// 单一未拆分脚本，不再需要 Simple/Core 两套引擎；
	// NewLoonEngine 内部会同步完成 worker 池预热，见 loon_engine.go。
	engine, err := NewLoonEngine(scriptSrc, "Sub-Store", store, subLogger)
	if err != nil {
		return err
	}

	backendPath := resolveBackendPath()

	listenAddr := ":" + strings.TrimPrefix(config.Get().SubStorePort, ":")

	server := NewLoonServer(listenAddr, engine, backendPath)

	// 这里调用 Start() 时，内部会进行端口绑定和 HTTP 探活
	if err := server.Start(); err != nil {
		IsSubStoreRunning.Store(false)
		return fmt.Errorf("启动 Sub-Store(SCP) 服务失败: %w", err)
	}

	currentLoonServer.Store(server)
	defer currentLoonServer.Store(nil)

	IsSubStoreRunning.Store(true)

	slog.Info("Sub-Store 服务启动", "port", strings.TrimPrefix(config.Get().SubStorePort, ":"), "path", backendPath)

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return nil
}

// resolveBackendPath 生成/复用后端 API 路径前缀，行为与旧版 SUB_STORE_FRONTEND_BACKEND_PATH 一致
func resolveBackendPath() string {
	p := strings.TrimSpace(config.Get().SubStorePath)
	if p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
			// copy-on-write 回写，供 BaseURL 推导与热重载比对
			config.Mutate(func(c *config.Config) { c.SubStorePath = p })
		}
	} else {
		p = "/" + utils.GenerateRandomString(20)
		config.Mutate(func(c *config.Config) { c.SubStorePath = p })
		slog.Info("已随机生成", "sub-store-path", p)
	}
	setInitSubStorePath(p)
	return p
}

// writeEmbeddedFile 将嵌入的原始（未压缩）内容直接写入目标文件
func writeEmbeddedFile(data []byte, targetPath string, perm os.FileMode, desc string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("创建 %s 的上层目录失败: %w", desc, err)
	}
	if err := os.WriteFile(targetPath, data, perm); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", desc, err)
	}
	return nil
}

// extractAssets 将嵌入资源释放到磁盘。
func extractAssets(paths *subStorePaths) error {
	var updatedLogs []any

	// 1. 后端脚本
	// 读取内嵌的后端版本与本地版本
	embedBVer := parseVersion(string(assets.EmbeddedSubStoreBackendVer))
	localBVerBytes, _ := os.ReadFile(paths.backendVerPath)
	localBVer := parseVersion(string(localBVerBytes))

	// 判断是否需要覆盖后端的三个条件：
	// a. 本地 JS 文件丢失
	// b. 本地没有版本记录文件
	// c. 二进制内嵌的版本号 > 本地的版本号
	_, jsStatErr := os.Stat(paths.jsPath)
	shouldOverwriteBackend := os.IsNotExist(jsStatErr) || localBVer == nil || (embedBVer != nil && embedBVer.GreaterThan(localBVer))

	if shouldOverwriteBackend {
		if err := writeEmbeddedFile(assets.EmbeddedSubStore, paths.jsPath, 0o644, "Sub-Store 后端脚本"); err != nil {
			return err
		}
		// 同步写入最新的 backend.version 到磁盘
		if embedBVer != nil {
			if err := os.WriteFile(paths.backendVerPath, assets.EmbeddedSubStoreBackendVer, 0o644); err != nil {
				return fmt.Errorf("写入后端版本文件失败: %w", err)
			}
			updatedLogs = append(updatedLogs, "后端", embedBVer.String())
		} else {
			updatedLogs = append(updatedLogs, "Sub-Store 后端脚本", "已写入")
		}
	}

	// 输出聚合日志
	if len(updatedLogs) > 0 {
		slog.Info("Sub-Store 资源更新", updatedLogs...)
	}
	return nil
}

// StopSubStore 优雅关闭当前运行中的 Sub-Store 内置 HTTP 服务。
// 通过实例 ctx 触发退出并等待实例完全结束：startSubStore 在 <-ctx.Done()
// 后执行 server.Shutdown(5s)，随后 defer 关闭日志句柄、清空 currentLoonServer，
// 最外层 defer 关闭 done —— 等到 done 即可确定端口与文件句柄已释放。
func StopSubStore() error {
	serviceMu.Lock()
	existed := serviceCancel != nil
	serviceMu.Unlock()

	if !existed {
		IsSubStoreRunning.Store(false)
		return nil
	}

	if !stopCurrentService(10 * time.Second) {
		IsSubStoreRunning.Store(false)
		return fmt.Errorf("Sub-Store 实例退出等待超时（10s）")
	}
	slog.Info("Sub-Store 服务 已停止")
	IsSubStoreRunning.Store(false)
	return nil
}

// ReStartSubStore 重启 Sub-Store 服务。
// StopSubStore 已确定性等待旧实例退出（端口、日志句柄均已释放），
// 不再需要固定 sleep——旧实现 500ms 常常不够，还会叠加 goroutine/句柄泄漏。
func ReStartSubStore(ctx context.Context) error {
	if err := StopSubStore(); err != nil {
		return err
	}
	go RunSubStoreService(ctx)

	return nil
}

// WaitSubStoreStopped 等待当前运行中的 Sub-Store 实例（LoonServer）完全停止。
// 由于 startSubStore 里对 server.Shutdown(...) 是同步调用、返回后才走 defer
// 清空 currentLoonServer，所以"currentLoonServer 变为 nil"是一个可靠信号，
// 代表底层监听端口已经释放，不是"猜个时间 sleep 一下"。
// 用于端口/路径变更等场景下的重启流程：先 cancel 旧的 ctx，再调用本函数
// 确定性地等旧实例真正退出，再启动新实例绑定同一个端口，避免固定 sleep
// 时间不够（比如旧实例正好有请求在处理，Shutdown 走满 5 秒优雅退出宽限期）
// 导致新实例绑定端口失败、触发不必要的重试。
func WaitSubStoreStopped(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if currentLoonServer.Load() == nil {
			return true
		}
		if time.Now().After(deadline) {
			return currentLoonServer.Load() == nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeOldSinspiredFiles 移除旧的 Sinspired_Rules_* 规则文件
func removeOldSinspiredFiles() {
	if saver, err := method.NewLocalSaver(); err == nil {
		oldFiles := []string{
			"Sinspired_Rules_CDN.yaml",
			"Sinspired_Rules_Lite_CDN.yaml",
		}
		for _, f := range oldFiles {
			path := filepath.Join(saver.OutputPath, f)
			if _, err := os.Stat(path); err == nil {
				_ = os.Remove(path)
			}
		}
	}
}
