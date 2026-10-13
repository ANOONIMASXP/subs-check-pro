// Package check 订阅检测主逻辑
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/juju/ratelimit"
	"github.com/metacubex/mihomo/adapter"
	mutils "github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/resolver"
	"github.com/metacubex/mihomo/constant"
	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/samber/lo"
	"github.com/sinspired/subs-check-pro/v3/assets"
	"github.com/sinspired/subs-check-pro/v3/check/platform"
	"github.com/sinspired/subs-check-pro/v3/config"
	proxyutils "github.com/sinspired/subs-check-pro/v3/proxy"
	"github.com/sinspired/subs-check-pro/v3/utils"
)

// 对外暴露变量
var (
	Progress   atomic.Uint32 // 已检测数量（语义见算法）
	Available  atomic.Uint32 // 已可用数量（测速阶段完成,可用即+1）
	ProxyCount atomic.Uint32 // 总数（动态=总节点；分阶段=当前阶段规模）

	TotalBytes     atomic.Uint64
	UP             atomic.Uint64
	DOWN           atomic.Uint64
	ForceClose     atomic.Bool
	Successlimited atomic.Bool
	ProcessResults atomic.Bool

	// Bucket 全局限速桶：run() 与结束清理跨 goroutine 写，countingConn 热路径读，
	// 必须原子访问（旧的裸指针是正式数据竞争）
	Bucket atomic.Pointer[ratelimit.Bucket]

	CheckStartTime    time.Time
	CheckEndTime      time.Time
	CheckDuration     time.Duration
	CheckTrafficTotal string
)

// 存储流媒体检测开关状态
var (
	mediaON        bool
	progressWeight ProgressWeight
)

// updateProxyName 热路径正则（每节点都跑，必须包级预编译；
// 旧实现在循环内 MustCompile，每个节点重编译两次）
var (
	// 剥离旧速度标签 `|x.xMB/s`
	reSpeedTagStrip = regexp.MustCompile(`\s*\|(?:\s*[\d.]+[KM]B/s)`)
	// 剥离旧媒体平台标签（NF/GPT/GM/...）
	reMediaTagStrip = regexp.MustCompile(
		`\s*\|(?:NF|D\+|GPT[⁺]?|CP[⁻]?|GM[⁺]?[ˀ]?(?:-[A-Z]{2})?|X|YT[⁻]?(?:-[A-Z]{2}[⁻]?)?|TK[⁻]?(?:-[^|]+)?|\d+%)`)
)

const MediaCheckMaxRetries = 3

// Result 存储节点检测结果
type Result struct {
	Proxy          map[string]any
	Openai         bool
	OpenaiWeb      bool
	Copilot        bool
	CopilotAPI     bool
	X              bool
	Youtube        string
	Netflix        bool
	Disney         bool
	Gemini         platform.GeminiStatus
	TikTok         string
	IP             string
	IPRisk         string
	Country        string
	CountryCodeTag string
	ISPTag         string

	// Delay 测活延迟(ms)，0=未知或测活失败
	Delay int
}

// ProxyChecker 处理代理检测的主要结构体
type ProxyChecker struct {
	results    []Result
	resultChan chan Result
	proxyCount int
	available  atomic.Int32

	aliveConcurrent int
	mediaConcurrent int

	aliveChan chan *ProxyJob
	mediaChan chan *ProxyJob

	pt *ProgressTracker

	// 用于回收失败但还有分数的历史节点、以及本次成功的节点
	historyChan chan map[string]any
	HistoryPool []map[string]any
	historyWg   sync.WaitGroup
}

// ProxyJob 在测活-流媒体检测任务间传输信息
type ProxyJob struct {
	Client *ProxyClient
	Result Result

	CfLoc string
	CfIP  string

	doneOnce sync.Once

	aliveMarked     atomic.Bool
	availableMarked atomic.Bool // 已计入“可用”计数
	mediaMarked     atomic.Bool

	NeedCF         bool
	IsCfAccessible bool

	GoogleCountry string // policies.google.com 预取的国家码（alpha-2），供 youtube/gemini 共享
}

// Close 确保 ProxyJob 的底层资源(mihomo客户端)被正确释放一次。
func (job *ProxyJob) Close() {
	job.doneOnce.Do(func() {
		if job.Client != nil {
			// 在关闭前读取该节点消耗的总流量
			traffic := job.Client.BytesRead.Load() + job.Client.BytesWritten.Load()
			if traffic > 0 && job.Result.Proxy != nil {
				if subURL, ok := job.Result.Proxy["sub_url"].(string); ok && subURL != "" {
					proxyutils.SubStatsMutex.Lock()
					st := proxyutils.SubStats[subURL]
					st.Traffic += traffic // 累加流量到订阅统计中
					proxyutils.SubStats[subURL] = st
					proxyutils.SubStatsMutex.Unlock()
				}
			}

			job.Client.Close()
			job.Client = nil // 切断对底层资源的引用
		}
		// 切断map引用，释放内存
		job.Result.Proxy = nil
		job.Result = Result{}
	})
}

// NewProxyChecker 创建新的检测器实例
func NewProxyChecker(proxyCount int) *ProxyChecker {
	cfg := config.Get()

	// 测活配置体检（每轮检测提示一次）
	if aliveCheckMode() == "urltest" && cfg.Timeout > 30000 {
		slog.Warn("alive-check=urltest 时单次探测超时会被 mihomo 内部 30s 截断", "timeout-ms", cfg.Timeout)
	}

	// 分别设置测活\媒体检测阶段并发数：
	// 显式值各自独立优先，未设置的项按 concurrent 倍率自动档回退
	aliveConc, mediaConc := StageConcurrent(cfg, proxyCount)

	// 自动档被上限截断时提示（仅 alive 侧超过 1000 的场景）
	if cfg.AliveConcurrent <= 0 && max(1, cfg.Concurrent)*autoAliveMultiplier > autoAliveCap {
		slog.Info("除非你的 CPU 和路由器同时允许, 超过 1000 并发可能影响其它上网程序,如确有需求,请在配置文件分别指定测活-媒体检测每个阶段并发数")
		slog.Info("已限制测活并发数", "并发", aliveConc)
	}

	return &ProxyChecker{
		proxyCount: proxyCount,

		// 设置不同检测阶段的并发数
		aliveConcurrent: aliveConc,
		mediaConcurrent: mediaConc,

		// 设置缓冲通道
		aliveChan: make(chan *ProxyJob, int(float64(aliveConc)*1.2)),
		mediaChan: make(chan *ProxyJob, mediaConc*2),

		// 设置进度跟踪
		pt: NewProgressTracker(proxyCount),

		historyChan: make(chan map[string]any, proxyCount), // 缓冲设为最大值防止阻塞
		HistoryPool: make([]map[string]any, 0),
	}
}

