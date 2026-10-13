package check

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sinspired/subs-check-pro/v3/config"
)

// setAliveConfig 覆盖测活相关配置，测试结束后恢复
func setAliveConfig(t *testing.T, mode, url string, retry, timeoutMs int) {
	t.Helper()
	prev := config.Get()
	t.Cleanup(func() { config.Set(prev) })

	config.Mutate(func(c *config.Config) {
		c.AliveCheck = mode
		c.AliveURL = url
		c.AliveRetry = retry
		c.Timeout = timeoutMs
	})
}

// newAliveTestJob 起一个本地探测服务并用 mihomo direct 节点构造成测活任务
func newAliveTestJob(t *testing.T, handler http.HandlerFunc) (*ProxyJob, string) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	job := &ProxyJob{Client: CreateClient(map[string]any{
		"name": "alive-test",
		"type": "direct",
	})}
	if job.Client == nil || job.Client.mProxy == nil {
		t.Fatal("创建 direct 代理客户端失败")
	}
	t.Cleanup(job.Close)

	return job, server.URL + "/generate_204"
}

func slow204(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusNoContent)
	}
}

// TestAliveCheckBothModes 204 节点：两种模式都判活且能拿到延迟
func TestAliveCheckBothModes(t *testing.T) {
	for _, mode := range []string{"urltest", "http"} {
		t.Run(mode, func(t *testing.T) {
			setAliveConfig(t, mode, "", 0, 3000)
			job, url := newAliveTestJob(t, slow204(20*time.Millisecond))
			config.Mutate(func(c *config.Config) { c.AliveURL = url })

			ok, delay := checkAlive(job, context.Background())
			if !ok {
				t.Fatal("204 节点应判活")
			}
			if delay < 15 {
				t.Errorf("延迟应包含服务端 20ms 等待，实际 %dms", delay)
			}
			if job.Result.Delay != 0 {
				t.Errorf("checkAlive 不应写 Result.Delay（由调用方写入），实际 %d", job.Result.Delay)
			}
		})
	}
}

// TestAliveCheckRejectsNon204 状态码不符：两种模式都判死，且不复测
func TestAliveCheckRejectsNon204(t *testing.T) {
	for _, mode := range []string{"urltest", "http"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			setAliveConfig(t, mode, "", 3, 3000)

			job, url := newAliveTestJob(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			config.Mutate(func(c *config.Config) { c.AliveURL = url })

			ok, _ := checkAlive(job, context.Background())
			if ok {
				t.Fatal("500 节点不应判活")
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("状态码不符不应复测，实际请求 %d 次", n)
			}
		})
	}
}

// TestAliveDefaultConfigIsLegacy http + 0 复测 → 与旧版口径一致（204 活 / 非 204 死）
func TestAliveDefaultConfigIsLegacy(t *testing.T) {
	setAliveConfig(t, "http", "", 0, 3000)

	liveJob, liveURL := newAliveTestJob(t, slow204(time.Millisecond))
	config.Mutate(func(c *config.Config) { c.AliveURL = liveURL })
	if ok, _ := checkAlive(liveJob, context.Background()); !ok {
		t.Error("204 节点应判活")
	}

	deadJob, deadURL := newAliveTestJob(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	})
	config.Mutate(func(c *config.Config) { c.AliveURL = deadURL })
	if ok, _ := checkAlive(deadJob, context.Background()); ok {
		t.Error("302 节点不应判活（旧口径跟随重定向后同样不命中 204）")
	}
}

