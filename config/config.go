// Package config 解析配置文件
package config

import (
	_ "embed"
	"sync/atomic"
)

type Config struct {
	PrintProgress   bool   `yaml:"print-progress"`
	ProgressMode    string `yaml:"progress-mode"`
	Concurrent      int    `yaml:"concurrent"`
	AliveConcurrent int    `yaml:"alive-concurrent"`
	MediaConcurrent int    `yaml:"media-concurrent"`
	EnableIPv6      bool   `yaml:"ipv6"`
	Timeout         int    `yaml:"timeout"`

	// AliveCheck 测活探测方式:
	//   urltest — mihomo 同源 Proxy.URLTest（默认，返回延迟并写 DelayHistory）
	//   http    — 旧版 gstatic HEAD（判定口径与历史版本一致，作为回退）
	AliveCheck string `yaml:"alive-check"`

	// AliveURL 测活探测 URL，必须返回 204（状态码固定 204，不开放配置）
	AliveURL string `yaml:"alive-url"`

	// AliveRetry 测活复测次数。只对 reset/EOF 等“快速失败”的可重试错误复测，
	// 超时不复测（避免死节点耗时翻倍）；0 或负数 = 关闭复测。默认 1。
	AliveRetry int `yaml:"alive-retry"`

	// TotalSpeedLimit 全局连接层限速(MB/s)，0=不限。
	// 作用于所有代理连接的读流量（测活/媒体检测通用），与已删除的测速功能无关。
	TotalSpeedLimit   int     `yaml:"total-speed-limit"`
	Threshold         float32 `yaml:"threshold"`
	GCThreshold       int64   `yaml:"gc-threshold"`
	MediaCheckTimeout int     `yaml:"media-check-timeout"`

	// FilterRegex 正则筛选：作为 Sub-Store 官方节点操作 `Regex Filter` 注入到
	// 本程序推送的 sub.process 首位，由 Sub-Store 在下载/产出时执行。
	// 匹配节点 name；多条任一命中；(?i) 前缀 = 忽略大小写；JS 正则语法。
	// 留空 = 不注入。注意：非法 JS 正则会导致订阅下载/产出失败。
	FilterRegex []string `yaml:"filter-regex"`

	// FilterRegexKeep true=保留命中节点(默认)，false=剔除命中节点
	FilterRegexKeep bool `yaml:"filter-regex-keep"`

	GithubToken          string `yaml:"github-token"`
	SubUrlsReTry         int    `yaml:"sub-urls-retry"`
	SubUrlsRetryInterval int    `yaml:"sub-urls-retry-interval"`
	SubUrlsTimeout       int    `yaml:"sub-urls-timeout"`
	SubUrlMaxSizeMB      int    `yaml:"sub-url-max-size-mb"`

	// SubsParseBatch 每批次发往去重队列的节点数
	// 生产者攒够该数量后整批发送，消费者逐批接收处理。
	// 在两次 FreeOSMemory 之间，流水线最多同时驻留 (并发数 + chanBuf) × batchSize 个节点。
	// 太小→channel 调度频繁；太大→流水线底座内存随之线性增大。
	// 默认 3000
	SubsParseBatch int `yaml:"subs-parse-batch"`

	// SubsDedupeBatch 消费者每处理多少节点触发一次 debug.FreeOSMemory()，控制内存归还 OS 的频率。
	// 越小→归还越频繁，峰值越低，GC 停顿越多；越大→峰值越高，停顿越少。
	// 默认 100000，建议范围 20000–500000。0 或负数视为默认值。
	SubsDedupeBatch int `yaml:"subs-dedupe-batch"`

	// MemoryLimitMB 设置 Go 运行时软内存上限（GOMEMLIMIT，单位 MB）。
	// 0 = 不显式配置，按以下优先级自动决定：
	//   Docker 容器：GOMEMLIMIT 环境变量 > cgroup 内存限制(打七五折)
	//   普通主机：系统物理内存打七五折
	MemoryLimitMB int `yaml:"memory-limit-mb"`

	// GCPercent 对应 Go 的 GOGC：堆允许长到存活对象的多少倍才触发下一次 GC，
	// 不是"空闲内存百分比"。值越小 GC 越频繁、内存峰值越低、CPU 开销略增。
	// 默认 70（Go 默认是 100）。<=0 时使用 Go 默认值 100。
	// 注意：真正防止 OOM 的是 MemoryLimitMB；这个只是日常情况下的内存/CPU 取舍旋钮。
	GCPercent int `yaml:"gc-percent"`

	NetGuard         bool     `yaml:"net-guard"`
	SubUrlsRemote    []string `yaml:"sub-urls-remote"`
	SubUrls          []string `yaml:"sub-urls"`
	SuccessRate      float64  `yaml:"success-rate"`
	MihomoAPIURL     string   `yaml:"mihomo-api-url"`
	MihomoAPISecret  string   `yaml:"mihomo-api-secret"`
	ListenPort       string   `yaml:"listen-port"`
	RenameNode       bool     `yaml:"rename-node"`
	UseLastResult    bool     `yaml:"use-last-result"`
	UseHistoryResult bool     `yaml:"use-history-result"`

	// KeepSuccessProxies 旧配置项（兼容保留）：true 等效 use-last-result + use-history-result，
	// 加载后由 app.loadConfig 归并到上面两个字段，本字段不参与实际读取逻辑。
	KeepSuccessProxies bool `yaml:"keep-success-proxies"`

	OutputDir string `yaml:"output-dir"`
	// ConfigDir 运行时由 app.loadConfig 注入，值为当前配置文件所在目录。
	// 不参与 YAML 序列化，仅供 save/method/local.go 计算默认输出路径使用。
	ConfigDir    string `yaml:"-"`
	SubStorePort string `yaml:"sub-store-port"`
	SubStorePath string `yaml:"sub-store-path"`

	// ISPCheck 是否开启出口 ISP 类型检测（机房/住宅/移动/商宽/教育/政府/银行等）
	ISPCheck bool `yaml:"isp-check"`

	// ISPTimeout 是 ISP 检查的超时时间，单位为秒
	ISPTimeout int `yaml:"isp-timeout"`

	// 以下四个渠道的 apikey 均为可选：留空则该渠道自动跳过，不参与轮询。
	// 建议至少配置 2 个渠道，通过轮询F叠加每日免费额度，并在某一渠道
	// 请求失败（超额 / 网络错误）时自动切换到下一个渠道。

	// ISPCheckAPIKeyIPAPI ipapi.is 的 apikey（https://ipapi.is）
	// 免费额度：注册后每天 1000 次
	ISPCheckAPIKeyIPAPI string `yaml:"isp-check-api-key-ipapi"`

	// ISPCheckAPIKeyProxyCheck proxycheck.io 的 apikey（https://proxycheck.io）
	// 免费额度：每天 1000 次（另有约 5 倍的突发令牌可用）
	ISPCheckAPIKeyProxyCheck string `yaml:"isp-check-api-key-proxycheck"`

	// ISPCheckAPIKeyIPLocate iplocate.io 的 apikey（https://iplocate.io）
	// 免费额度：每天 1000 次，免费版与付费版字段完全一致
	ISPCheckAPIKeyIPLocate string `yaml:"isp-check-api-key-iplocate"`

	// ISPCheckAPIKeyIPData ipdata.co 的 apikey（https://ipdata.co）
	// 免费额度：每天 1500 次（或每月 45000 次）
	ISPCheckAPIKeyIPData string `yaml:"isp-check-api-key-ipdata"`

	MediaCheck       bool     `yaml:"media-check"`
	Platforms        []string `yaml:"platforms"`
	MaxMindDBPath    string   `yaml:"maxmind-db-path"`
	DropBadCfNodes   bool     `yaml:"drop-bad-cf-nodes"`
	EnhancedTag      bool     `yaml:"enhanced-tag"`
	SuccessLimit     int32    `yaml:"success-limit"`
	NodePrefix       string   `yaml:"node-prefix"`
	NodeType         []string `yaml:"node-type"`
	NodeLoc          []string `yaml:"node-loc"`
	CallbackScript   string   `yaml:"callback-script"`
	SystemProxy      string   `yaml:"system-proxy"`
	GithubProxy      string   `yaml:"github-proxy"`
	GithubProxyGroup []string `yaml:"ghproxy-group"`
}