// Check 执行代理检测的主函数
func Check() ([]Result, []map[string]any, error) {
	proxyutils.ResetRenameCounter()
	ForceClose.Store(false)
	Successlimited.Store(false)
	ProcessResults.Store(false)

	ProxyCount.Store(0)
	Available.Store(0)
	Progress.Store(0)
	// 计数器必须随每次检测重置，否则进程内第二次检测的报告会累计历史值
	AliveCount.Store(0)

	TotalBytes.Store(0)

	// 重置预计剩余时间计算
	ResetETA()

	// 初始化流媒体检测开关
	mediaON = config.Get().MediaCheck

	// 标记订阅获取阶段开始
	CurrentStepName.Store("解析远程列表")
	// 获取订阅节点和之前成功的节点数量(已前置)
	proxies, rawCount, lastResultLength, historyResultLength, err := proxyutils.GetProxies(func(stepName string, done, total, available int) {
		CurrentStepName.Store(stepName)
		Progress.Store(uint32(done))
		ProxyCount.Store(uint32(total))
		Available.Store(uint32(available))
	})
	if err != nil {
		return nil, nil, fmt.Errorf("获取节点失败: %w", err)
	}
	slog.Info("已获取节点", "数量", rawCount)
	slog.Info("去重后节点", "数量", len(proxies))

	proxyutils.ClearCache()

	debug.FreeOSMemory()

	Available.Store(0)
	Progress.Store(0)
	ProxyCount.Store(0)

	if lastResultLength > 0 {
		slog.Info("已加载上次检测可用节点", "数量", lastResultLength)
	}

	if historyResultLength > 0 {
		slog.Info("已加载历次检测可用节点", "数量", historyResultLength)
	}

	CurrentStepName.Store("节点乱序")

	// 假设有 15 个相似的 IP/域名
	calcMinSpacing := max(config.Get().Concurrent*5, len(proxies)/15)

	// 随机乱序并根据 IP/域名/真实Host 打乱节点顺序, 减少测速直接测死的概率
	cfg := proxyutils.ShuffleConfig{
		Threshold:  float64(config.Get().Threshold),
		Passes:     3,
		MinSpacing: calcMinSpacing,
		ScanLimit:  config.Get().Concurrent * 2,
	}

	// 获取动态映射的 CIDR 和 域名层级 文本
	cidr, domainLevel := proxyutils.ThresholdToLevel(cfg.Threshold)

	// 设置之前成功的节点顺序在前
	headSize := lastResultLength

	if len(proxies) > headSize && headSize > 0 {
		tail := proxies[headSize:]
		proxyutils.SmartShuffleByServer(tail, cfg)
		// 动态输出到终端日志
		slog.Info(fmt.Sprintf("节点乱序, 相同 CIDR %s 或 %s 最小间距: %d", cidr, domainLevel, cfg.MinSpacing))
		slog.Info("上次成功节点未乱序", "count", headSize)
	} else {
		proxyutils.SmartShuffleByServer(proxies, cfg)
		// 动态输出到终端日志
		slog.Info(fmt.Sprintf("节点乱序, 相同 CIDR %s 或 %s 最小间距: %d", cidr, domainLevel, cfg.MinSpacing))
	}

	CurrentStepName.Store("获取订阅完成")
	if len(proxies) == 0 {
		slog.Info("没有需要检测的节点")
		return nil, nil, nil
	}

	// 标记订阅获取阶段结束
	checker := NewProxyChecker(len(proxies))

	results, historyPool, err := checker.run(proxies)
	checker = nil //nolint:ineffassign
	return results, historyPool, err
}

// Run 运行检测流程
func (pc *ProxyChecker) run(proxies []map[string]any) ([]Result, []map[string]any, error) {
	CurrentStepName.Store("初始化检测")
	// 限速设置
	limit := config.Get().TotalSpeedLimit
	if limit <= 0 {
		limit = 100 // 默认最大 100MB/s
	}
	rate := float64(limit) * 1024 * 1024
	capacity := int64(rate / 10)
	Bucket.Store(ratelimit.NewBucketWithRate(rate, capacity))

	// // 初始化全局上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 如果 MaxMindDBPath 为空会自动使用 subs-check-pro 内置数据库
	geoDB, err := assets.OpenMaxMindDB(config.Get().MaxMindDBPath)
	if err != nil {
		slog.Debug(fmt.Sprintf("打开 MaxMind 数据库失败: %v", err))
		geoDB = nil
	}

	// 确保数据库在函数结束时关闭
	if geoDB != nil {
		defer func() {
			if err := geoDB.Close(); err != nil {
				slog.Debug(fmt.Sprintf("关闭 MaxMind 数据库失败: %v", err))
			}
		}()
	}

	slog.Info("开始检测节点")

	// 记录开始检测时间
	CheckStartTime = time.Now()

	// 组装参数
	cfg := config.Get()
	args := []any{
		"media-check", mediaON,
		"drop-bad-cf-nodes", cfg.DropBadCfNodes,
	}

	// 流水线并发参数（auto-alive/auto-media 只在该项走了自动档时输出）
	args = append(args, "concurrent", cfg.Concurrent, ":alive", pc.aliveConcurrent)
	if cfg.AliveConcurrent <= 0 {
		args = append(args, "auto-alive", true)
	}
	if mediaON {
		args = append(args, ":media", pc.mediaConcurrent)
		if cfg.MediaConcurrent <= 0 {
			args = append(args, "auto-media", true)
		}
	}
	// 只有在 >0 时才输出
	if config.Get().SuccessLimit > 0 {
		args = append(args, "success-limit", config.Get().SuccessLimit)
	}
	// total-speed-limit 是全局连接层限速（测活/媒体等所有代理连接读流量）
	if config.Get().TotalSpeedLimit > 0 {
		args = append(args, "total-speed-limit", config.Get().TotalSpeedLimit)
	}

	// 再追加剩余参数
	args = append(args,
		"timeout", config.Get().Timeout,
	)

	if config.Get().UseLastResult {
		args = append(args, "use-last-result", true)
	}

	if config.Get().UseHistoryResult {
		args = append(args, "use-history-result", true)
	}

	args = append(args, "analysis", "true")

	// 添加 isp 检测参数
	cfgISP := config.Get()

	hasISPAPIKey := cfgISP.ISPCheckAPIKeyIPAPI != "" || cfgISP.ISPCheckAPIKeyIPData != "" || cfgISP.ISPCheckAPIKeyIPLocate != "" || cfgISP.ISPCheckAPIKeyProxyCheck != ""

	isISPCheck := cfgISP.ISPCheck && hasISPAPIKey

	args = append(args, "isp-check", isISPCheck)

	if config.Get().SuccessRate > 0 {
		r := fmt.Sprintf("%.1f%%", config.Get().SuccessRate*100)
		args = append(args, "success-rate", r)
	}

	// 最终日志调用
	slog.Info("当前参数", args...)

	// 监测 ForceClose
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ForceClose.Load() {
					slog.Warn("用户手动结束检测,等待收集结果")
					cancel()
					return
				}
			}
		}
	}()

	// // 进度显示 —— 使用关闭信号并等待 showProgress 完成
	// doneCh := make(chan struct{})
	// finishedCh := make(chan struct{})

	// if config.Get().PrintProgress {
	// 	go func() {
	// 		pc.showProgress(doneCh)
	// 		close(finishedCh)
	// 	}()
	// }

	// 计算预计剩余时间
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				UpdateETA()
				return
			case <-ticker.C:
				SnapshotRate()
				UpdateETA()
			}
		}
	}()

	// 启动流水线任务前设置 mihomo 变量
	resolver.DisableIPv6 = !config.Get().EnableIPv6

	CurrentStepName.Store("进度")

	// 启动本机网络健康探针：独立于代理，直连探测本机网络是否被检测流量拖垮。
	// 使用独立 ctx，不随「达到成功数量限制」的 cancel 提前停止，检测收尾后统一关闭。
	probeCtx, probeCancel := context.WithCancel(context.Background())
	defer probeCancel()
	netGuardOn := config.Get().NetGuard
	// 三个阶段各一个闸门，容量即各自配置的自然并发上限（网络健康时不产生任何额外限制）：
	//   测活 = 测活并发；
	//   媒体 = 媒体并发 × 平台数（每个节点会同时跑多个平台检测，闸门按「单平台任务」计）。
	// 复核预算 = 总节点数的 5%（至少 20），避免死节点被整体复测拖慢检测。
	// 由配置项 net-guard 控制，关闭后不启动探针，Acquire/Release/WaitSettled 均为空操作。
	if netGuardOn {
		caps := utils.GateCaps{Alive: pc.aliveConcurrent}
		if mediaON {
			caps.Media = pc.mediaConcurrent * len(config.Get().Platforms)
		}
		utils.NetGuard.Start(probeCtx, caps, max(20, pc.proxyCount/20))
	}

	pc.historyWg.Add(1)
	go func() {
		defer pc.historyWg.Done()
		for node := range pc.historyChan {
			pc.HistoryPool = append(pc.HistoryPool, node)
		}
	}()

	// 启动流水线阶段
	go pc.distributeJobs(proxies, ctx)
	go pc.runAliveStage(ctx, geoDB)
	pc.runMediaStageAndCollect(geoDB, ctx, cancel)

	// 流水线已结束，停止探针，并输出保护机制是否介入的摘要，便于定位「变慢」是否与保护有关
	probeCancel()
	if netGuardOn {
		if text, intervened := utils.NetGuard.Summary(); intervened {
			slog.Info(text)
		}
	}

	// 关闭历史节点收集并按分数排序
	close(pc.historyChan)
	pc.historyWg.Wait()

	sort.Slice(pc.HistoryPool, func(i, j int) bool {
		scoreI, _ := pc.HistoryPool[i]["history_score"].(int)
		scoreJ, _ := pc.HistoryPool[j]["history_score"].(int)
		return scoreI > scoreJ // 降序，最高分的排在前面
	})

	CurrentStepName.Store("处理结果")

	// 确保进度显示到 100%
	pc.pt.Finalize()

	if config.Get().SuccessLimit > 0 && pc.available.Load() >= config.Get().SuccessLimit {
		slog.Info(fmt.Sprintf("达到成功节点数量限制 %d, 收集结果完成。", config.Get().SuccessLimit))
	}

	// 标记检测完成，开始处理结果，保存，上传等
	ProcessResults.Store(true)

	// 重置预计剩余时间计算
	ETASeconds.Store(0)

	// 计算本次检测所有订阅文件的体积
	var totalSubSize uint64
	for _, stat := range proxyutils.SubStats {
		totalSubSize += uint64(stat.Size)
	}

	totalSubSizeStr := utils.FormatTraffic(totalSubSize)

	slog.Info(fmt.Sprintf("可用节点数量: %d", len(pc.results)))

	totalBytes := TotalBytes.Load()

	CheckTrafficTotal = utils.FormatTraffic(totalBytes)

	// 总流量 = 订阅文件大小 + 检测流量
	totalTraffic := totalSubSize + totalBytes
	totalTrafficStr := utils.FormatTraffic(totalTraffic)

	slog.Info(fmt.Sprintf("检测消耗流量: %s", CheckTrafficTotal))

	slog.Debug("流量", "UP", UP.Load(), "DOWN", DOWN.Load())

	// 计算检测用时
	CheckEndTime = time.Now()
	CheckDuration = time.Since(CheckStartTime)

	CurrentStepName.Store("生成检测报告")
	// （此处原有无条件 time.Sleep(10s)：已核实报告输入全部同步落定——
	// runMediaStageAndCollect 已收完结果、SubStats 在取数阶段写完、
	// pt.Finalize 已调用，报告不读任何异步状态——纯属每次检测白烧 10 秒）

	// 仅当本次检测中出现过拥塞，才等本机网络（NAT 连接表 / DNS / 队列）恢复后再保存、上传、通知，
	// 否则紧随其后的请求容易撞上残余拥塞而超时。没出现过拥塞时立即返回，最多等 15s。
	utils.NetGuard.WaitSettled(context.Background(), 15*time.Second)

	// 1. 深度分析 (利用上一步的成功率进行排序，生成 analysis yaml)
	pc.GenerateAnalysisReport()

	slog.Info("消耗流量统计", "总计", totalTrafficStr, "订阅", totalSubSizeStr, "检测", CheckTrafficTotal)

	// 2. 清理元数据 (删除 sub_url 等字段，防止污染最终配置)
	pc.CleanupMetadata()

	// 手动解除引用
	for i := range proxies {
		proxies[i] = nil
	}

	Bucket.Store(nil)

	CurrentStepName.Store("释放检测内存")

	// 在保存上传之前直接归还内存
	debug.FreeOSMemory()

	return pc.results, pc.HistoryPool, nil
}