// TestRunAliveProbeRetry 复测口径：只复测“快速失败”，超时/状态码不符/取消都不复测
func TestRunAliveProbeRetry(t *testing.T) {
	type result struct {
		ok    bool
		delay int
		err   error
	}
	cases := []struct {
		name      string
		retries   int
		script    []result
		wantOK    bool
		wantDelay int
		wantCalls int
	}{
		{name: "快速失败后成功", retries: 1, script: []result{{err: syscall.ECONNRESET}, {ok: true, delay: 42}}, wantOK: true, wantDelay: 42, wantCalls: 2},
		{name: "超时不复测", retries: 3, script: []result{{err: context.DeadlineExceeded}}, wantOK: false, wantCalls: 1},
		{name: "状态码不符不复测", retries: 3, script: []result{{ok: false, delay: 7}}, wantOK: false, wantDelay: 7, wantCalls: 1},
		{name: "取消立即退出", retries: 1, script: []result{{err: context.Canceled}}, wantOK: false, wantCalls: 1},
		{name: "不可重试错误不复测", retries: 2, script: []result{{err: errors.New("connection refused")}}, wantOK: false, wantCalls: 1},
		{name: "retries=0 不复测", retries: 0, script: []result{{err: syscall.ECONNRESET}}, wantOK: false, wantCalls: 1},
		{name: "复测次数耗尽", retries: 1, script: []result{{err: syscall.ECONNRESET}, {err: syscall.ECONNRESET}}, wantOK: false, wantCalls: 2},
		{name: "负数按 0 处理", retries: -5, script: []result{{err: syscall.ECONNRESET}}, wantOK: false, wantCalls: 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			probe := func(ctx context.Context) (bool, int, error) {
				if calls >= len(c.script) {
					t.Fatalf("探测被调用 %d 次，超出脚本长度 %d", calls+1, len(c.script))
				}
				r := c.script[calls]
				calls++
				return r.ok, r.delay, r.err
			}

			ok, delay, err := runAliveProbe(context.Background(), probe, c.retries)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v (err=%v)", ok, c.wantOK, err)
			}
			if delay != c.wantDelay {
				t.Errorf("delay = %d, want %d", delay, c.wantDelay)
			}
			if calls != c.wantCalls {
				t.Errorf("探测调用 %d 次, want %d", calls, c.wantCalls)
			}
		})
	}
}

// TestIsRetryableAlive 只允许快速失败参与复测
func TestIsRetryableAlive(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"超时", context.DeadlineExceeded, false},
		{"取消", context.Canceled, false},
		{"连接重置", syscall.ECONNRESET, true},
		{"意外 EOF", io.ErrUnexpectedEOF, true},
		{"明确失败", errors.New("connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryableAlive(c.err); got != c.want {
				t.Errorf("isRetryableAlive(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}

	if isRetryableAlive(errors.Join(context.DeadlineExceeded)) {
		t.Error("包装后的超时也不应复测")
	}
}

// TestExpectedAliveStatus 期望状态码必须是严格 204（空集会放行任意状态码）
func TestExpectedAliveStatus(t *testing.T) {
	ranges := expectedAliveStatus()
	if len(ranges) == 0 {
		t.Fatal("期望状态码不能是空集（空集等价于放行任何状态码）")
	}
	if !ranges.Check(204) {
		t.Error("204 应命中")
	}
	if ranges.Check(500) {
		t.Error("500 不应命中")
	}
	if ranges.Check(200) {
		t.Error("200 不应命中")
	}
}

// TestAliveConfigDefaults 测活配置的默认值
func TestAliveConfigDefaults(t *testing.T) {
	d := config.OriginDefaultConfig
	if d.AliveCheck != "urltest" {
		t.Errorf("AliveCheck 默认 %q, want urltest", d.AliveCheck)
	}
	if d.AliveURL != "https://gstatic.com/generate_204" {
		t.Errorf("AliveURL 默认 %q", d.AliveURL)
	}
	if d.AliveRetry != 1 {
		t.Errorf("AliveRetry 默认 %d, want 1", d.AliveRetry)
	}
}

// TestAliveCheckModeFallback 未知配置回退 urltest
func TestAliveCheckModeFallback(t *testing.T) {
	prev := config.Get()
	t.Cleanup(func() { config.Set(prev) })

	for _, c := range []struct{ in, want string }{
		{"", "urltest"},
		{"urltest", "urltest"},
		{"URLTest", "urltest"},
		{" http ", "http"},
		{"whatever", "urltest"},
	} {
		in := c.in
		config.Mutate(func(cfg *config.Config) { cfg.AliveCheck = in })
		if got := aliveCheckMode(); got != c.want {
			t.Errorf("aliveCheckMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
