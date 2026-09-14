package app

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"github.com/sinspired/subs-check-pro/v3/check"
)

// reportFallback 从分析报告中提取的兜底数据
type reportFallback struct {
	trafficTotalRaw    uint64    // check_info.check_traffic_total_raw（字节）
	trafficUploadRaw   uint64    // check_info.check_traffic_upload_raw（字节）
	trafficDownloadRaw uint64    // check_info.check_traffic_download_raw（字节）
	checkEndTime       time.Time // check_info.check_end_time_raw
}

const (
	// totalBytes = 1024 GiB，以字节计
	// 1 GiB = 1024^3 = 1 073 741 824 bytes
	// 1024 GiB = 1 099 511 627 776 bytes
	totalBytes int64 = 1024 * 1_073_741_824

	// expireUnix = 2077-06-01 00:00:00 UTC
	// 赛博朋克儿童节 🎉
	expireUnix int64 = 3389731200 // time.Date(2077,6,1,0,0,0,0,time.UTC).Unix()

	planName = "Subs-Check-Pro"
	appURL   = "https://github.com/sinspired/subs-check-pro"
)

// registerSubscriptionInfoRoute 注册公共订阅信息路由（无需鉴权）。
func (app *App) registerSubscriptionInfoRoute(router *gin.Engine) {
	router.GET(SubInfoPath, app.handleSubscriptionInfo)
}

// handleSubscriptionInfo 公共路由，构建最新订阅信息字符串，写入 subscription-userinfo 响应头，
func (app *App) handleSubscriptionInfo(c *gin.Context) {
	info := buildSubscriptionInfo()

	c.Header("subscription-userinfo", info)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, info)
}

// buildSubscriptionInfo 组装符合代理客户端规范的订阅信息字符串。
//
// 字段说明：
//   - upload / download  来自 check.UP / check.DOWN（原子计数器，单位 bytes）
//   - total              固定 1024 TiB，1 PB
//   - expire             固定 2077-06-01 UTC Unix 时间戳
//   - reset_hour         距下次重置不足 1 天时显示，值为重置时刻的小时数
//   - reset_day          距下次重置超过 1 天时显示，值为剩余整天数
//   - next_update        下次重置的格式化时间（始终显示）
//   - last_update        上次检测完成时间（check.CheckEndTime）
func buildSubscriptionInfo() string {
	now := time.Now()
	upload := check.UP.Load()
	download := check.DOWN.Load()

	// 兜底：程序重启后原子计数器归零，从历史报告中补充
	var fb reportFallback
	if upload == 0 && download == 0 || check.CheckEndTime.IsZero() {
		fb = loadReportFallback()
	}

	// 流量：有实时值用实时值，否则用报告中的历史流量
	if upload == 0 && fb.trafficUploadRaw > 0 {
		upload = fb.trafficUploadRaw
	}

	if download == 0 && fb.trafficDownloadRaw > 0 {
		download = fb.trafficDownloadRaw
	}

	// last_update：有检测结束时间用检测结束时间，否则从报告中取，再否则用当前时间占位
	var lastUpdate string
	switch {
	case !check.CheckEndTime.IsZero():
		lastUpdate = check.CheckEndTime.Format(LogTimeFormat)
	case !fb.checkEndTime.IsZero():
		lastUpdate = fb.checkEndTime.Format(LogTimeFormat)
	default:
		lastUpdate = now.Format(LogTimeFormat)
	}

	next := calcNextResetTime(now)
	remaining := next.Sub(now)
	nextUpdate := next.Format(LogTimeFormat)

	var resetField string
	if remaining < 24*time.Hour {
		resetField = "reset_hour=" + strconv.Itoa(next.Hour())
	} else {
		resetDays := int(remaining.Hours() / 24)
		resetField = "reset_day=" + strconv.Itoa(resetDays)
	}

	var b strings.Builder

	writeKV(&b, "upload", strconv.FormatUint(upload, 10))
	writeKV(&b, "download", strconv.FormatUint(download, 10))
	writeKV(&b, "total", strconv.FormatInt(totalBytes, 10))
	writeKV(&b, "expire", strconv.FormatInt(expireUnix, 10))

	b.WriteString(resetField)
	b.WriteString("; ")

	writeKV(&b, "next_update", nextUpdate)
	writeKV(&b, "last_update", lastUpdate)

	b.WriteString("plan_name='")
	b.WriteString(planName)
	b.WriteString("'; ")

	writeKV(&b, "app_url", appURL)

	return b.String()
}

func writeKV(b *strings.Builder, key string, val string) {
	b.WriteString(key)
	b.WriteString("=")
	b.WriteString(val)
	b.WriteString("; ")
}

// calcNextResetTime 计算下次流量重置的绝对时间（固定按 24 小时）。
func calcNextResetTime(now time.Time) time.Time {
	return now.Add(24 * time.Hour)
}

// loadReportFallback 读取最新分析报告，提取流量与结束时间。
func loadReportFallback() reportFallback {
	reportPath, err := AnalysisReportPath()
	if err != nil {
		return reportFallback{}
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		return reportFallback{}
	}

	// 只解析用到的字段，避免引入完整结构体
	var doc struct {
		CheckInfo struct {
			CheckTrafficTotalRaw    uint64 `yaml:"check_traffic_total_raw"`
			CheckTrafficUploadRaw   uint64 `yaml:"check_traffic_upload_raw"`
			CheckTrafficDownloadRaw uint64 `yaml:"check_traffic_download_raw"`
			CheckEndTimeRaw         string `yaml:"check_end_time_raw"`
		} `yaml:"check_info"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return reportFallback{}
	}

	fb := reportFallback{
		trafficTotalRaw:    doc.CheckInfo.CheckTrafficTotalRaw,
		trafficUploadRaw:   doc.CheckInfo.CheckTrafficUploadRaw,
		trafficDownloadRaw: doc.CheckInfo.CheckTrafficDownloadRaw,
	}

	if raw := doc.CheckInfo.CheckEndTimeRaw; raw != "" {
		// check_end_time_raw 为 RFC3339 格式：2026-03-17T02:18:17+08:00
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			fb.checkEndTime = t
		}
	}
	return fb
}
