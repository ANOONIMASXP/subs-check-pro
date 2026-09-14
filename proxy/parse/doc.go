// Package parse 对获取的订阅链接进行解码，适配标准 mihomo、v2ray，singbox 协议，和非标格式
//
//   - parse.go：解析调度与格式路由（parseSubscriptionData、parseLineBasedFormats）
//   - convert.go：各格式到 map[string]any 的具体转换实现
//   - convert_extra.go：上游暂未支持的非标协议扩展（mieru、anytls 等）
//   - normalize.go：节点字段语义修正（NormalizeNode 及相关工具函数）
//   - codec.go：编解码与 URL 工具（Base64、HostPort 分割、协议猜测）
//   - url_utils.go：URL 字符串处理（CleanURL、NormalizeGitHubRawURL、日志辅助）
//
// # 扩展指引
//
// 新增协议支持：若上游 Mihomo 尚未合并，在 convert_extra.go 的 [ConvertsV2RayExtra]
// 中添加对应 case 即可；待上游修复后直接删除临时代码，无需改动其他文件。
//
// 新增订阅格式：在 dispatch.go 的 [parseLineBasedFormats] 中追加解析器调用，
// 并在 convert.go 中实现对应的转换函数。
package parse
