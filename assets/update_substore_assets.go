package assets

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/sinspired/subs-check-pro/v2/config"
	"github.com/sinspired/subs-check-pro/v2/utils"
)

// SubStoreUpdateResult 包含了 Sub-Store 资产更新的结果信息
type SubStoreUpdateResult struct {
	UpdatedBackend  bool
	UpdatedFrontend bool
	NewBackendVer   string
	NewFrontendVer  string
}

// 进度条追踪器
type progressReader struct {
	io.Reader
	total    int64
	current  int64
	title    string
	lastStr  string
	finished bool
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.Reader.Read(p)
	pr.current += int64(n)
	pr.printProgress(err == io.EOF)
	return n, err
}

func (pr *progressReader) printProgress(isEOF bool) {
	if !config.GlobalConfig.PrintProgress || pr.total <= 0 || pr.finished {
		return
	}
	percent := float64(pr.current) / float64(pr.total) * 100
	if percent > 100 {
		percent = 100
	}

	barWidth := 40
	barFilled := int(percent / 100 * float64(barWidth))
	bar := strings.Repeat("=", barFilled)
	if barFilled < barWidth {
		bar += ">" + strings.Repeat(" ", barWidth-barFilled-1)
	}

	curKB := pr.current / 1024
	totKB := pr.total / 1024

	str := fmt.Sprintf("\r%s: [%s] %.1f%% (%dKB/%dKB)", pr.title, bar, percent, curKB, totKB)
	if str != pr.lastStr {
		fmt.Print(str)
		pr.lastStr = str
	}

	// 结束时换行，防止吃掉后面的日志
	if isEOF || pr.current >= pr.total {
		fmt.Println()
		pr.finished = true
	}
}

// subStoreUpdater 封装了显式指定代理的 HTTP 客户端
type subStoreUpdater struct {
	client      *http.Client
	useSysProxy bool
}

// newSubStoreUpdater 创建并初始化更新器，显式指定代理规则
func newSubStoreUpdater() *subStoreUpdater {
	transport := &http.Transport{}
	useProxy := utils.GetSysProxy()

	if useProxy {
		proxyStr := config.GlobalConfig.SystemProxy
		proxyURL, err := url.Parse(proxyStr)
		if err != nil {
			slog.Error("解析配置中的代理 URL 失败，将不使用代理", "proxy_url", proxyStr, "error", err)
			transport.Proxy = nil
			useProxy = false
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	} else {
		transport.Proxy = nil
	}

	return &subStoreUpdater{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		useSysProxy: useProxy,
	}
}

// UpdateSubStoreAssets 检查并自动更新 Sub-Store 前后端，返回更新结果
func UpdateSubStoreAssets() (*SubStoreUpdateResult, error) {
	paths, err := getSubStorePaths()
	if err != nil {
		return nil, fmt.Errorf("获取路径失败: %w", err)
	}

	updater := newSubStoreUpdater()
	result := &SubStoreUpdateResult{}

	// 1. 检查并更新后端
	if tag, dlURL, err := updater.getLatestRelease("sub-store-org/Sub-Store", "sub-store.bundle.js"); err == nil {
		localVer := parseVersion(getLocalJSVersion(paths.jsPath))
		remoteVer := parseVersion(tag)

		if remoteVer != nil && (localVer == nil || remoteVer.GreaterThan(localVer)) {
			slog.Info("发现 Sub-Store 后端新版本，正在下载...", "local", localVer, "remote", tag)
			if err := updater.downloadFile(dlURL, paths.jsPath, "下载后端"); err == nil {
				slog.Info("Sub-Store 已更新后端文件", "version", tag)
				result.UpdatedBackend = true
				result.NewBackendVer = tag
			} else {
				slog.Error("下载 Sub-Store 后端失败", "error", err)
			}
		}
	} else {
		slog.Error("获取 Sub-Store 后端版本失败", "error", err)
	}

	// 2. 检查并更新前端
	if ftag, fdlURL, err := updater.getLatestRelease("sub-store-org/Sub-Store-Front-End", "dist.zip"); err == nil {
		localFVerBytes, _ := os.ReadFile(filepath.Join(paths.frontDir, "frontend.version"))
		localFVer := parseVersion(string(localFVerBytes))
		remoteFVer := parseVersion(ftag)

		if remoteFVer != nil && (localFVer == nil || remoteFVer.GreaterThan(localFVer)) {
			slog.Info("发现 Sub-Store 前端新版本，正在解压...", "local", localFVer, "remote", ftag)
			if err := updater.extractRemoteZipToPath(fdlURL, paths.frontDir, "下载前端"); err == nil {
				_ = os.WriteFile(filepath.Join(paths.frontDir, "frontend.version"), []byte(ftag), 0o644)
				slog.Info("Sub-Store 前端已更新", "version", ftag)
				result.UpdatedFrontend = true
				result.NewFrontendVer = ftag
			} else {
				slog.Error("更新 Sub-Store 前端失败", "error", err)
			}
		}
	} else {
		slog.Error("获取 Sub-Store 前端版本失败", "error", err)
	}

	// 移除了 utils.SendNotifySubStoreAssets，交由外部处理
	return result, nil
}

// getLatestRelease 智能获取代理后的下载地址，包含 API 请求防挂回退
func (u *subStoreUpdater) getLatestRelease(repo string, assetName string) (string, string, error) {
	apiBase := "https://api.github.com"
	if config.GlobalConfig.GithubAPIMirror != "" {
		apiBase = strings.TrimRight(config.GlobalConfig.GithubAPIMirror, "/")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, repo)

	resp, err := u.client.Get(apiURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
		}

		// 回退机制 1：系统代理请求 API 失败，尝试放弃代理直连 API
		if u.useSysProxy {
			slog.Warn("系统代理请求 GitHub API 失败，尝试无代理直连...", "error", errMsg)
			cleanClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}
			resp, err = cleanClient.Get(apiURL)
			if err != nil {
				return "", "", err
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return "", "", fmt.Errorf("GitHub API 直连请求仍失败: %d", resp.StatusCode)
			}
		} else {
			return "", "", fmt.Errorf("GitHub API 请求失败: %s", errMsg)
		}
	}
	defer resp.Body.Close()

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}

	for _, asset := range rel.Assets {
		if asset.Name == assetName {
			// 返回原始下载直链，让 doGetWithFallback 方法去决定拼接逻辑
			return rel.TagName, asset.BrowserDownloadURL, nil
		}
	}
	return "", "", fmt.Errorf("未找到对应的资源文件: %s", assetName)
}