// distributeJobs 分发代理任务
func (pc *ProxyChecker) distributeJobs(proxies []map[string]any, ctx context.Context) {
	defer close(pc.aliveChan)

	concurrency := min(pc.proxyCount, pc.aliveConcurrent)
	var wg sync.WaitGroup

	// 使用原子索引来分发任务
	var proxyIndex atomic.Int64
	proxyIndex.Store(-1) // 初始化为 -1

	// 定义主动 GC 的阈值：0 = 默认 20000；非零值下限 1000——
	// 用户误配成 1 时每个节点都会触发一次 STW FreeOSMemory，等同卡死
	var gcThreshold int64
	switch v := config.Get().GCThreshold; {
	case v == 0:
		gcThreshold = 20000
	case v < 1000:
		gcThreshold = 1000
	default:
		gcThreshold = v
	}

	// 启动工作协程池
	for range concurrency {
		wg.Go(func() {
			for {
				// 原子地获取下一个代理索引
				index := proxyIndex.Add(1)
				if index >= int64(len(proxies)) {
					return // 所有代理都已处理完毕
				}

				if checkCtxDone(ctx) {
					// 收到终止信号，将当前队列未处理的历史节点无伤抢救下来
					pc.scoreAndRecycle(proxies[index], "untested", 0, nil)
					proxies[index] = nil
					continue // 这里用 continue 继续消耗数组完成抢救
				}

				mapping := proxies[index]

				// 任务取出后，立即断开源切片的引用
				// 此时，如果 mapping 没被后续 CreateClient 引用，它就是垃圾；
				// 如果 mapping 被传给了 Client，等 Job.Close() 时它也会变成垃圾。
				proxies[index] = nil

				// 周期性强制归还内存
				// 只有当索引达到阈值倍数时触发
				if index > 0 && index%gcThreshold == 0 {
					// 同步执行：FreeOSMemory 是 STW 重活，起 goroutine 并发跑
					// 只会在 400+ worker 压力上叠加重复暂停，且 goroutine 会
					// 活过触发它的这一轮迭代
					slog.Debug("已处理 " + strconv.FormatInt(index, 10) + " 个节点，正在执行主动内存回收...")
					debug.FreeOSMemory()
				}

				cli := CreateClient(mapping)
				if cli == nil {
					// 创建失败：视为 alive 完成（失败），不进入 speed/media
					pc.pt.CountAlive(false)
					// 底层生成故障视为死节点
					pc.scoreAndRecycle(mapping, "alive_fail", 0, nil)
					continue
				}

				job := &ProxyJob{
					Client: cli,
					Result: Result{Proxy: mapping},
				}
				job.NeedCF = config.Get().DropBadCfNodes ||
					(config.Get().MediaCheck && needsCF(config.Get().Platforms))

				// 当 aliveChan 满时会阻塞
				select {
				case pc.aliveChan <- job:
				case <-ctx.Done():
					job.Close()
					return
				}
			}
		})
	}

	// 等待所有工作协程完成
	wg.Wait()
	// 分发结束，再次强制清理（此时 proxies 切片虽然 length 很大，但全是 nil）
	debug.FreeOSMemory()
}

