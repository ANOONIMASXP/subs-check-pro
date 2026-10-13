# AGENTS.md — subs-check-pro（本仓库 = 基于上游 main 的精简分支）

面向在本仓库工作的 AI/开发者。**先读本文件再改代码。**

## 1. 项目概览
- Go CLI 项目（`github.com/sinspired/subs-check-pro/v3`），入口 `main.go`，主逻辑 `app/`、`check/`、`save/`、`substore/`、`assets/`、`config/`、`proxy/`。
- 功能：拉取订阅 → 并发检测节点（测活/媒体/ISP）→ 输出订阅文件 → 同步到内置 Sub-Store。
- **基线**：`master` 重建于上游 `sinspired/subs-check-pro` 的 `main`（2026-10-13 合并，含 v3/Alpha 之后的 89 个上游提交），其上叠加本分支的**大量删减**与本地改造；合并前历史在备份分支 `backup/pre-main-merge`。

## 2. 构建与验证（每次改动后必做）
```powershell
go build ./...        # 必须通过
go vet ./...          # 必须通过
go test ./check/ ./config/ ./substore/   # 验证基线：这三包必须全绿
```
构建脚本（PowerShell，**必须保持 UTF-8 带 BOM**，否则 PS5.1 解析中文/emoji 会报语法错）：
```powershell
.\go_build.ps1 -Debug -Win        # 仅编 Windows/amd64
.\go_build.ps1 -Lin               # Linux 全架构（Zig musl 交叉编译）
.\go_build.ps1 -Android -Debug    # 仅编 Android/arm64（需 NDK）
.\go_build.ps1 -Android           # 编 + 打包 tar.gz
```
- 不传 `-Version` 时，脚本自动读取 `init.go` 的 `var Version = "..."`。
- 产物在 `bin/` 下（`Windows_x86_64/`、`Linux_aarch64/`、`Android_aarch64/`）。
- `go test ./...` 中 **`check/platform`、`proxy` 的部分用例要联网**（gemini/copilot/cf-trace/IP API），离线/被墙时失败属正常，**不阻塞**；`substore` 集成测试会起真实引擎占 **18973** 端口，`-short` 可跳过。

## 3. 工具链与环境（本机已配置）
- Go **1.27.0**；CGO **必须开启**（`buke/quickjs-go` 是 CGO 绑定）。
- Windows 交叉/本地 C 编译器用 **Zig**：`C:\tools\zig\zig.exe`（`zig cc`）。
- Android 用 NDK r27d：`C:\Android\android-ndk-r27d`（脚本用 `aarch64-linux-android24-clang`）。
- 依赖下载走国内镜像（`proxy.golang.org` 被墙）：
  - `GOPROXY=https://goproxy.cn,direct`
  - `GOSUMDB=sum.golang.google.cn`
  - 已写入 `.vscode/settings.json` 的 `go.toolsEnvVars` / `gopls.build.env`，并设为用户环境变量。
- 编辑器诊断报 `undefined: quickjs.NewRuntime` 之类的**实为 CGO 未生效**；`golang.org/x/...` 报错**实为代理未生效**，不是代码问题。

