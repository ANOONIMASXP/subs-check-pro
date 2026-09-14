// Package app 应用程序主入口，app\app.go
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/sinspired/subs-check-pro/v3/app/monitor"
	"github.com/sinspired/subs-check-pro/v3/check"
	"github.com/sinspired/subs-check-pro/v3/config"
	proxyutils "github.com/sinspired/subs-check-pro/v3/proxy"
	"github.com/sinspired/subs-check-pro/v3/save"
	"github.com/sinspired/subs-check-pro/v3/substore"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

// App 结构体用于管理应用程序状态
type App struct {
	ctx        context.Context
	cancel     context.CancelFunc
	configPath string
	watcher    *fsnotify.Watcher
	// watcherCancel 用于停止轮询配置监听 goroutine（inotify 不可用时的降级方案）。
	// 若 inotify 正常工作则此字段为 nil。
	watcherCancel context.CancelFunc
	checkChan     chan struct{} // 触发检测的通道
	checking      atomic.Bool   // 检测状态标志

	version    string
	httpServer *http.Server
	stopCh     <-chan struct{}

	lastCheck lastCheckResult
}

type lastCheckResult struct {
	time      atomic.Value // 存储 time.Time
	duration  atomic.Int64
	Total     atomic.Int64
	available atomic.Int64
	traffic   atomic.Int64
}

// New 创建新的应用实例
// 不再在这里调用 flag.Parse() 或定义 flags，全部由 main 负责。
func New(version string, configPath string) *App {
	ctx, cancel := context.WithCancel(context.Background())

	return &App{
		ctx:        ctx,
		cancel:     cancel,
		configPath: configPath,
		checkChan:  make(chan struct{}),
		version:    version,
	}
}

// InitConfigLoad 初始化配置文件加载
func (app *App) InitConfigLoad() error {
	// 初始化配置文件路径
	if err := app.initConfigPath(); err != nil {
		return fmt.Errorf("初始化配置文件路径失败: %w", err)
	}

	// 加载配置文件
	if err := app.loadConfig(); err != nil {
		return fmt.Errorf("加载配置文件失败: %w", err)
	}
	return nil
}