// 测活
func (pc *ProxyChecker) runAliveStage(ctx context.Context, db *maxminddb.Reader) {
	// alive 结束即闭合媒体阶段通道（唯一后继阶段）
	defer close(pc.mediaChan)

	var wg sync.WaitGroup
	concurrency := pc.aliveConcurrent
	pc.pt.currentStage.Store(0)

	for range concurrency {
		wg.Go(func() {
			for job := range pc.aliveChan {
				if checkCtxDone(ctx) {
					if job.aliveMarked.CompareAndSwap(false, true) {
						pc.pt.CountAlive(false)
					}
					pc.scoreAndRecycle(job.Result.Proxy, "untested", 0, nil) // 抢救
					job.Close()
					continue
				}
				// 自适应闸门（net-guard 关闭或网络健康时几乎零开销）：
				// 仅当本机网络真的连不上外网时才收紧同时在途的测活数（最低保留 1/2），恢复后自动加回。
				if !utils.NetGuard.Acquire(ctx, utils.GateAlive) {
					if job.aliveMarked.CompareAndSwap(false, true) {
						pc.pt.CountAlive(false)
					}
					pc.scoreAndRecycle(job.Result.Proxy, "untested", 0, nil) // 抢救
					job.Close()
					continue
				}

				// 节点测活
				isAlive, delay := checkAlive(job, ctx)
				job.Result.Delay = delay

				// 拥塞期间失败：用有限的预算复核一次，避免把「本来存活」的节点误判为死亡。
				// 不等待网络恢复，只做一次短暂抖动后重测，且预算用尽后不再复核。
				if !isAlive && !checkCtxDone(ctx) && utils.NetGuard.TakeRecheck() {
					slog.Debug("拥塞期间测活失败，复核一次", "Name", job.Client.mProxy.Name())
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(300+rand.IntN(500)) * time.Millisecond):
					}
					if !checkCtxDone(ctx) {
						isAlive, _ = checkAlive(job, ctx)
					}
				}
				utils.NetGuard.Release(utils.GateAlive)

				if !isAlive {
					// 记录非存活
					if job.aliveMarked.CompareAndSwap(false, true) {
						pc.pt.CountAlive(false)
					}
					pc.scoreAndRecycle(job.Result.Proxy, "alive_fail", 0, nil) // 失败打分扣除
					job.Close()
					continue // 不进入媒体检测
				}

				// CF 过滤
				if job.NeedCF {
					job.IsCfAccessible, job.CfLoc, job.CfIP = platform.CheckCloudflare(ctx, job.Client.Client)
					if config.Get().DropBadCfNodes && !job.IsCfAccessible {
						// 记录丢弃
						if job.aliveMarked.CompareAndSwap(false, true) {
							pc.pt.CountAlive(false)
						}
						pc.scoreAndRecycle(job.Result.Proxy, "alive_fail", 0, nil) // 视为故障
						job.Close()
						continue
					}
				}

				// 地区过滤
				if !job.checkJobLocation(db, ctx) {
					pc.scoreAndRecycle(job.Result.Proxy, "untested", 0, nil) // 未通过地区过滤，算作无伤丢弃，保留原分数
					job.Close()
					// 必须记为 alive 完成：漏记会让 aliveDone 永远到不了 totalJobs，
					// 进度条卡 100% 以下、ETA 不收敛、报告 AliveCount 少计
					if job.aliveMarked.CompareAndSwap(false, true) {
						pc.pt.CountAlive(false)
					}
					continue
				}

				// 通过全部过滤，记录为存活
				if job.aliveMarked.CompareAndSwap(false, true) {
					pc.pt.CountAlive(true)
				}

				// 流转：通过 alive 即视为“可用”，保证 Available 与最终可用数量一致
				if job.availableMarked.CompareAndSwap(false, true) {
					pc.incrementAvailable()
				}
				select {
				case pc.mediaChan <- job:
				case <-ctx.Done():
					job.Close()
				}
			}
		})
	}
	wg.Wait()

	// alive 阶段闭合：分阶段算法据此确定下一阶段规模
	pc.pt.FinishAliveStage()
}

// 流媒体检测 + 收集结果
func (pc *ProxyChecker) runMediaStageAndCollect(db *maxminddb.Reader, ctx context.Context, cancel context.CancelFunc) {
	var wg sync.WaitGroup
	resultLength := pc.mediaConcurrent
	if config.Get().SuccessLimit != 0 {
		resultLength = int(config.Get().SuccessLimit)
	}

	pc.resultChan = make(chan Result, resultLength)

	// 设置成功数量限制
	var stopOnce sync.Once

	// 收集结果
	var collectorWg sync.WaitGroup
	collectorWg.Go(func() {
		pc.collectResults()
	})

	// 启动 workers（确保 collector 已启动以避免阻塞在无缓冲时）
	concurrency := pc.mediaConcurrent
	for range concurrency {
		wg.Go(func() {
			for job := range pc.mediaChan {
				// 接受停止信号：达到成功上限后丢弃多余结果
				if checkCtxDone(ctx) {
					if job.mediaMarked.CompareAndSwap(false, true) {
						pc.pt.CountMedia()
					}
					job.Close()
					continue
				}

				// 设置成功数量限制
				if config.Get().SuccessLimit > 0 && pc.available.Load() >= config.Get().SuccessLimit {
					stopOnce.Do(func() {
						Successlimited.Store(true)
						pc.pt.FinishAliveStage()
						if mediaON {
							slog.Warn(fmt.Sprintf("达到成功节点数量限制 %d, 等待媒体检测任务完成...", config.Get().SuccessLimit))
						} else {
							slog.Warn(fmt.Sprintf("达到成功节点数量限制 %d, 等待节点重命名任务完成...", config.Get().SuccessLimit))
						}
						slog.Warn("测活模式将丢弃多余结果")

						cancel()
					})
				}

				if mediaON && !checkCtxDone(ctx) {
					mediaCheck(job, db, ctx)
				}

				pc.updateProxyName(&job.Result, job.Client, db, job.CfLoc, job.CfIP, ctx)

				// 成功节点打分并入池
				pc.scoreAndRecycle(job.Result.Proxy, "success", 0, &job.Result)

				// 将结果发送到 collector
				pc.resultChan <- job.Result

				if job.mediaMarked.CompareAndSwap(false, true) {
					pc.pt.CountMedia()
				}

				job.Close()
			}
		})
	}

	// 等待所有 worker 完成，再关闭 pc.resultChan，让 collector 退出
	wg.Wait()
	close(pc.resultChan)
	collectorWg.Wait()

	// 最后一次刷新并返回收集结果
	pc.pt.refresh()
	ProcessResults.Store(true)
}

// collectResults 收集检测结果
func (pc *ProxyChecker) collectResults() {
	for result := range pc.resultChan {
		pc.results = append(pc.results, result)
	}
}

// aliveProbe 测活探测函数：返回 (是否通过, 延迟ms, 错误)。
// 拆成独立类型便于单测注入假探测器。
type aliveProbe func(ctx context.Context) (bool, int, error)

// expectedAliveStatus 预解析测活期望状态码（固定 204）。
// mihomo 的 IntRanges 空集等价于“放行任何状态码”，所以解析失败必须显式构造 [204,204]，
// 绝不能落成 nil，否则 500/302 也会被判活。
var expectedAliveStatus = sync.OnceValue(func() mutils.IntRanges[uint16] {
	ranges, err := mutils.NewUnsignedRanges[uint16]("204")
	if err == nil && len(ranges) > 0 {
		return ranges
	}
	slog.Warn("测活状态码 204 解析失败，使用兜底区间", "error", err)
	return mutils.IntRanges[uint16]{mutils.NewRange[uint16](204, 204)}
})

