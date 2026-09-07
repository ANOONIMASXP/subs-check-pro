package assets

import (
	"archive/zip"
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

	curKB, totKB := pr.current/1024, pr.total/1024
	// \033[K 用于清除当前行光标后的内容，防止字符残留
	str := fmt.Sprintf("\r\033[K%s: [%s] %.1f%% (%dKB/%dKB)", pr.title, bar, percent, curKB, totKB)

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
	proxyClient  *http.Client
	directClient *http.Client
	useSysProxy  bool
}

// newSubStoreUpdater 创建并初始化更新器，显式指定代理规则
func newSubStoreUpdater() *subStoreUpdater {
	// 继承 DefaultTransport 的优良特性(超时、连接池等)，而不是用空结构体
	directTransport := http.DefaultTransport.(*http.Transport).Clone()
	directTransport.Proxy = nil

	proxyTransport := directTransport.Clone()
	useSysProxy := utils.GetSysProxy()

	if useSysProxy {
		if proxyURL, err := url.Parse(config.GlobalConfig.SystemProxy); err == nil && proxyURL.String() != "" {
			proxyTransport.Proxy = http.ProxyURL(proxyURL)
		} else {
			slog.Warn("代理 URL 解析失败或为空，退化为直连", "url", config.GlobalConfig.SystemProxy)
			useSysProxy = false
			proxyTransport.Proxy = nil
		}
	}

	return &subStoreUpdater{
		proxyClient:  &http.Client{Transport: proxyTransport, Timeout: 30 * time.Second},
		directClient: &http.Client{Transport: directTransport, Timeout: 30 * time.Second},
		useSysProxy:  useSysProxy,
	}
}

// doRequest 统一封装带 fallback 机制的 HTTP 请求，并自动注入 Token
func (u *subStoreUpdater) doRequest(targetURL string) (*http.Response, error) {
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}

	// 注入 Token (仅官方域名)
	if token := config.GlobalConfig.GithubToken; token != "" {
		if host := strings.ToLower(req.URL.Host); strings.HasSuffix(host, "github.com") || strings.HasSuffix(host, "githubusercontent.com") {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	// 策略 1: 走默认 Client (代理或直连)
	client := u.directClient
	if u.useSysProxy {
		client = u.proxyClient
	}

	resp, err := client.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	if resp != nil {
		resp.Body.Close()
	}

	// 策略 2: Fallback (如果启用了系统代理且失败了，尝试强制直连)
	if u.useSysProxy {
		slog.Warn("系统代理请求失败，尝试无代理直连...", "url", targetURL, "error", err)
		resp, err = u.directClient.Do(req) // 复用 req 对象
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	return nil, fmt.Errorf("请求失败 (url: %s, err: %v)", targetURL, err)
}

// 核心更新逻辑

// UpdateSubStoreAssets 检查并自动更新 Sub-Store 前后端
func UpdateSubStoreAssets() (*SubStoreUpdateResult, error) {
	paths, err := getSubStorePaths()
	if err != nil {
		return nil, fmt.Errorf("获取路径失败: %w", err)
	}

	updater := newSubStoreUpdater()
	result := &SubStoreUpdateResult{}

	// 更新后端
	result.UpdatedBackend, result.NewBackendVer = updater.updateComponent(
		"后端", "sub-store-org/Sub-Store", "sub-store.bundle.js", getLocalJSVersion(paths.jsPath),
		func(dlURL string) error { return updater.downloadFile(dlURL, paths.jsPath, "下载后端") },
	)

	// 更新前端
	localFVerBytes, _ := os.ReadFile(filepath.Join(paths.frontDir, "frontend.version"))
	result.UpdatedFrontend, result.NewFrontendVer = updater.updateComponent(
		"前端", "sub-store-org/Sub-Store-Front-End", "dist.zip", string(localFVerBytes),
		func(dlURL string) error {
			if err := updater.extractRemoteZipToPath(dlURL, paths.frontDir, "下载前端"); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(paths.frontDir, "frontend.version"), []byte(result.NewFrontendVer), 0644)
		},
	)

	return result, nil
}

// updateComponent 抽象的通用组件更新流
func (u *subStoreUpdater) updateComponent(name, repo, assetName, localVerRaw string, downloadAction func(string) error) (bool, string) {
	tag, dlURL, err := u.getLatestRelease(repo, assetName)
	if err != nil {
		slog.Error(fmt.Sprintf("获取 Sub-Store %s 版本失败", name), "error", err)
		return false, ""
	}

	localVer, remoteVer := parseVersion(localVerRaw), parseVersion(tag)
	if remoteVer == nil || (localVer != nil && !remoteVer.GreaterThan(localVer)) {
		return false, "" // 无需更新
	}

	slog.Info(fmt.Sprintf("Sub-Store %s 有新版", name), "local", localVer, "remote", tag)

	// 处理加速代理 URL
	if !u.useSysProxy {
		dlURL = utils.WarpURL(dlURL, utils.GetGhProxy())
	}

	if err := downloadAction(dlURL); err != nil {
		slog.Error(fmt.Sprintf("更新 Sub-Store %s 失败", name), "error", err)
		return false, ""
	}

	slog.Info(fmt.Sprintf("Sub-Store %s 已更新", name), "version", tag)
	return true, tag
}

// getLatestRelease 智能获取代理后的下载地址，包含 API 请求防挂回退
func (u *subStoreUpdater) getLatestRelease(repo string, assetName string) (string, string, error) {
	apiBase := "https://api.github.com"
	if config.GlobalConfig.GithubAPIMirror != "" {
		apiBase = strings.TrimRight(config.GlobalConfig.GithubAPIMirror, "/")
	}
	apiURL := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, repo)

	resp, err := u.doRequest(apiURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}

	for _, asset := range rel.Assets {
		if asset.Name == assetName {
			return rel.TagName, asset.URL, nil
		}
	}
	return "", "", fmt.Errorf("未找到对应的资源文件: %s", assetName)
}