## 4. 本分支已删除的功能（**勿回退、勿重新引入**）
| 已删除 | 说明 |
|---|---|
| Web 控制面板 / 管理 API | 无 `/admin`、`/api/*`、主题/版本/分析路由；无 `api-key` |
| 桌面 GUI | 已删 `app/gui.go`、`GetRouter`、`START_FROM_GUI` 等；外部 GUI 仓库不再兼容 |
| Apprise 通知 | 整个通知子系统删除（`apprise-api-server`/`recipient-url`/`notify-title`/`*-notify` 全删） |
| 远程保存方式 | 只保留 `local`；r2/gist/webdav/s3 全删，`save-method` 配置项删除 |
| sing-box 规则配置 | `singbox-latest/old`（json/js）删除；不再生成 sing-box `file` 资源 |
| mihomo 覆写 | `mihomo-overwrite-url` 删除；不再生成 mihomo `file` 资源 |
| sub-process | Sub-Store 节点操作（resolve/node-split/sort/sub-info 等）注入逻辑全删；`utils/script/` 删除；**2026-10 有限恢复：仅注入官方 `Regex Filter`（配置 `filter-regex`，id 带 `SCP.` 前缀，由 `substore/sync.go` 管理）** |
| 自更新 | `app/updater.go` 删除；`update`/`update-on-startup`/`cron-check-update`/`prerelease`/`update-timeout` 删除 |
| Sub-Store 前端 | 只保留后端；`assets/frontend/`、`scp-logo.svg`、前端托管/更新删除；**`scp-app.svg` 保留**（后端自定义图标，由 `loon_server` 的 `/scp/scp-app.svg` 路由直接提供） |
| winres | `winres/`、`//go:generate`、goreleaser 的 go-winres 钩子删除 |
| doc / docs-site | 两个目录删除；README 中相关图片/链接已清理 |
| cron | `app/cron.go` 删除；`robfig/cron` 依赖删除；`cron-expression`/`sub-store-*-cron` 配置删除 |
| GeoDB/Sub-Store 自动更新 | `UpdateGeoLite2DB`、`UpdateSubStoreAssets`、`ReloadSubStoreEngine` 删除（改为随程序内嵌 + `update_assets.ps1` 手动更新） |
| check-interval | 删除；程序**启动检测一次后常驻**，不再自动重复（周期执行交给外部调度/重启） |
| 下载测速 | **整体删除**：`speed-test-url`/`min-speed`/`download-mb`/`download-timeout`/`speed-concurrent` 配置、`check/platform/speed*.go`、`runSpeedStage`、进度测速阶段全删；流水线简化为 **测活 → 媒体**；`total-speed-limit` 保留（实为全局连接层限速，`countingConn` 消费 `Bucket`）；节点名旧 `\|x.xMB/s` 标签在改名时无条件剥离 |
| 订阅流量信息 | `/sub-info`、`subUserinfo` 生成逻辑删除；发现旧版本注入的 `display-name`/`tag`/`subUserinfo` 时，走官方 `PUT /api/subs` **一次性重建**清理 |
| max-delay | 测活延迟门限配置删除（实测本机节点池存活延迟 p50≈2s，任何绝对阈值都会清零）；延迟门限交给 `timeout`；老配置残留键被 goccy 静默忽略 |
| `CurrentCommit` | 删除；版本只由 `init.go` 的 `Version`（或 `-Version`）决定 |
| 配置迁移脚本 | `app/config_migrate.go` 删除（其迁移目标多为已删功能）；新旧键兼容改为 `app.loadConfig` 内联映射（见第 5 节 `keep-success-proxies`） |

## 5. 运行行为现状
- 启动 → **执行一次检测** → 输出 `output/`（`sub/`、`stats/`、`sub-store/` 等）→ 常驻（HTTP 订阅服务 + 内置 Sub-Store 后端）。
- 测活：`alive-check` 默认 `urltest`（mihomo `Proxy.URLTest` 同源探测，返回 ms 延迟并写 DelayHistory），可切 `http` 回退旧版 gstatic HEAD（UA/跟随重定向口径不变）；`alive-retry` 默认 1，**只复测 reset/EOF 等快速失败、超时不复测**。延迟只进 `Result.Delay` 与 Debug 日志，**不参与判定**（延迟门限交给 `timeout`：测得 delay 恒 ≤ timeout，超时即判死）。实现 `check/check.go` 的 `checkAlive`，单测 `check/alive_test.go`。
- 测速：**已整体移除**，流水线为 测活 → 媒体；`total-speed-limit` 保留为全局连接层限速（作用于所有代理连接读流量，与已删除的测速无关）；节点名旧速度标签 `|x.xMB/s` 由 `updateProxyName` 无条件剥离。
- 上游移植的新增能力（2026-10-13 合并 main 引入）：
  - `net-guard`（默认 `false`）：本机网络健康探针 `utils.NetGuard`，拥塞时收紧测活/媒体闸门（`GateAlive`/`GateMedia`；上游的测速闸门随测速一起不移植），拥塞期测活失败可复核一次（`TakeRecheck`）。
  - `sub-url-max-size-mb`（默认 50）：单个订阅下载体积上限（`proxy/fetch.go` 的 `currentMaxSubscriptionBytes`）；空响应体判 `errEmptySubscription`（permanent 失败）。
  - 历史结果加载：上游把 `keep-success-proxies` 拆成 `use-last-result` / `use-history-result` 两个键，**实际读取只看新键**；旧键 `keep-success-proxies` 在 `app.loadConfig` 归并为「两键同时开启」（示例配置仍写旧键，行为不变）。
  - 远程订阅清单为**并发拉取**（本地保留，上限 4），并按清单记录 `RemoteStat` 供分析报告的 `subs_remote` 段使用（`proxy/proxies.go` + `check/analysis_stats.go`）。
  - 节点重名治理：`utils.RenameDuplicateNodes` 在 `save.SaveConfig` 落盘前统一加序号去重（上游 fc49dfc 修复）。
