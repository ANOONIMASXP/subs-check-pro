package app

import (
	"log/slog"

	"github.com/robfig/cron/v3"
	"github.com/sinspired/subs-check-pro/v3/assets"
	"github.com/sinspired/subs-check-pro/v3/config"
	"github.com/sinspired/subs-check-pro/v3/substore"
)

// SetupUpdateTasks 初始化并启动所有后台更新任务（仅在程序启动时调用一次）
func (app *App) SetupUpdateTasks() {
	if app.updateCron == nil {
		app.updateCron = cron.New()
		app.updateCron.Start()
	}

	// 注册各项定时任务
	app.UpdateGeoDBCron()
	app.UpdateSubStoreCron()
}

// UpdateGeoDBCron 独立配置 GeoLite2 数据库更新任务
func (app *App) UpdateGeoDBCron() {
	if app.updateCron == nil {
		return
	}
	if app.idGeoDB != 0 {
		app.updateCron.Remove(app.idGeoDB)
	}

	id, err := app.updateCron.AddFunc("0 12 * * 5", func() {
		if !app.checking.Load() {
			app.updateMu.Lock()
			defer app.updateMu.Unlock()
			slog.Debug("定时更新 GeoLite2 数据库...")
			if err := assets.UpdateGeoLite2DB(); err != nil {
				slog.Error("更新 GeoLite2 数据库失败", "error", err)
			}
		}
	})
	if err != nil {
		slog.Error("注册 GeoLite2 更新任务失败", "error", err)
		return
	}

	app.idGeoDB = id
	if entry := app.updateCron.Entry(id); entry.Valid() {
		slog.Debug("设置 GeoLite2 数据库更新 任务", "next", app.formatNextRunTime(entry.Next, app.updateCron.Location()))
	}
}

// UpdateSubStoreCron 独立配置 Sub-Store 更新任务
func (app *App) UpdateSubStoreCron() {
	if app.updateCron == nil {
		return
	}
	if app.idSubStore != 0 {
		app.updateCron.Remove(app.idSubStore)
	}

	subStoreSchedule := config.GlobalConfig.SubStoreUpdateCron
	if subStoreSchedule == "" {
		subStoreSchedule = "0 12 * * 5"
	}

	id, err := app.updateCron.AddFunc(subStoreSchedule, func() {
		if !app.checking.Load() {
			app.updateMu.Lock()
			defer app.updateMu.Unlock()

			slog.Debug("定时检查并更新 Sub-Store 前后端...")
			// 接收更新结果对象
			result, err := substore.UpdateSubStoreAssets()
			if err != nil {
				slog.Error("更新 Sub-Store 失败", "error", err)
				return
			}

			if result != nil && result.UpdatedBackend {
				slog.Info("Sub-Store 更新成功", "后端", result.NewBackendVer)
			}
		}
	})
	if err != nil {
		slog.Error("注册 Sub-Store 更新任务失败", "error", err)
		return
	}

	app.idSubStore = id
	if entry := app.updateCron.Entry(id); entry.Valid() {
		slog.Debug("设置 Sub-Store 资源更新 任务", "next", app.formatNextRunTime(entry.Next, app.updateCron.Location()))
	}
}
