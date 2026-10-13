package check

import (
	"testing"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// TestStageConcurrent 倍率制自动档：concurrent×20/×10，受 proxyCount 与上限封顶。
func TestStageConcurrent(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.Config
		proxyCount int
		wantAlive  int
		wantMedia  int
	}{
		{
			name:       "默认档 concurrent=10 → 200/100（=示例配置推荐值）",
			cfg:        config.Config{Concurrent: 10},
			proxyCount: 5000,
			wantAlive:  200,
			wantMedia:  100,
		},
		{
			name:       "proxyCount 封顶",
			cfg:        config.Config{Concurrent: 10},
			proxyCount: 80,
			wantAlive:  80,
			wantMedia:  80,
		},
		{
			name:       "双上限生效 concurrent=1000 → 1000/500",
			cfg:        config.Config{Concurrent: 1000},
			proxyCount: 5000,
			wantAlive:  1000,
			wantMedia:  500,
		},
		{
			name:       "concurrent=0 兜底 base=1 → 20/10（不再产出 0 worker）",
			cfg:        config.Config{Concurrent: 0},
			proxyCount: 5000,
			wantAlive:  20,
			wantMedia:  10,
		},
		{
			name: "半显式：只设 alive，media 独立走自动档",
			cfg:  config.Config{Concurrent: 10, AliveConcurrent: 300},
			// 旧 bug：只设一项会把两项都拉回自动档（alive 丢弃 300）
			proxyCount: 5000,
			wantAlive:  300,
			wantMedia:  100,
		},
		{
			name:       "半反向：只设 media，alive 独立走自动档",
			cfg:        config.Config{Concurrent: 10, MediaConcurrent: 50},
			proxyCount: 5000,
			wantAlive:  200,
			wantMedia:  50,
		},
		{
			name:       "显式值超 proxyCount → min 封顶",
			cfg:        config.Config{Concurrent: 10, AliveConcurrent: 500, MediaConcurrent: 400},
			proxyCount: 50,
			wantAlive:  50,
			wantMedia:  50,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			alive, media := StageConcurrent(&c.cfg, c.proxyCount)
			if alive != c.wantAlive || media != c.wantMedia {
				t.Errorf("StageConcurrent(concurrent=%d, alive=%d, media=%d, proxyCount=%d) = (%d, %d), want (%d, %d)",
					c.cfg.Concurrent, c.cfg.AliveConcurrent, c.cfg.MediaConcurrent, c.proxyCount,
					alive, media, c.wantAlive, c.wantMedia)
			}
			if alive < 1 || media < 1 {
				t.Errorf("并发数下限必须 ≥1（0 worker 会静默空跑）: alive=%d media=%d", alive, media)
			}
		})
	}
}