// aliveCheckWarnOnce 未知 alive-check 取值只告警一次
var aliveCheckWarnOnce sync.Once

// aliveCheckMode 归一化 alive-check 配置，未知值按默认 urltest 处理。
func aliveCheckMode() string {
	switch mode := strings.ToLower(strings.TrimSpace(config.Get().AliveCheck)); mode {
	case "":
		return "urltest"
	case "urltest", "http":
		return mode
	default:
		aliveCheckWarnOnce.Do(func() {
			slog.Warn("未知的 alive-check 配置，按默认 urltest 处理", "alive-check", config.Get().AliveCheck)
		})
		return "urltest"
	}
}

// effectiveTimeout 生效的测活/客户端超时：配置 <=0（缺省或显式 0）一律回落到
// 默认 6000ms，避免探测完全没有截止时间、单个挂死节点耗尽拨号超时才返回。
func effectiveTimeout() time.Duration {
	ms := config.Get().Timeout
	if ms <= 0 {
		ms = 6000
	}
	return time.Duration(ms) * time.Millisecond
}

// aliveProbeTimeout 单次探测的 ctx 超时。
// http 模式与历史一致直接用 config.Timeout（由 http.Client 生效）；
// urltest 模式因 mihomo 内部 http.Client 硬编码 30s，必须截断到 30s 以内。
func aliveProbeTimeout(mode string) time.Duration {
	timeout := effectiveTimeout()
	if mode == "urltest" {
		return min(timeout, 30*time.Second)
	}
	return timeout
}

// withClientCancel 合并阶段 ctx 与 ProxyClient ctx（job.Close() 可中断探测），并按需加超时。
func withClientCancel(stageCtx, clientCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(stageCtx, timeout)
	} else {
		ctx, cancel = context.WithCancel(stageCtx)
	}
	if clientCtx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(clientCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// urlTestProbe mihomo 同源探测：Proxy.URLTest 与 smart/url-test 输入同源，
// 返回毫秒延迟并写入该节点的 DelayHistory / Alive 状态。
func urlTestProbe(job *ProxyJob, url string, timeout time.Duration) aliveProbe {
	expected := expectedAliveStatus()
	return func(ctx context.Context) (bool, int, error) {
		probeCtx, cancel := withClientCancel(ctx, job.Client.ctx, timeout)
		defer cancel()

		delay, err := job.Client.mProxy.URLTest(probeCtx, url, expected)
		// 状态码不符时 URLTest 返回 err==nil，必须以 AliveForTestUrl 的最终判定为准
		ok := err == nil && job.Client.mProxy.AliveForTestUrl(url)
		return ok, int(delay), err
	}
}

// httpProbe 旧版口径：HEAD + UA/Accept 头 + 跟随重定向，走 job 自己的 http.Client，顺带计时。
func httpProbe(job *ProxyJob, url string, timeout time.Duration) aliveProbe {
	return func(ctx context.Context) (bool, int, error) {
		probeCtx, cancel := withClientCancel(ctx, job.Client.ctx, timeout)
		defer cancel()
		return platform.CheckGstaticURL(job.Client.Client, probeCtx, url)
	}
}

// isRetryableAlive 测活专用的可重试判定：只允许“快速失败”（连接被重置 / 意外 EOF），
// 明确剔除超时——超时复测会让死节点耗时翻倍（timeout 默认 6000ms）。
// 注意不能直接委托给 isRetryable：后者遇到 net.Error（含 syscall.Errno）会直接返回
// netErr.Timeout()，ECONNRESET 分支实际是死代码。
func isRetryableAlive(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF)
}

// runAliveProbe 执行测活探测：按 alive-retry 复测，退避 i²*200ms。
// 通过 / 状态码不符(err==nil 未通过) / 不可重试错误 → 立即定案；ctx 取消立即中断。
func runAliveProbe(ctx context.Context, probe aliveProbe, retries int) (bool, int, error) {
	if retries < 0 {
		retries = 0
	}
	var (
		ok    bool
		delay int
		err   error
	)
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt*attempt) * 200 * time.Millisecond
			select {
			case <-ctx.Done():
				return false, 0, ctx.Err()
			case <-time.After(wait):
			}
		}
		ok, delay, err = probe(ctx)
		if ok || err == nil || !isRetryableAlive(err) {
			return ok, delay, err
		}
	}
	return false, delay, err
}

// crossCheckAliveDebug 仅在 debug 日志开启时用另一模式复核一次并输出差异（A/B 归因用）。
// 正常运行零开销：debug 未开启时直接返回，不产生额外请求。
func crossCheckAliveDebug(ctx context.Context, mode, url string, job *ProxyJob, ok bool, delay int) {
	if !slog.Default().Handler().Enabled(ctx, slog.LevelDebug) {
		return
	}
	otherMode := "http"
	if mode == "http" {
		otherMode = "urltest"
	}
	// 复核同样必须有上界，避免 config.Timeout=0 时挂住
	timeout := aliveProbeTimeout(otherMode)
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	var other aliveProbe
	if otherMode == "urltest" {
		other = urlTestProbe(job, url, timeout)
	} else {
		other = httpProbe(job, url, timeout)
	}
	otherOK, otherDelay, otherErr := other(ctx)
	if otherOK == ok {
		return
	}
	slog.Debug("测活双模式判定不一致",
		"name", job.Client.mProxy.Name(),
		"mode", mode, "ok", ok, "delay", delay,
		"other-mode", otherMode, "other-ok", otherOK, "other-delay", otherDelay, "other-error", otherErr,
	)
}

// checkAlive 测活：按 alive-check 选择探测方式并执行复测。
// 返回 (是否通过, 延迟ms；失败时延迟为最后一次探测的耗时)。
// 延迟只用于日志与统计，不参与判定（延迟门限交给 timeout 传输超时）。
func checkAlive(job *ProxyJob, ctx context.Context) (bool, int) {
	if job.Client == nil || job.Client.mProxy == nil {
		slog.Debug("测活跳过：客户端已释放")
		return false, 0
	}

	aliveURL := strings.TrimSpace(config.Get().AliveURL)
	if aliveURL == "" {
		aliveURL = platform.DefaultAliveURL
	}

	mode := aliveCheckMode()
	var probe aliveProbe
	if mode == "http" {
		probe = httpProbe(job, aliveURL, aliveProbeTimeout(mode))
	} else {
		probe = urlTestProbe(job, aliveURL, aliveProbeTimeout(mode))
	}

	ok, delay, err := runAliveProbe(ctx, probe, config.Get().AliveRetry)
	// 归因用：debug 模式下用另一模式复核并输出差异（正常运行零开销）
	crossCheckAliveDebug(ctx, mode, aliveURL, job, ok, delay)

	if !ok {
		if err == nil {
			slog.Debug("测活失败：状态码不符", "mode", mode, "name", job.Client.mProxy.Name(), "delay", delay)
		} else {
			slog.Debug("测活出错", "mode", mode, "Name", job.Client.mProxy.Name(), "error", err)
		}
		return false, delay
	}

	return true, delay
}

// needsCF 判断所选的媒体检测平台是否需要Cloudflare访问权限。
func needsCF(platforms []string) bool {
	for _, p := range platforms {
		if p == "openai" || p == "x" {
			return true
		}
	}
	return false
}