// Initialize 初始化应用程序
func (app *App) Initialize() error {
	if err := app.InitConfigLoad(); err != nil {
		return err
	}
	httpPortAvailable, subStorePortAvailable := app.CheckPortConflict()

	if config.GlobalConfig.ListenPort != "" && !httpPortAvailable {
		listenAddr := normalizeListenAddr(config.GlobalConfig.ListenPort)
		// 端口冲突属于致命错误，直接返回
		return fmt.Errorf("HTTP 端口 %s 已被占用，请修改配置中的 listen-port", listenAddr)
	}

	// 初始化配置文件监听
	if err := app.initConfigWatcher(); err != nil {
		return fmt.Errorf("初始化配置文件监听失败: %w", err)
	}

	if config.GlobalConfig.ListenPort != "" {
		if err := app.initHTTPServer(); err != nil {
			return fmt.Errorf("初始化HTTP服务器失败: %w", err)
		}
	}

	if config.GlobalConfig.SubStorePort != "" {
		subStoreAddr := normalizeListenAddr(config.GlobalConfig.SubStorePort)
		if !subStorePortAvailable {
			substore.IsSubStoreRunning.Store(false)
			slog.Warn("Sub-Store 端口已被其他进程占用，Sub-Store 服务未启动，请修改端口后重启",
				"addr", subStoreAddr)
		} else {
			// 使用 app.ctx 启动 sub-store，让其可被取消
			go substore.RunSubStoreService(app.ctx)
			// 短暂等待，保证 Sub-Store 启动日志按预期顺序输出
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		slog.Warn("Sub-Store 服务已禁用", "port", "未设置")
		substore.IsSubStoreRunning.Store(false)
	}

	// 启动内存监控
	monitor.StartMemoryMonitor()

	// 注册退出前清理逻辑（兜底）
	utils.BeforeExitHook = func() {
		// 检查内置 Sub-Store 服务是否仍在运行
		if substore.IsSubStoreRunning.Load() {
			slog.Warn("强制退出前，尝试关闭 Sub-Store 内置服务及释放端口")
			if err := substore.StopSubStore(); err != nil {
				slog.Error("强制停止 Sub-Store 服务失败", "err", err)
			} else {
				slog.Info("Sub-Store 服务已成功停止，端口已释放")
			}
		}
	}

	// 注册 ShutdownHook（第二次 Ctrl+C 立即调用）
	utils.ShutdownHook = func() {
		slog.Warn("立即退出程序")
		err := app.Shutdown()
		if err != nil {
			slog.Error("关闭应用失败", "err", err)
		} else {
			os.Exit(0)
		}
	}

	// 设置信号处理器
	app.stopCh = utils.SetupSignalHandler(&check.ForceClose, &app.checking)

	return nil
}

// Run 运行应用程序主循环
func (app *App) Run() {
	defer func() {
		if app.watcher != nil {
			_ = app.watcher.Close()
		}
		if app.watcherCancel != nil {
			app.watcherCancel()
		}
	}()

	// 启动时立即执行一次检测
	app.triggerCheck()

	// 并发处理 checkChan
	go func() {
		for range app.checkChan {
			go app.triggerCheck()
		}
	}()

	// 阻塞等待 stopCh 被关闭
	<-app.stopCh
	err := app.Shutdown()
	if err != nil {
		slog.Error("关闭应用失败", "err", err)
	}
}

// TriggerCheck 供外部调用的触发检测方法
func (app *App) TriggerCheck() {
	select {
	case app.checkChan <- struct{}{}:
		slog.Info("手动触发检测")
	default:
		slog.Warn("已有检测正在进行，忽略本次触发")
	}
}

// triggerCheck 内部检测方法
func (app *App) triggerCheck() {
	// 如果已经在检测中，直接返回
	if !app.checking.CompareAndSwap(false, true) {
		slog.Warn("已有检测正在进行，跳过本次检测")
		return
	}
	defer app.checking.Store(false)

	if err := app.checkProxies(); err != nil {
		check.CurrentStepName.Store("检测失败")
		slog.Error("检测代理失败", "error", err)
	}

	debug.FreeOSMemory()
	check.CurrentStepName.Store("检测完成")
}

// checkProxies 执行代理检测
func (app *App) checkProxies() error {
	if config.GlobalConfig.PrintProgress {
		slog.Info("启动检测任务", "终端进度条", "显示")
	} else {
		slog.Info("启动检测任务", "终端进度条", "隐藏")
	}

	startTime := time.Now()

	loadHistoricalCheckRate() // 注入历史速率后再开始检测

	check.StartProgress()
	defer check.StopProgress()

	results, err := check.Check()
	if err != nil {
		return fmt.Errorf("检测代理失败: %w", err)
	}

	slog.Info("检测完成")

	check.CurrentStepName.Store("保存配置")
	save.SaveConfig(results)

	check.CurrentStepName.Store("更新订阅")
	utils.UpdateSubs()

	if config.GlobalConfig.CallbackScript != "" {
		check.CurrentStepName.Store("执行回调脚本")
	}
	// 执行回调脚本
	utils.ExecuteCallback(len(results))

	endTime := time.Now()

	// 更新 lastCheck 结果（使用 Store 方法确保原子性）
	app.lastCheck.time.Store(endTime)
	app.lastCheck.duration.Store(int64(endTime.Sub(startTime).Seconds()))
	app.lastCheck.Total.Store(int64(check.ProxyCount.Load()))
	app.lastCheck.available.Store(int64(len(results)))
	app.lastCheck.traffic.Store(int64(check.TotalBytes.Load()))

	check.CurrentStepName.Store("内存释放")
	// 切断所有大对象的应用
	results = nil //nolint:ineffassign
	proxyutils.ClearCache()
	cleanupMihomo()

	// 等待底层 xhttp/tcp goroutine 退出释放缓冲区
	waitGoroutinesDrain(30 * time.Second)

	// 连续触发两次 GC，彻底清空 go-yaml 等库的 sync.Pool
	// 第一次 GC：将 sync.Pool 中的存活对象降级到 victim cache
	runtime.GC()
	// 第二次 GC：清空 victim cache，并强制将物理内存还给操作系统
	debug.FreeOSMemory()

	slog.Debug("当前 goroutine 数", "count", runtime.NumGoroutine())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	slog.Debug("内存状态",
		"HeapInuse", ms.HeapInuse/1024/1024,
		"HeapReleased", ms.HeapReleased/1024/1024,
		"Sys", ms.Sys/1024/1024)
	return nil
}

// waitGoroutinesDrain 等待残留 goroutine 退出
func waitGoroutinesDrain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	baseline := runtime.NumGoroutine()

	// 预期正常闲置时的 goroutine 数量大约在 30-50 之间
	for time.Now().Before(deadline) {
		current := runtime.NumGoroutine()
		if current <= baseline/2 || current < 50 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func cleanupMihomo() {
	// 清除 Hosts 映射
	if resolver.DefaultHostMapper != nil {
		resolver.DefaultHostMapper = nil
	}

	// 动清理 DNS 解析器的内部缓存和长连接，并断开引用
	if resolver.DefaultResolver != nil {
		// 释放内部的 LRU/Map 缓存，断开大量的内存引用
		resolver.DefaultResolver.ClearCache()
		resolver.DefaultResolver.ResetConnection()
		resolver.DefaultResolver = nil
	}

	resolver.ClearCache()
}

// Shutdown 尝试优雅关闭所有子服务与资源
func (app *App) Shutdown() error {
	slog.Debug("开始关闭应用...")

	var lastErr error

	// 停止轮询配置监听 goroutine（inotify 降级模式专用）
	if app.watcherCancel != nil {
		app.watcherCancel()
	}

	// 取消上下文，通知各子服务退出（sub-store 等）
	if app.cancel != nil {
		app.cancel()
	}

	// 停止 watcher（如果存在）
	if app.watcher != nil {
		lastErr = app.watcher.Close()
	}

	// 优雅关闭 HTTP 服务
	if app.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.httpServer.Shutdown(ctx); err != nil {
			lastErr = errors.New("关闭 HTTP 服务器失败: " + err.Error())
			slog.Error("关闭 HTTP 服务器失败", "err", err)
		} else {
			listenPort := strings.TrimPrefix(config.GlobalConfig.ListenPort, ":")
			slog.Info("HTTP 服务器关闭", "port", listenPort)
		}
	}

	// 等待短时间，给子 goroutine 清理时间（作为最小可行方案）
	time.Sleep(500 * time.Millisecond)

	slog.Info("应用已关闭")
	return lastErr
}
