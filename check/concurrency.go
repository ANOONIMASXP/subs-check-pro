package check

import "github.com/sinspired/subs-check-pro/v3/config"

// 自动档倍率与上限：
//
//	alive = concurrent×20（≤1000）
//	media = concurrent×10（≤500）
//
// concurrent=10（缺省）时即 200/100，与 config.yaml.example 的推荐值一致。
const (
	autoAliveMultiplier = 20
	autoAliveCap        = 1000
	autoMediaMultiplier = 10
	autoMediaCap        = 500
)

// StageConcurrent 计算测活/媒体两阶段的并发数。
//
// 显式配置各自独立优先（互不牵连）：只设置 alive-concurrent 或 media-concurrent
// 其中一项时，另一项单独走自动档，不会把已设置的值拉回自动档。
//
// 自动档以 concurrent 为基准：base = max(1, concurrent)（concurrent=0 兜底为 1，
// 避免产出 0 worker 静默空跑），结果再与 proxyCount 取 min 封顶。
func StageConcurrent(cfg *config.Config, proxyCount int) (alive, media int) {
	alive = min(proxyCount, autoAliveCap)
	media = min(proxyCount, autoMediaCap)

	if cfg.AliveConcurrent > 0 {
		alive = min(proxyCount, cfg.AliveConcurrent)
	} else {
		base := max(1, cfg.Concurrent)
		alive = min(alive, base*autoAliveMultiplier)
	}

	if cfg.MediaConcurrent > 0 {
		media = min(proxyCount, cfg.MediaConcurrent)
	} else {
		base := max(1, cfg.Concurrent)
		media = min(media, base*autoMediaMultiplier)
	}

	return alive, media
}