- 筛选：`filter-regex`（列表）非空时，作为官方 `Regex Filter` 节点操作注入本程序推送的 `sub.process` **首位**（`filter-regex-keep` 控保留/剔除；`(?i)` 前缀忽略大小写；JS 正则、由 Sub-Store 执行），因此 **`output/sub/*` 产出文件同样会被筛掉**；本程序检测侧不筛。配置清空后下轮同步自动移除该操作。实现 `substore/sync.go` 的 `desiredProcess`，单测 `substore/filter_test.go`。
- HTTP 路由仅剩：静态订阅结果文件（`/all.yaml`、`/history.yaml`、`/base64.yaml` 与 `/base64.txt`（均指向磁盘上的 `base64.txt`）、`/mihomo.yaml`、`/singbox.json` 及 `/sub/...` 形式）、`bdg.yaml`、加密/公开分享已删除。
- 订阅产出：
  - `mihomo.yaml` = Sub-Store `?target=mihomo`（**仅 proxies，无分组/规则**）
  - `singbox.json` = Sub-Store `?target=sing-box`（**仅 outbounds/endpoints 片段**，非完整配置）
  - 均需 Sub-Store 运行；未运行时跳过。
- Sub-Store 资源：只维护 `sub`（官方最小字段 `name/displayName/source/url/content/ua/process` + `content/icon`），**不管理 process**（仅清理历史 SCP 注入）。
- `sub-store.json` 由官方 bundle 序列化（`JSON.stringify(cache, null, 2)`）；`LoonKVStore.Write` **只校验+透传字节，禁止二次 `MarshalIndent`**，否则键序会被重排成字母序、与官方不一致（`substore/loon_store.go`）。
- 顶层键顺序应为 `subs → collections → artifacts → rules → files → tokens → schemaVersion → settings → archives → modules`（集成测试 `TestSyncSubStoreOfficialDefaultShape` 断言）。
- 保存方式固定 `local`。

## 6. 资源更新（GeoLite2 / Sub-Store）
- 内嵌资源：`assets/sub-store.min.js`(+`backend.version`)、`assets/GeoLite2-Country.mmdb.zst`(+`geolite2.version`)。
- **一键更新**：
  ```powershell
  .\update_assets.ps1                 # 最新版才下；默认走 https://hk.gh-proxy.org/ 加速
  .\update_assets.ps1 -Force          # 强制重下
  .\update_assets.ps1 -SkipGeo        # 只更新 Sub-Store
  ```
  脚本：GitHub API 取最新 Release → `digest` 校验 sha256 → Sub-Store 直接覆盖；GeoLite2 用 zstd 压缩（优先 `zstd` CLI，否则 `go run` + `klauspolst/compress/zstd`，含往返校验）→ 写版本文件。
- **运行时会自动覆盖**：
  - Sub-Store：`extractAssets` 比较 `backend.version`，磁盘旧则覆盖 `output/sub-store/sub-store.min.js`。
  - GeoLite2：`assets/maxmind_db_process.go` 的 `ensureGeoLite2` 比较 `geolite2.version`，内嵌更新则覆盖 `output/MaxMindData/GeoLite2-Country.mmdb`（注意 `O_TRUNC`）。
- ⚠️ 不要手工编辑 `assets/sub-store.min.js`（那是 Sub-Store 官方产物）；要升级用上面的脚本。
- `output/sub-store/sub-store.json` 是**内置 Sub-Store 后端自行生成的 KV 存储**（首次运行创建，结构与官方默认 KV 一致），本程序只对其中的 `sub` 资源做 PATCH，不整体生成该文件。

## 7. 终端输出约定
- 日志（tint → `getStdout()`）经 `lineClearWriter` 在写入前输出 `\r\x1b[K`，确保**日志从行首左对齐**，不与进度条混行。
- 进度条（`check/progress.go` 的 `renderProgressString`）重绘前也带 `\r\x1b[K` 清行。
- 保持这两处，避免再次出现 `可用: 1009-23 …` 粘连。

## 8. Git 与远程
- 本仓库为本地 git 仓库（`master`），远程：
  - `origin = https://github.com/ANOONIMASXP/subs-check-pro`（fork，承载推送）
  - `upstream = https://github.com/sinspired/subs-check-pro.git`（真上游；**对比/同步基线 = `upstream/main`**）
  - 本地 `master` 跟踪 `origin/v3/Alpha-local`（承载推送）；历史基线已切到 `upstream/main`
  - `origin/v3/Alpha` 的内容已全部并入 `main`（上游已合并，该分支仅作历史参照）
  - 旧历史（合并 main 之前）保留在本地分支 `backup/pre-main-merge`
