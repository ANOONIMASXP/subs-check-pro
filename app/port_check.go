package app

import (
	"github.com/sinspired/subs-check-pro/v3/config"
)

// CheckPortConflict 检查HTTP和sub-store端口冲突
//
// 需先初始化配置加载
//
// err := app.InitConfigLoad()
func (app *App) CheckPortConflict() (httpPortAvailable bool, subStorePortAvailable bool) {
	if config.Get().ListenPort == "" {
		if err := app.loadConfig(); err != nil {
			return false, false
		}
	}

	if config.Get().ListenPort != "" {
		listenAddr := normalizeListenAddr(config.Get().ListenPort)
		if checkPortFree(listenAddr) {
			httpPortAvailable = true
		} else {
			httpPortAvailable = false
		}
	} else {
		httpPortAvailable = true
	}

	if config.Get().SubStorePort != "" {
		subStoreAddr := normalizeListenAddr(config.Get().SubStorePort)
		// 不再依赖外部 Node 环境，移除 Linux 386 架构限制，直接检查端口
		if checkPortFree(subStoreAddr) {
			subStorePortAvailable = true
		} else {
			subStorePortAvailable = false
		}
	} else {
		subStorePortAvailable = true
	}

	return httpPortAvailable, subStorePortAvailable
}