// mediaCheck 并发检测所有媒体解锁平台
func mediaCheck(job *ProxyJob, db *maxminddb.Reader, ctx context.Context) {
	mediaTimeout := config.Get().MediaCheckTimeout
	if mediaTimeout <= 0 {
		mediaTimeout = 10
	}

	mediaClient := &http.Client{
		Transport: job.Client.Transport,
		Timeout:   time.Duration(mediaTimeout) * time.Second,
	}

	plats := config.Get().Platforms

	// 若同时检测 youtube 或 gemini，提前获取 Google 国家码供两者共享，避免重复请求
	needGoogleCountry := false
	for _, p := range plats {
		if p == "youtube" || p == "gemini" {
			needGoogleCountry = true
			break
		}
	}
	if needGoogleCountry {
		if country, err := platform.GetGoogleCountry(ctx, mediaClient); err == nil && country != "" {
			job.GoogleCountry = country
		}
	}

	var wg sync.WaitGroup
	for _, plat := range plats {
		wg.Go(func() {
			// 媒体闸门：按「单平台任务」限制全局并发。这里刻意用 Background 而不是 ctx：
			// 达到成功数量上限后 ctx 会被取消，但已进入媒体阶段的节点仍要完整检测完，不能被闸门跳过。
			utils.NetGuard.Acquire(context.Background(), utils.GateMedia)
			defer utils.NetGuard.Release(utils.GateMedia)
			checkOnePlatform(job, plat, mediaClient, db, ctx)
		})
	}
	wg.Wait()
}

// mediaCheck 根据平台类型分发到相应的检测函数。
func checkOnePlatform(job *ProxyJob, plat string, mediaClient *http.Client, db *maxminddb.Reader, ctx context.Context) {
	switch plat {
	case "x":
		if job.NeedCF && !job.IsCfAccessible {
			break
		}
		job.Result.X = true
	case "openai":
		if job.NeedCF && !job.IsCfAccessible {
			break
		}
		var cookiesOK, clientOK bool
		err := withRetry(ctx, func() error {
			var e error
			cookiesOK, clientOK, e = platform.CheckOpenAI(ctx, mediaClient)
			return e
		})
		if err != nil {
			break
		}
		if clientOK && cookiesOK {
			job.Result.Openai = true
		} else if clientOK || cookiesOK {
			job.Result.OpenaiWeb = true
		}
	case "copilot":
		if job.NeedCF && !job.IsCfAccessible {
			break
		}
		var homeOK, apiOK bool
		err := withRetry(ctx, func() error {
			var e error
			homeOK, apiOK, e = platform.CheckCopilot(ctx, mediaClient)
			return e
		})
		if err != nil {
			break
		}
		job.Result.Copilot, job.Result.CopilotAPI = homeOK, apiOK
	case "netflix":
		var ok bool
		withRetry(ctx, func() error { //nolint:errcheck
			var e error
			ok, e = platform.CheckNetflix(ctx, mediaClient)
			return e
		})
		if ok {
			job.Result.Netflix = true
		}
	case "disney":
		var ok bool
		withRetry(ctx, func() error { //nolint:errcheck
			var e error
			ok, e = platform.CheckDisney(ctx, mediaClient)
			return e
		})
		if ok {
			job.Result.Disney = true
		}
	case "youtube":
		var ytRaw string
		withRetry(ctx, func() error { //nolint:errcheck
			var e error
			ytRaw, e = platform.CheckYoutube(ctx, mediaClient)
			return e
		})

		// 分离地区码和 Premium 标记
		// ytRaw 可能是: "US" / "US⁻" / "⁻" / "CN" / ""
		ytRegion := strings.TrimSuffix(ytRaw, "⁻")
		ytNoPremium := strings.HasSuffix(ytRaw, "⁻")

		// 地区兜底：无论是否单独检测，逻辑统一
		switch {
		case ytRegion == "CN":
			// CN 封锁，不设置结果
		case ytRegion != "" && job.GoogleCountry != "" && ytRegion != job.GoogleCountry:
			// 两者均有值但不一致，以 YouTube 自身检测为准
			slog.Debug("YouTube地区与Google策略页不一致，以Google策略页为准",
				"youtube", ytRegion, "google_policy", job.GoogleCountry)
			job.Result.Youtube = job.GoogleCountry
		case ytRegion != "":
			job.Result.Youtube = ytRegion
		case job.GoogleCountry != "" && ytRaw != "CN":
			// YouTube 检测失败，降级使用 GoogleCountry
			slog.Debug("YouTube检测失败，降级使用GoogleCountry", "country", job.GoogleCountry)
			job.Result.Youtube = job.GoogleCountry
		}

		// Premium 标记附加在最终地区码上
		if job.Result.Youtube != "" && ytNoPremium {
			job.Result.Youtube += "⁻"
		}

	case "gemini":
		// 主路径：含特征码，可识别 Normal/Blocked/Suspect 三种状态
		var status platform.GeminiStatus
		err := withRetry(ctx, func() error {
			var e error
			status, e = platform.CheckGemini(ctx, mediaClient)
			return e
		})

		switch {
		case err == nil && status.Region != "":
			// 完全成功
			job.Result.Gemini = status
		case errors.Is(err, platform.ErrGeminiBotDetected):
			// bot 拦截：不代表地区封锁，降级判断
			// 只能区分 Normal/Blocked，无法识别 Suspect
			if job.GoogleCountry != "" {
				job.Result.Gemini = platform.CheckGeminiByCountry(job.GoogleCountry)
				slog.Debug("Gemini被bot检测拦截，降级判断", "country", job.GoogleCountry)
			}
		case err != nil:
			// 真正的网络错误，不可达，不设置结果
			slog.Debug("Gemini不可达", "error", err)
		default:
			// err==nil 但 Region 为空：页面结构变化，无法解析
			slog.Debug("Gemini响应正常但未解析到地区")
		}
	case "tiktok":
		var region string
		withRetry(ctx, func() error { //nolint:errcheck
			var e error
			region, e = platform.CheckTikTok(ctx, mediaClient)
			return e
		})
		if region != "" {
			job.Result.TikTok = region
		}
	case "iprisk":
		// 如果已有 IP，就直接用，不再调用 GetProxyCountry
		if job.Result.IP == "" {
			country, ip, countryCodeTag, ispTag, _ := proxyutils.GetProxyCountry(mediaClient, db, ctx, job.CfLoc, job.CfIP)
			if ip == "" {
				break
			}
			job.Result.IP = ip
			job.Result.Country = country
			job.Result.CountryCodeTag = countryCodeTag
			job.Result.ISPTag = ispTag
		}
		var risk string
		err := withRetry(ctx, func() error {
			var e error
			risk, e = platform.CheckIPRisk(ctx, mediaClient, job.Result.IP)
			return e
		})
		if err == nil {
			job.Result.IPRisk = risk
		} else {
			// 失败的可能性高，所以放上日志
			slog.Debug("查询IP风险失败", "error", err)
		}
	}
}