- 同步上游：
  ```powershell
  git fetch upstream main
  # 大改动前先备份（见第 10 节），再合并/叠加本地差分
  ```
- 推送（分支名不同，必须显式）：
  ```powershell
  git push -f origin HEAD:v3/Alpha-local   # 首次：历史已重建到 main 系，需 force
  ```
- 提交信息风格：`chore:` / `fix:` / `feat:` + 中文简述。

## 9. 常见问题
- **“卡住”**：多为 CGO 全量编译（quickjs/mihomo）或 GeoLite2 走代理下载（约 35KB/s，8.5MB 需数分钟）。`update_assets.ps1` 现有进度条与断点续传；构建请耐心等待，勿误判为死锁。
- **编辑器报一堆 undefined / import 错**：先确认 CGO 与 GOPROXY（见第 3 节），重启 gopls。
- **`/sub-info` 404 日志**：旧 Sub-Store 里残留 `subUserinfo`；程序检测到后会一次性重建清掉（见第 4 节）。
- **PowerShell 脚本语法错**：检查文件是否为 UTF-8 **带 BOM**。
- **`go test ./...` 有红**：`check/platform`、`proxy` 的联网用例失败属正常（见第 2 节）；以 `go test ./check/ ./config/ ./substore/` 为验收基线。
- **老配置含已删除的键**（`max-delay`/`min-speed`/`speed-test-url` 等）：goccy 裸 `Unmarshal` 静默忽略未知键，正常启动，行为由 `config/config_test.go` 锁定。
- **与上游对比/合并时 `merge-base` 失效、报 `refusing to merge unrelated histories`**：先查 `git rev-parse --is-shallow-repository`；shallow clone 下历史被截断，`git fetch --unshallow origin` 补全后再算。

## 10. 实战经验与避坑（2026-10 测活/筛选/测速改造沉淀）

**考证官方与依赖实现（别猜，读源码）**
- Sub-Store 官方源码走 `https://cdn.jsdelivr.net/gh/sub-store-org/Sub-Store@master/<path>`（`raw.githubusercontent.com` 被墙）；或 GitHub Contents API（返回 base64）；代码搜索用 `grep.app`。
- mihomo 依赖实现直接读本地模块缓存：`C:\Users\Anonima\go\pkg\mod\github.com\metacubex\mihomo@v1.19.30\...`（如 `adapter/adapter.go` 的 `URLTest`），比查文档快且与锁定版本一致。
- 本机 PowerShell **没有 `grep`/`rg`/`head`**：用 Kilo 的 Grep 工具或 `type`/`cat`/`ls`；带管道/分号的命令可能被权限规则拦截。

**mihomo `URLTest` 用于测活的坑**
- 状态码不符时返回 **`err == nil`**，必须用 `AliveForTestUrl(url)` 才拿得到最终判定；`utils.IntRanges` **空集 = 放行任何状态码**，期望码必须兜底构造 `[204,204]`（否则 500 也判活）。
- 内部 `http.Client{Timeout: 30s}` 硬编码 → 业务超时要用 ctx 截断 `min(config.Timeout, 30s)`。
- 不带 UA、不跟随重定向（`ErrUseLastResponse`）→ 与旧 gstatic HEAD 口径不同，判定可能翻转，**必须留 `alive-check: http` 回退开关**。
- 不加载 mihomo 配置时 `UnifiedDelay` 恒为 false（单次 HEAD，不会翻倍请求）。
- `URLTest` 走 `p.DialContext`、**绕过本程序 transport**，也不受 `job.Close()` 中断 → 需自行合并 stage ctx 与 `job.Client.ctx`。

**Go 陷阱（本仓库实踩）**
- **`isRetryable` 的 `ECONNRESET` 分支是死代码**：`syscall.Errno` 实现了 `net.Error`，`errors.AsType` 提前返回 `Timeout()`，根本走不到 reset 判断 → 测活复测必须用独立谓词 `isRetryableAlive`（显式列 reset/EOF、排除超时，避免死节点耗时翻倍）。
- `json` 的 **`omitempty` 对空 slice 也省略** → 想下发 `"process": []` 必须用 `*[]any` 指针，否则清不掉 Sub-Store 里的操作。
- 幂等比较用 `reflect.DeepEqual` 时，**构造值与 JSON 解析值类型必须对齐**（`[]string` vs `[]any` 永不相等 → 每轮重复 PATCH）。
- 删配置字段是安全的：`app/config.go` 用 goccy 裸 `Unmarshal`、全仓无 `DisallowUnknownField`，老配置残留键静默忽略（`config/config_test.go` 锁定）。