// downloadFile 文件下载函数
func (u *subStoreUpdater) downloadFile(rawURL, path, title string) error {
	resp, err := u.doRequest(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	outFile, err := os.Create(path)
	if err != nil {
		return err
	}
	defer outFile.Close()

	// 显示下载进度
	_, err = io.Copy(outFile, &progressReader{Reader: resp.Body, total: resp.ContentLength, title: title})
	return err
}

// extractRemoteZipToPath 下载并解压
func (u *subStoreUpdater) extractRemoteZipToPath(rawURL string, targetDir string, title string) error {
	resp, err := u.doRequest(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 将下载流写入临时文件，避免将整个 ZIP 文件读入内存(防止低配设备 OOM)
	tmpFile, err := os.CreateTemp("", "substore-front-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	_, err = io.Copy(tmpFile, &progressReader{Reader: resp.Body, total: resp.ContentLength, title: title})
	tmpFile.Close() // 必须先关闭文件写入
	if err != nil {
		return fmt.Errorf("下载 ZIP 失败: %w", err)
	}

	// 从磁盘打开 ZIP 进行流式解压
	zipReader, err := zip.OpenReader(tmpName)
	if err != nil {
		return fmt.Errorf("解析 ZIP 失败: %w", err)
	}
	defer zipReader.Close()

	_ = os.RemoveAll(targetDir)
	cleanTargetDir := filepath.Clean(targetDir) + string(os.PathSeparator)

	for _, f := range zipReader.File {
		if !strings.HasPrefix(f.Name, "dist/") || strings.TrimPrefix(f.Name, "dist/") == "" {
			continue
		}

		targetPath := filepath.Join(targetDir, filepath.FromSlash(strings.TrimPrefix(f.Name, "dist/")))
		if !strings.HasPrefix(targetPath, cleanTargetDir) {
			return fmt.Errorf("非法的文件路径穿越: %s", targetPath)
		}

		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0755)
		if err := extractZipFile(f, targetPath); err != nil {
			return err
		}
	}
	return nil
}

// extractZipFile 辅助函数，确保 defer 能及时释放文件句柄
func extractZipFile(f *zip.File, targetPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, rc)
	return err
}