// updateProxyName 更新代理名称
func (pc *ProxyChecker) updateProxyName(res *Result, httpClient *ProxyClient, db *maxminddb.Reader, cfLoc string, cfIP string, jctx context.Context) {
	// 以节点IP查询位置重命名（如果开启）
	if config.Get().RenameNode {
		if res.Country == "" {
			// 使用 MediaCheckTimeout 替换默认的短超时
			locTimeout := config.Get().MediaCheckTimeout
			if locTimeout <= 0 {
				locTimeout = 10
			}
			renameClient := &http.Client{
				Transport: httpClient.Transport,
				Timeout:   time.Duration(locTimeout) * time.Second,
			}

			country, _, countryCodeTag, ispTag, _ := proxyutils.GetProxyCountry(renameClient, db, jctx, cfLoc, cfIP)
			res.Country = country
			res.CountryCodeTag = countryCodeTag
			res.ISPTag = ispTag
		}
		if res.Country != "" {
			res.Proxy["name"] = config.Get().NodePrefix + proxyutils.Rename(res.Country, res.CountryCodeTag)
		} else {
			// 裸断言会在 name 缺失/非字符串时 panic 整个媒体 worker（无 recover）
			originName, _ := res.Proxy["name"].(string)
			res.Proxy["name"] = config.Get().NodePrefix + proxyutils.Rename(res.Country, res.CountryCodeTag) + originName
		}
	}

	name := ""
	if v, ok := res.Proxy["name"].(string); ok {
		name = strings.TrimSpace(v)
	}

	var tags []string
	// 历史速度标签：测速功能已移除，无条件剥离旧的 |x.xMB/s 段（不再追加新标签）
	name = reSpeedTagStrip.ReplaceAllString(name, "")

	if config.Get().MediaCheck {
		// 移除旧标签
		name = reMediaTagStrip.ReplaceAllString(name, "")
	}

	// 平台标签（按用户配置顺序）
	for _, plat := range config.Get().Platforms {
		switch plat {
		case "openai":
			if res.Openai {
				tags = append(tags, "GPT⁺")
			} else if res.OpenaiWeb {
				tags = append(tags, "GPT")
			}
		case "copilot":
			if res.Copilot && res.CopilotAPI {
				tags = append(tags, "CP")
			} else if res.Copilot {
				tags = append(tags, "CP⁻")
			}
		case "x":
			if res.X && !strings.Contains(name, "⁻¹") && !strings.Contains(name, "🏴‍☠️") {
				tags = append(tags, "X")
			}
		case "netflix":
			if res.Netflix {
				tags = append(tags, "NF")
			}
		case "disney":
			if res.Disney {
				tags = append(tags, "D+")
			}
		case "youtube":
			yt := res.Youtube
			switch yt {
			case "":
				// 不可达或封锁，无标签
			default:
				// 分离地区和 Premium 标记
				region := strings.TrimSuffix(yt, "⁻")
				noPremium := strings.HasSuffix(yt, "⁻")

				tag := "YT"
				if noPremium {
					tag += "⁻"
				}
				if region != "" && region != res.Country {
					tag = "YT-" + region
				}
				tags = append(tags, tag)
			}
		case "gemini":
			g := res.Gemini
			if g.Region == "" {
				break
			}
			switch g.Access {
			case platform.AccessBlocked:
				// 明确封锁，不生成标签
			case platform.AccessSuspect:
				tags = append(tags, "GMˀ")
			case platform.AccessNormal:
				tag := "GM"
				if g.IsEU {
					tag = "GM⁻"
				}
				if g.Region != res.Country {
					tag = tag + "-" + g.Region
				}
				tags = append(tags, tag)
			}
		case "iprisk":
			if res.IPRisk != "" {
				tags = append(tags, res.IPRisk)
			}
		case "tiktok":
			if res.TikTok != "" {
				// 只有TikTok地区和节点位置不一致时才添加TikTok地区
				if res.Country != res.TikTok {
					tags = append(tags, "TK-"+res.TikTok)
				} else {
					tags = append(tags, "TK")
				}
			}
		}
	}

	if tag, ok := res.Proxy["sub_tag"].(string); ok && tag != "" {
		tags = append(tags, tag)
	}

	// 运营商标签
	if config.Get().ISPCheck {
		if res.ISPTag != "" {
			tags = append(tags, res.ISPTag)
		}
	}

	// 将所有标记添加到名称中
	if len(tags) > 0 {
		name += "|" + strings.Join(tags, "|")
	}

	res.Proxy["name"] = name
}

type ProxyClient struct {
	*http.Client
	baseTransport *http.Transport
	BytesRead     atomic.Uint64
	BytesWritten  atomic.Uint64
	ctx           context.Context
	cancel        context.CancelFunc
	mProxy        constant.Proxy

	// conns 记录该客户端建立的所有底层连接。
	// CloseIdleConnections 只能关闭「空闲」连接，仍在使用/被遗弃的连接会一直挂着
	// （表现为连接数居高不下、路由器连接表被占满），因此 Close 时统一强制关闭。
	conns sync.Map // map[*countingConn]struct{}
}

// CreateClient 创建独立的代理客户端
func CreateClient(mapping map[string]any) (client *ProxyClient) {
	// 捕获 panic，防止由于底层库解析畸形节点导致整个程序崩溃
	defer func() {
		if r := recover(); r != nil {
			name, _ := mapping["name"].(string)
			slog.Debug("底层mihomo创建代理Client时发生Panic，已自动丢弃该畸形节点", "name", name, "panic", r)
			client = nil // 发生panic时，向外层返回 nil
		}
	}()

	pc := &ProxyClient{}

	var err error

	// 解析代理
	pc.mProxy, err = adapter.ParseProxy(mapping)
	if err != nil {
		slog.Debug("底层mihomo创建代理Client失败", "error", err)
		return nil
	}

	// 初始化全局控制 Context
	pc.ctx, pc.cancel = context.WithCancel(context.Background())
	// 捕获 ctx 用于闭包，防止 pc 指针后续变化（防御性，避免某次测试出现的nil指针错误）
	clientCtx := pc.ctx

	networkLimitDefault := true

	baseTransport := &http.Transport{
		DialContext: func(reqCtx context.Context, network, addr string) (net.Conn, error) {
			// 基于请求的 ctx 创建合并 ctx
			mergedCtx, mergedCancel := context.WithCancel(reqCtx)

			// 使用 context.AfterFunc 监听全局 clientCtx
			// 当 clientCtx (ProxyClient) 被关闭时，立即调用 mergedCancel
			stop := context.AfterFunc(clientCtx, func() {
				mergedCancel()
			})

			// 资源清理
			// 无论拨号成功还是失败，函数返回时：
			// 1. mergedCancel(): 释放 mergedCtx 资源
			// 2. stop(): 注销监听器（cancel 幂等，顺序无实际影响）
			defer mergedCancel()
			defer stop()

			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var u16Port uint16
			if port, err := strconv.ParseUint(portStr, 10, 16); err == nil {
				u16Port = uint16(port)
			}

			// 使用合并后的 ctx 进行拨号
			rawConn, err := pc.mProxy.DialContext(mergedCtx, &constant.Metadata{
				Host:    host,
				DstPort: u16Port,
			})
			if err != nil {
				return nil, err
			}

			cc := &countingConn{
				Conn:         rawConn,
				readCounter:  &pc.BytesRead,
				writeCounter: &pc.BytesWritten,
				networkLimit: networkLimitDefault,
				bucket:       Bucket.Load(),
			}
			cc.onClose = func() { pc.conns.Delete(cc) }
			pc.conns.Store(cc, struct{}{})

			// 拨号期间客户端已被关闭：立即回收，避免 Close 之后才建立成功的连接泄漏
			if clientCtx.Err() != nil {
				_ = cc.Close()
				return nil, clientCtx.Err()
			}
			return cc, nil
		},
		ForceAttemptHTTP2:   true, // 强制尝试 HTTP/2 协议
		DisableKeepAlives:   false,
		Proxy:               nil,
		IdleConnTimeout:     5 * time.Second,
		MaxIdleConnsPerHost: 5,
	}

	pc.baseTransport = baseTransport
	pc.Client = &http.Client{
		Timeout:   effectiveTimeout(),
		Transport: baseTransport,
	}

	// xhttp/splithttp 禁用外层 H2：防止双层 H2 冲突导致 DATA after END_STREAM
	if network, ok := mapping["network"].(string); ok &&
		(network == "xhttp" || network == "splithttp") {
		baseTransport.ForceAttemptHTTP2 = false
	}

	return pc
}

