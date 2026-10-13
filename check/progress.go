package check

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// CurrentStepName 用于 UI 显示当前阶段名称
var CurrentStepName atomic.Value
var AliveCount atomic.Uint32 // 存储测活可用节点

// ProgressWeight 不同检测阶段的进度权重
type ProgressWeight struct {
	alive float64
	media float64
}

// ProgressTracker 存储每个阶段的检测进度信息
type ProgressTracker struct {
	totalJobs atomic.Int32 // 初始总任务数

	// 已检测数量（执行完计数）
	aliveDone atomic.Int32
	mediaDone atomic.Int32

	// 成功数量（通过计数）
	aliveSuccess atomic.Int32

	// 当前处于 测活-媒体检测 阶段
	currentStage atomic.Int32

	// 确保进度条输出完成
	finalized atomic.Bool

	finishAliveOnce sync.Once
}

// NewProgressTracker 初始化进度追踪器并重置外部原子变量。
func NewProgressTracker(total int) *ProgressTracker {
	pt := &ProgressTracker{}
	if total > math.MaxInt32 {
		total = math.MaxInt32
	}
	pt.totalJobs.Store(int32(total))
	pt.currentStage.Store(0)

	ProxyCount.Store(uint32(total))
	Progress.Store(0)
	Available.Store(0)

	// 初始化进度权重
	progressWeight = getCheckWeight(mediaON)

	// 默认阶段名（根据配置）
	mode := config.Get().ProgressMode
	if mode == "stage" {
		CurrentStepName.Store("测活")
	} else {
		CurrentStepName.Store("进度")
	}

	return pt
}

// getCheckWeight 根据启用的检查来确定进度权重的分配。
// 权重取自历史「无测速」分支，保证进度百分比与既有行为完全一致。
func getCheckWeight(mediaON bool) ProgressWeight {
	if mediaON {
		return ProgressWeight{alive: 80, media: 20}
	}
	return ProgressWeight{alive: 100}
}

// CountAlive 标记一个存活检测已完成，并更新进度。
func (pt *ProgressTracker) CountAlive(success bool) {
	pt.aliveDone.Add(1)
	AliveCount.Add(1)
	if success {
		pt.aliveSuccess.Add(1)
	}
	pt.refresh()
}

// CountMedia 标记一个媒体检测已完成，并更新进度。
func (pt *ProgressTracker) CountMedia() {
	pt.mediaDone.Add(1)
	pt.refresh()
}

// FinishAliveStage 在所有存活检测完成后，将追踪器转换到下一阶段。
func (pt *ProgressTracker) FinishAliveStage() {
	pt.finishAliveOnce.Do(func() {
		aliveSucc := int(pt.aliveSuccess.Load())
		// 如果没有活节点，直接结束
		if aliveSucc <= 0 && mediaON {
			pt.Finalize()
			return
		}

		// 根据配置动态决定下一个阶段
		if mediaON {
			pt.currentStage.Store(2) // 进入媒体检测阶段
		}

		if !mediaON && config.Get().RenameNode {
			pt.currentStage.Store(-1)
		}

		pt.refresh()
	})
}

// Finalize 确保进度被标记为 100% 完成。
func (pt *ProgressTracker) Finalize() {
	pt.finalized.Store(true)
	// 强制设置为 100%
	total := ProxyCount.Load()
	if total == 0 {
		total = 1 // 防止除以0
	}
	Progress.Store(total)
	pt.refresh()
}

// refresh 根据所选算法更新进度的统一入口。
func (pt *ProgressTracker) refresh() {
	switch config.Get().ProgressMode {
	case "stage":
		pt.refreshStage()
	default:
		pt.refreshDynamic()
	}
}

// refreshDynamic 根据各阶段完成率的加权和来计算进度，支持中途停止信号
func (pt *ProgressTracker) refreshDynamic() {
	stopped := Successlimited.Load() || ForceClose.Load() || ProcessResults.Load() || pt.finalized.Load()

	// 1. 确定计算基数（分母）
	// 如果触发了限制（成功数达到 or 强制关闭），分母不再是总订阅数，而是“实际已测活数”
	// 这样进度条会瞬间适配到剩余任务上。
	realTotal := int(pt.totalJobs.Load())

	// 处理停止信号
	if stopped {
		pt.refreshStage()
		return
	}

	if realTotal <= 0 {
		return
	}

	aliveDone := int(pt.aliveDone.Load())
	mediaDone := int(pt.mediaDone.Load())

	aliveSucc := int(pt.aliveSuccess.Load())

	// 2. 计算各阶段完成率 (0.0 - 1.0)
	// 限制最大值为 1.0，防止因异步计数导致瞬间溢出
	ratio := func(done, total int) float64 {
		if total <= 0 {
			return 0
		}
		if r := float64(done) / float64(total); r < 1.0 {
			return r
		}
		return 1.0
	}

	rAlive := ratio(aliveDone, realTotal)

	// 媒体检测的分母：存活成功数
	rMedia := 0.0
	if aliveSucc > 0 {
		rMedia = ratio(mediaDone, aliveSucc)
	}

	// 3. 约束系数 (Constraint)
	// 后一阶段的总体贡献不能超过前一阶段的进度。
	// 例如：测活只完成了 10%，那么媒体检测即使完成了 100% (相对于已活节点)，
	// 它对总进度的贡献也应该受限于测活的 10%。

	// P_Total = (rAlive * wAlive) + (rMedia * wMedia * rAlive)
	// 这种级联乘法能保证进度条平滑，不会因为后一阶段任务量少而瞬间跳变。

	pAlive := rAlive * progressWeight.alive
	pMedia := 0.0
	if mediaON {
		pMedia = rMedia * progressWeight.media * rAlive
	}

	finalPercent := pAlive + pMedia

	// 4. 映射回数值
	if pt.finalized.Load() {
		finalPercent = 100.0
	}

	// 为了兼容 GUI/CLI 显示，将百分比映射回 realTotal
	// ProxyCount 存储当前的“有效总数”
	ProxyCount.Store(uint32(realTotal))

	mapped := uint32(math.Ceil(finalPercent / 100.0 * float64(realTotal)))
	Progress.Store(min(mapped, uint32(realTotal)))
}