var OriginDefaultConfig = &Config{
	ListenPort: ":8199",

	// 拉取订阅并发度：缺省时必须 >=1，否则无缓冲信号量会让获取订阅阶段永久挂起
	Concurrent: 10,

	// 测活超时：缺省 0 会让测活没有任何截止时间（挂死节点耗尽拨号超时），与示例配置对齐
	Timeout: 6000,

	// 测活：默认 mihomo 同源 URLTest，可整体回退到旧版 http HEAD
	AliveCheck: "urltest",
	AliveURL:   "https://gstatic.com/generate_204",
	AliveRetry: 1,

	// 筛选：FilterRegex 默认空（不注入 Sub-Store 节点操作）
	FilterRegexKeep: true,

	Platforms: []string{
		"iprisk",
		"openai",
		"gemini",
		"youtube",
	},

	Threshold:   0.75,
	GCThreshold: 20000,

	// 每个线程获取3000个节点时进入一次去重队列
	SubsParseBatch: 3000,

	// 10 万原始节点触发一次；百万量级约 10 次 GC，CPU 开销可忽略
	SubsDedupeBatch: 100000,

	SubUrlMaxSizeMB: 50,

	NetGuard: false,

	UseLastResult:    false,
	UseHistoryResult: false,

	ISPTimeout: 5, // 默认 5 秒，最高 15 秒
}

// global 保存当前生效配置，只能通过 Set/Mutate 发布。
var global atomic.Pointer[Config]

// Get 返回当前生效配置的只读快照。
//
// 快照一旦发布即视为**不可变**：任何字段修改都必须走 Set（整体替换）
// 或 Mutate（拷贝-修改-发布），禁止对返回指针做原地写入（会造成数据竞争）。
// 同函数内多次读取建议在入口取一次快照（cfg := config.Get()）。
// 尚未加载（首次 Set 之前）时返回零值配置，与旧版 GlobalConfig 初始态一致。
func Get() *Config {
	if c := global.Load(); c != nil {
		return c
	}
	return &Config{}
}

// Set 发布新配置，是全局配置的唯一整体替换入口。
func Set(c *Config) {
	global.Store(c)
}

// Mutate 对当前配置做值拷贝，交给 fn 修改后发布（copy-on-write）。
// 并发写场景下的唯一合法就地修改方式。
func Mutate(fn func(*Config)) {
	cp := *Get()
	fn(&cp)
	Set(&cp)
}

//go:embed config.yaml.example
var DefaultConfigTemplate []byte
