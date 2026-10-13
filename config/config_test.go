package config

import (
	"sync"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestUnknownKeysIgnored 旧配置里残留的已删除配置键必须被静默忽略。
// 保证删除某项配置后，用户已有的 config.yaml 仍能正常加载（loadConfig 走 goccy 裸 Unmarshal）。
func TestUnknownKeysIgnored(t *testing.T) {
	raw := []byte(`timeout: 6000
alive-check: urltest
alive-retry: 1
max-delay: 800
speed-mode: estimate
sub-store-push-service: 'https://api.day.app/xxx'
github-api-mirror: 'https://mirror.example'
`)

	cfg := *OriginDefaultConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("含未知键的配置解析失败（老配置会因此无法启动）: %v", err)
	}

	if cfg.Timeout != 6000 {
		t.Errorf("Timeout = %d, want 6000", cfg.Timeout)
	}
	if cfg.AliveCheck != "urltest" {
		t.Errorf("AliveCheck = %q, want urltest", cfg.AliveCheck)
	}
	if cfg.AliveRetry != 1 {
		t.Errorf("AliveRetry = %d, want 1", cfg.AliveRetry)
	}
	if cfg.AliveURL != OriginDefaultConfig.AliveURL {
		t.Errorf("AliveURL = %q, want %q", cfg.AliveURL, OriginDefaultConfig.AliveURL)
	}
}

// TestGetSetMutate 访问器语义：Set 整体发布、Mutate 拷贝-改-发布且不污染已发布快照。
func TestGetSetMutate(t *testing.T) {
	prev := Get()
	t.Cleanup(func() { Set(prev) })

	Set(&Config{Timeout: 1000})
	snap := Get()
	if snap.Timeout != 1000 {
		t.Fatalf("Set 后 Get 读到 %d, want 1000", snap.Timeout)
	}

	Mutate(func(c *Config) { c.Timeout = 2000 })
	if snap.Timeout != 1000 {
		t.Errorf("Mutate 污染了旧快照: %d, want 1000（发布后的配置必须视为不可变）", snap.Timeout)
	}
	if got := Get().Timeout; got != 2000 {
		t.Errorf("Mutate 后 Get 读到 %d, want 2000", got)
	}
}

// TestGetSetRace 并发 Get + Mutate 发布，配合 -race 验证无数据竞争。
func TestGetSetRace(t *testing.T) {
	prev := Get()
	t.Cleanup(func() { Set(prev) })

	Set(&Config{Timeout: 6000, SubUrls: []string{"a", "b"}})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cfg := Get()
				_ = cfg.Timeout
				_ = len(cfg.SubUrls)
			}
		}()
	}

	for i := 0; i < 2000; i++ {
		Mutate(func(c *Config) { c.Timeout = 1000 + i })
	}
	close(stop)
	wg.Wait()

	if Get().Timeout != 1000+1999 {
		t.Errorf("最终 Timeout = %d, want %d", Get().Timeout, 1000+1999)
	}
}