// refreshStage 分阶段算法：分母动态切换
func (pt *ProgressTracker) refreshStage() {
	stage := int(pt.currentStage.Load())

	switch stage {
	case 0: // 存活检测阶段
		CurrentStepName.Store("测活")
		total := uint32(pt.totalJobs.Load())
		// 如果在测活阶段就停止了（比如强制停止），修正总数显示
		if ForceClose.Load() || Successlimited.Load() {
			done := uint32(pt.aliveDone.Load())
			if done > 0 {
				total = done
			}
		}

		done := min(uint32(pt.aliveDone.Load()), total)

		ProxyCount.Store(total)
		Progress.Store(done)

	case 2: // 媒体检测阶段
		CurrentStepName.Store("媒体")
		// 分母：测活阶段的成功数
		total := uint32(pt.aliveSuccess.Load())
		if total == 0 {
			ProxyCount.Store(0)
			Progress.Store(0)
			return
		}

		done := min(uint32(pt.mediaDone.Load()), total)

		ProxyCount.Store(total)
		Progress.Store(done)
	case -1: // 重命名阶段
		CurrentStepName.Store("重命名")
		base := Available.Load()
		if base == 0 {
			ProxyCount.Store(0)
			Progress.Store(0)
			return
		}
		done := min(uint32(pt.mediaDone.Load()), base)
		ProxyCount.Store(base)
		Progress.Store(done)
	}

	// 处理停止信号下的显示文字
	if ProcessResults.Load() {
		total := uint32(pt.aliveDone.Load())
		if total == 0 {
			ProxyCount.Store(0)
			Progress.Store(0)
			return
		}

		done := min(uint32(pt.aliveDone.Load()), total)

		ProxyCount.Store(total)
		Progress.Store(done)
	}
}

var progressDone chan struct{}
var progressFinished chan struct{}

// StartProgress 启动进度显示，暴露给外部以便统一管理进度条生命周期
func StartProgress() {
	if !config.Get().PrintProgress {
		return
	}
	// 已在运行则不重复启动：覆盖 channel 会泄漏上一个渲染 goroutine
	if progressDone != nil {
		return
	}
	progressDone = make(chan struct{})
	progressFinished = make(chan struct{})

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-progressDone:
				fmt.Print(renderProgressString())
				fmt.Println()
				close(progressFinished)
				return
			case <-ticker.C:
				fmt.Print(renderProgressString())
			}
		}
	}()
}

// StopProgress 停止进度显示。
// 必须无条件按已启动的 channel 收尾，不能读当前 PrintProgress 配置：
// 热重载把开关改成 false 后，旧实现提前 return 会让渲染 goroutine 永久泄漏。
func StopProgress() {
	if progressDone == nil {
		return
	}
	close(progressDone)
	<-progressFinished
	progressDone = nil
	progressFinished = nil
}

// renderProgressString 计算并格式化进度条字符串。
func renderProgressString() string {
	currentChecked := int(Progress.Load())
	total := int(ProxyCount.Load())
	available := Available.Load()
	etaSec := ETASeconds.Load()
	step := ""

	// 获取阶段名称
	if s, ok := CurrentStepName.Load().(string); ok {
		step = s
	}

	var percent float64
	if total == 0 {
		if ProcessResults.Load() || step == "保存配置" {
			percent = 100
		}
	} else {
		percent = float64(currentChecked) / float64(total) * 100
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}

	// ETA 后缀：-1=计算中, 0=完成/空闲不显示, >0=剩余时间
	etaSuffix := ""
	switch {
	case etaSec == -1:
		etaSuffix = " ETA: \033[90m--:--\033[0m"
	case etaSec > 0:
		etaSuffix = " ETA: \033[36m" + FormatEta(etaSec) + "\033[0m"
	}

	barWidth := 40
	barFilled := min(int(percent/100*float64(barWidth)), barWidth) // 先限制不超过 barWidth

	bar := strings.Repeat("=", barFilled) + ">"
	padding := max(barWidth-len(bar), 0)
	bar += strings.Repeat(" ", padding)

	return "\r\x1b[K" + step +
		": [" + bar + "] " +
		strconv.FormatFloat(percent, 'f', 1, 64) + "% " +
		"(" + strconv.Itoa(currentChecked) + "/" + strconv.Itoa(total) + ") " +
		"可用: \033[32m" + strconv.Itoa(int(available)) + "\033[0m" +
		etaSuffix
}

// pad2 补零到两位数
func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// FormatEta 预计检测剩余时间格式化
func FormatEta(sec int64) string {
	if sec <= 0 {
		return "0s"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60

	switch {
	case h > 0:
		return strconv.FormatInt(h, 10) + "h" +
			pad2(m) + "m" +
			pad2(s) + "s"
	case m > 0:
		return strconv.FormatInt(m, 10) + "m" +
			pad2(s) + "s"
	default:
		return strconv.FormatInt(s, 10) + "s"
	}
}
