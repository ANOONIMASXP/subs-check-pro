// Package proxies 负责从各类订阅源获取、解析并去重代理节点。
//
// # 整体流程
//
// 调用方只需使用 [GetProxies] 作为统一入口，内部按以下流水线执行：
//
//  1. 环境初始化：检测系统代理、GitHub 代理可用性，迁移历史文件
//  2. 订阅收集：合并本地配置、远程列表、历史存活节点，去重排序
//  3. 并发拉取：按 [config.GlobalConfig.Concurrent] 控制并发度，下载各订阅源数据
//  4. 格式解析：自动识别 Clash/Mihomo、Sing-Box、V2Ray、SSR、WireGuard 等十余种格式
//  5. 节点去重：基于协议指纹（类型 + 地址 + 端口 + 凭证 + 传输层）精确去重
//  6. 优先级合并：存活节点 > 历史节点 > 普通节点，保留最高优先级版本
//
//   - proxies.go：主流程与并发调度（GetProxies、processSubscription、resolveSubUrls）
//   - fetch.go：网络 I/O 层（FetchSubsData、fetchOnce、连接池管理）
//   - info.go：获取代理地理位置信息
//   - isp.go：获取代理地址的isp信息
//   - rename.go：重命名代理节点
//   - shuffle.go：对节点进行智能乱序
package proxies