// Close 关闭客户端，释放所有资源
func (pc *ProxyClient) Close() {
	// 防御性检查：防止对 nil 指针调用 Close
	if pc == nil {
		return
	}

	// 发送取消信号
	// 这会立即触发 CreateClient 中 context.AfterFunc 注册的回调，
	// 从而中断所有正在进行的 Dial 过程。
	if pc.cancel != nil {
		pc.cancel()
	}

	// 强制关闭所有仍未关闭的底层连接（sync.Map 允许在 Range 中删除）
	pc.conns.Range(func(k, _ any) bool {
		if cc, ok := k.(*countingConn); ok {
			_ = cc.Close()
		}
		return true
	})

	// 关闭mihomo代理实例
	if pc.mProxy != nil {
		if err := pc.mProxy.Close(); err != nil {
			slog.Error("关闭代理实例失败", "err", err)
		}
	}

	// 关闭 HTTP 连接池
	if pc.Client != nil {
		pc.CloseIdleConnections()
	}

	// 流量统计
	bytesRead := pc.BytesRead.Load()
	bytesWritten := pc.BytesWritten.Load()
	if bytesRead > 0 {
		DOWN.Add(bytesRead)
		TotalBytes.Add(bytesRead)
	}
	if bytesWritten > 0 {
		UP.Add(bytesWritten)
		TotalBytes.Add(bytesWritten)
	}
}

// countingConn 包裹 net.Conn，在网络连接层统计读/写字节数。
type countingConn struct {
	net.Conn
	readCounter  *atomic.Uint64
	writeCounter *atomic.Uint64
	networkLimit bool

	// bucket 在连接建立时捕获一次：热路径（每次 Read）不再做原子加载，
	// 也彻底避免与 run() 的 Bucket.Store(nil) 产生竞争
	bucket *ratelimit.Bucket

	onClose   func() // 关闭时从 ProxyClient.conns 中注销
	closeOnce sync.Once
}

// Close 关闭连接并从所属客户端的连接表中注销（幂等）
func (c *countingConn) Close() error {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.Conn.Close()
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.readCounter.Add(uint64(n))
		// 在连接层消耗 token
		if c.bucket != nil && c.networkLimit {
			c.bucket.Wait(int64(n))
		}
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.writeCounter.Add(uint64(n))
	}
	return n, err
}

// 工具函数
func (pc *ProxyChecker) incrementAvailable() {
	pc.available.Add(1)
	Available.Add(1)
}

// checkCtxDone 提供一个非阻塞的检查，判断上下文是否已结束或是否收到强制关闭信号。
func checkCtxDone(c context.Context) bool {
	if ForceClose.Load() {
		return true
	}
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// isRetryable 判断错误是否值得重试（仅网络层瞬时故障）
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// 明确不重试：context 被外部取消（超出全局限制等）
	if errors.Is(err, context.Canceled) {
		return false
	}
	// 重试：超时（含 mediaClient 自身的 Timeout）
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// 网络错误
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}

	if sysErr, ok := errors.AsType[*os.SyscallError](err); ok {
		switch {
		case errors.Is(sysErr.Err, syscall.ECONNRESET):
			return true

		case errors.Is(sysErr.Err, syscall.EPIPE):
			return true

		case errors.Is(sysErr.Err, syscall.ETIMEDOUT):
			return true
		}
	}

	return false
}

// withRetry 只在 isRetryable 时重试，否则直接返回
func withRetry(ctx context.Context, fn func() error) error {
	var err error
	for i := range MediaCheckMaxRetries {
		// 取消后连首次尝试都不发起：否则 stage 结束/ForceClose 后，
		// 每个在途节点还要白跑 media-check-timeout × 重试次数
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if i > 0 {
			// 指数退避，但不超过全局 timeout
			// 指数退避 + 随机抖动，避免大量任务同一时刻重试
			wait := time.Duration(i*i)*200*time.Millisecond + time.Duration(rand.Int64N(int64(300*time.Millisecond)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err = fn()
		if !isRetryable(err) {
			return err // 成功 or 明确失败，不重试
		}
	}
	return err
}

// checkJobLocation 获取节点归属地并进行地区过滤。
// 通过过滤后将归属地信息写入 job.Result，返回 false 表示节点未通过过滤。
func (job *ProxyJob) checkJobLocation(db *maxminddb.Reader, ctx context.Context) bool {
	filterLocs := config.Get().NodeLoc
	if len(filterLocs) == 0 {
		return true
	}

	locTimeout := config.Get().MediaCheckTimeout
	if locTimeout <= 0 {
		locTimeout = 10
	}
	locClient := &http.Client{
		Transport: job.Client.Transport,
		Timeout:   time.Duration(locTimeout) * time.Second,
	}

	country, ip, countryCodeTag, ispTag, err := proxyutils.GetProxyCountry(locClient, db, ctx, job.CfLoc, job.CfIP)
	if err != nil || country == "" {
		return false
	}

	if !containsLocation(filterLocs, country) {
		return false
	}

	job.Result.IP = ip
	job.Result.Country = country
	job.Result.ISPTag = ispTag
	job.Result.CountryCodeTag = countryCodeTag
	return true
}

func containsLocation(filterLocs []string, country string) bool {
	return lo.ContainsBy(filterLocs, func(loc string) bool {
		return strings.EqualFold(loc, country)
	})
}

// scoreAndRecycle 计算节点健康度分数。若分数 > 0，回收至 historyChan 以便后续统一排序保存。
func (pc *ProxyChecker) scoreAndRecycle(proxy map[string]any, state string, speed int, result *Result) {
	// 识别是否是历史节点或刚成功的节点
	tag, _ := proxy["sub_tag"].(string)
	urlStr, _ := proxy["sub_url"].(string)
	isHistory := tag == "#History" || tag == "#Succeed" || strings.Contains(urlStr, "/history.yaml") || strings.Contains(urlStr, "/all.yaml")

	// 如果是全新抓取来的节点，且本次检测失败，直接无视不记录入历史
	if !isHistory && state != "success" {
		return
	}

	// 提取当前分数，首次打分赋予初始值 10
	score := 10
	if scoreRaw, exists := proxy["history_score"]; exists {
		switch v := scoreRaw.(type) {
		case int:
			score = v
		case float64:
			score = int(v) // yaml 反序列化后数值可能是 float64
		}
	}

	// 动态调整分值
	switch state {
	case "alive_fail":
		score -= 2 // 连通性彻底断开，重罚 (-2)
	case "speed_fail":
		score -= 1 // 速度不达标，轻罚 (-1)
	case "success":
		score += 1 // 存活奖励 (+1)
		// 速度奖励附加
		if speed > 50*1024*1024 { // > 50MB/s 额外 +2
			score += 2
		} else if speed > 20*1024*1024 { // > 20MB/s 额外 +1
			score += 1
		}
		// 流媒体与AI高质量奖励附加
		if result != nil && (result.Openai || result.OpenaiWeb || result.Netflix || result.Disney || result.Gemini.Region != "") {
			score += 1
		}
	case "untested":
		// 未测试(例如达到上限提前终止或被地区强行过滤)，不增不减，保留原分数
	}

	// 锁定最高分限制
	if score > 20 {
		score = 20
	}

	// 如果扣除后大于 0 就能留在历史库中
	if score > 0 {
		proxy["history_score"] = score
		pc.historyChan <- proxy
	}
}
