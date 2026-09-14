// Package app: errors.go — 应用级别可传播错误哨兵值
package app

import "errors"

// ErrFirstRun 首次运行错误哨兵值。
//
// 当 config.yaml 不存在时，Initialize() 会创建默认配置文件并返回此错误。
// 调用方（main）应打印提示信息后 os.Exit(0)。
//
// 由于 Initialize() 内部用 fmt.Errorf("%w", ...) 包装，
// 调用方需使用 errors.Is(err, app.ErrFirstRun) 判断。
var ErrFirstRun = errors.New("first-run: config.yaml already created, please edit and restart")