**Sub-Store 侧**
- 节点操作是**内置处理器**（官方 `core/proxy-utils/processors/index.js` 注册表），`Regex Filter` 等写死在 bundle 里，**不依赖远程脚本**；条目形状 `{id, type, args}`，本程序用 `SCP.` 前缀 id 管生命周期（每轮 strip 后按配置重建 = 幂等）。
- 正则语义：JS RegExp、**只匹配 `name`**、多条任一命中、`(?i)` 忽略大小写；Go 不执行也无法校验 JS 正则 → **非法正则会导致下载/产出失败**，只能文档警示 + 集成测试兜底。
- `PATCH /api/sub/*` 是**合并语义，删不掉已有键**；要删字段只能 `PUT /api/subs` 整体重写（见"订阅流量信息"重建逻辑）。

**测试方法**
- 无网络单测：`httptest` 注入已知延迟/速率 + `adapter.ParseProxy(map[string]any{"type":"direct"})` 构造直连节点。
- 端到端：`startTestSubStore(t)`（18973 端口 + temp 输出目录，跑完自停），用 `GET /download/sub?target=mihomo` 验证官方链路真实行为（如 `RegexFilter` 是否真执行）；同包测试串行可复用端口，`testing.Short()` 跳过。
- 真网络双口径 A/B：临时 `*_test.go` 读 `output/sub/history.yaml` 的历史可用节点对比，**用完移出仓库**，别混进测试集。

**实测基准（本机 2026-10-07/08，改前留档）**
- 60 个历史可用节点：`http` 口径活 17（28.3%）、`urltest` 口径活 20（33.3%），交叉 **http活→urltest死 = 0**（新口径不比旧口径严，反而略宽）。
- 存活节点 urltest 延迟 **p50≈2011ms / p90≈3241ms / max=5850ms**（含拨号与 200 并发自载荷）→ **任何 <1.5s 的绝对延迟阈值都会清零**；设阈值前先看分布，这是删 `max-delay` 的直接原因。
- 失败大头是 `i/o timeout`（节点连不上），6s 超时占比高 → ETA 主要由死节点超时决定。

**工作流**
- 大改前先备份：显式 `git add <文件>`（**禁止 `git add -A`**）打一个 `feat/chore` 提交，再在**仓库之外**打全量包 `tar -czf <上级>\<目录>.bak-<时间>.tar.gz -C <上级> <目录>`（含 `.git`/`bin`/`exe`，gitignore 覆盖不到），并 `tar -tzf` 抽验成员。
- 文件跨主题重叠时**合并为一个 feat 提交**：按主题拆会造出无法独立编译的中间提交（如只提 `substore` 而 `config.go` 未跟上），且 `git add -p` 交互式暂存在本环境不可用。
- 计划文件放 `.kilo/plans/`（已 gitignore），按【待执行 / 已完成 / 已取消】标注状态，同一文件可并存多份计划。
- 删功能的收尾自检：目标标识符（如 `speedON|CheckSpeed|MinSpeed`）全仓 `.go` grep 归零 + 保留白名单（如 `Bucket`/`TotalSpeedLimit`）仍在。

**上游同步（2026-10-13 合并 main 沉淀）**
- 合并方式：历史被本地重建过，无法 `git merge` → `git checkout -B master main` 后用 **净差分 3-way 应用**：`git diff --binary e87640c <本地旧tip> > patch`（必须 `cmd /c "... > file"` 原生重定向，PS 管道会把补丁写成 CRLF 导致 `git apply` 解析失败）→ `git apply -3 patch`，再逐文件解决冲突。
- `git apply -3` 的方向与直觉相反：**ours = 当前分支（main）**、**theirs = 补丁（本地）**；冲突处默认偏向 theirs（保住删减），上游新增能力按第 5 节逐项移植。
- 已删功能的文件常以「上游干净落入」方式回流（本地未改到的行会直接采用 main 版本），**不能只看冲突标记**：合并后必须对 `GlobalConfig`/`robfig`/`wails`/`appurge`/`Singbox`/`SaveMethod` 等禁用标识符全仓 grep 清零，并以 `go build` 循环收口（未定义符号 = 上游共享段引用了已删字段，按删减策略去掉该段）。
- 上游测试可能与上游自身实现脱节（如 `TestFetchOnceShortBody` 空响应体断言 vs `errEmptySubscription`）：以合并后**实际行为**为准修测试，并在提交信息里注明。
- 合并后收尾三件套：`go mod tidy`（剔除 wails/robfig/minio 等幽灵依赖）→ 死代码清理（零调用方的包装函数/类型）→ 更新本文件。