// doGetWithFallback 发起带回退机制的 HTTP 下载请求
func (u *subStoreUpdater) doGetWithFallback(rawURL string) (*http.Response, error) {
	targetURL := rawURL

	// 策略：如果没有使用系统代理，且配置了 GithubProxy，则拼接加速前缀
	if !u.useSysProxy && config.GlobalConfig.GithubProxy != "" {
		targetURL = config.GlobalConfig.GithubProxy + rawURL
	}

	// 尝试一：通过已设定的网络环境发起请求
	resp, err := u.client.Get(targetURL)
	if err == nil && resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	} else {
		errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		resp.Body.Close() // 失败响应顺手关掉，防止泄露
	}

	// 回退机制 2：如果启用了系统代理但下载失败，丢弃系统代理并改用 GithubProxy 直连尝试
	if u.useSysProxy && config.GlobalConfig.GithubProxy != "" {
		slog.Warn("系统代理下载失败，尝试回退至 GithubProxy...", "error", errMsg)

		fallbackURL := config.GlobalConfig.GithubProxy + rawURL

		// 构建干净的无代理客户端，防止坏掉的系统代理继续干涉
		cleanClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}

		resp, err = cleanClient.Get(fallbackURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		if err != nil {
			return nil, fmt.Errorf("回退至 GithubProxy 依然失败: %w", err)
		}
		resp.Body.Close()
		return nil, fmt.Errorf("回退至 GithubProxy 状态码异常: %d", resp.StatusCode)
	}

	return nil, fmt.Errorf("下载请求失败: %s", errMsg)
}

func (u *subStoreUpdater) downloadFile(rawURL, path, title string) error {
	resp, err := u.doGetWithFallback(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// 使用 progressReader 包装器显示下载进度
	pr := &progressReader{
		Reader: resp.Body,
		total:  resp.ContentLength,
		title:  title,
	}

	_, err = io.Copy(outFile, pr)
	return err
}

func (u *subStoreUpdater) extractRemoteZipToPath(rawURL string, targetDir string, title string) error {
	resp, err := u.doGetWithFallback(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 同样使用 progressReader 包装，读取完毕后再传给 Zip
	pr := &progressReader{
		Reader: resp.Body,
		total:  resp.ContentLength,
		title:  title,
	}

	zipData, err := io.ReadAll(pr)
	if err != nil {
		return err
	}

	zipReader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}

	_ = os.RemoveAll(targetDir)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	cleanTargetDir := filepath.Clean(targetDir) + string(os.PathSeparator)

	for _, f := range zipReader.File {
		if !strings.HasPrefix(f.Name, "dist/") {
			continue
		}
		rel := strings.TrimPrefix(f.Name, "dist/")
		if rel == "" {
			continue
		}

		targetPath := filepath.Join(targetDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(targetPath, cleanTargetDir) {
			return fmt.Errorf("非法的文件路径穿越: %s", targetPath)
		}

		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0755)
		outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
