<#
.SYNOPSIS
  一键更新内嵌资源：
    1) assets/sub-store.min.js  + assets/backend.version
    2) assets/GeoLite2-Country.mmdb.zst + assets/geolite2.version

.DESCRIPTION
  通过 GitHub API 获取最新 Release，校验 sha256 后写入 assets；
  GeoLite2 会 zstd 压缩（优先 zstd CLI，否则用仓库 Go 依赖并做往返校验）。
  默认：本地版本已是最新且文件存在 → 跳过；加 -Force 可强制重新下载。

.PARAMETER Proxy
  GitHub 加速前缀，默认 https://hk.gh-proxy.org/；传空字符串 "" 表示直连。

.PARAMETER SkipSubStore / SkipGeo
  跳过对应资源。

.PARAMETER Force
  忽略本地版本对比，强制重新下载。

.EXAMPLE
  .\update_assets.ps1
  .\update_assets.ps1 -Proxy ""
  .\update_assets.ps1 -SkipGeo
  .\update_assets.ps1 -Force -SkipSubStore

.NOTES
  可选环境变量 GITHUB_TOKEN：提升 GitHub API 速率限制（未认证 60 次/小时）。
#>
param(
    [string]$Proxy = "https://hk.gh-proxy.org/",
    [switch]$SkipSubStore,
    [switch]$SkipGeo,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
$repoRoot  = $PSScriptRoot
$assetsDir = Join-Path $repoRoot "assets"
$tmpDir    = Join-Path ([System.IO.Path]::GetTempPath()) ("scp-assets-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

function Write-Step([string]$msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Ok([string]$msg)   { Write-Host "    $msg" -ForegroundColor Green }
function Write-Skip([string]$msg) { Write-Host "    跳过: $msg" -ForegroundColor DarkYellow }

function Get-LatestRelease([string]$repo) {
    $headers = @{ "User-Agent" = "subs-check-pro-assets" }
    if ($env:GITHUB_TOKEN) { $headers["Authorization"] = "Bearer $($env:GITHUB_TOKEN)" }
    return Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers $headers -TimeoutSec 60
}

function Get-AssetUrl([string]$url) {
    if ([string]::IsNullOrWhiteSpace($Proxy)) { return $url }
    return ($Proxy.TrimEnd("/") + "/" + $url)
}

function Get-AssetSha([object]$asset) {
    if ($null -eq $asset.digest) { return "" }
    return ($asset.digest -replace "^sha256:", "")
}

function Get-LocalVersion([string]$file) {
    if (Test-Path -LiteralPath $file) { return (Get-Content -LiteralPath $file -Raw).Trim() }
    return ""
}

function Invoke-Download([string]$url, [string]$outFile, [string]$sha256Expected) {
    $full = Get-AssetUrl $url
    Write-Host "    下载: $full" -ForegroundColor DarkGray
    $sw = [System.Diagnostics.Stopwatch]::StartNew()

    # 先尝试断点续传；若服务端不支持 range 再完整重下
    & curl.exe -L --fail --retry 5 --retry-all-errors --retry-delay 2 -C - --progress-bar -o $outFile $full
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    (续传失败，改为完整下载)" -ForegroundColor DarkYellow
        & curl.exe -L --fail --retry 5 --retry-all-errors --retry-delay 2 --progress-bar -o $outFile $full
        if ($LASTEXITCODE -ne 0) { throw "下载失败: $url" }
    }
    $sw.Stop()

    if (-not [string]::IsNullOrWhiteSpace($sha256Expected)) {
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $outFile).Hash.ToLower()
        if ($actual -ne $sha256Expected.ToLower()) {
            throw "sha256 校验失败: 期望 $sha256Expected, 实际 $actual"
        }
    }

    $size = (Get-Item -LiteralPath $outFile).Length
    Write-Host ("    完成: {0:N0} bytes, {1:N1}s" -f $size, $sw.Elapsed.TotalSeconds) -ForegroundColor DarkGray
}

function Compress-Zstd([string]$inFile, [string]$outFile) {
    $zstd = Get-Command zstd -ErrorAction SilentlyContinue
    if ($zstd) {
        & $zstd.Source -19 -f -o $outFile $inFile
        if ($LASTEXITCODE -ne 0) { throw "zstd 压缩失败" }
        return
    }

    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "未找到 zstd 命令行工具，也未找到 go，无法压缩 GeoLite2"
    }

    $helper = Join-Path $repoRoot "zz_zstd_tmp.go"
    $src = @'
//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	out, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	enc, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		panic(err)
	}
	if _, err := enc.Write(raw); err != nil {
		panic(err)
	}
	if err := enc.Close(); err != nil {
		panic(err)
	}
	if err := out.Close(); err != nil {
		panic(err)
	}

	comp, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	dec, err := zstd.NewReader(bytes.NewReader(comp))
	if err != nil {
		panic(err)
	}
	defer dec.Close()
	back, err := io.ReadAll(dec)
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(back, raw) {
		panic("round-trip mismatch")
	}
	fmt.Printf("ok: %d -> %d bytes\n", len(raw), len(comp))
}
'@
    Set-Content -LiteralPath $helper -Value $src -Encoding UTF8
    try {
        Push-Location $repoRoot
        & go run zz_zstd_tmp.go $inFile $outFile
        if ($LASTEXITCODE -ne 0) { throw "go run 压缩失败" }
    }
    finally {
        Pop-Location
        Remove-Item -LiteralPath $helper -Force -ErrorAction SilentlyContinue
    }
}

try {
    # ---------------- Sub-Store ----------------
    if (-not $SkipSubStore) {
        Write-Step "检查 Sub-Store 后端脚本"
        $rel = Get-LatestRelease "sub-store-org/Sub-Store"
        $asset = $rel.assets | Where-Object { $_.name -eq "sub-store.min.js" } | Select-Object -First 1
        if (-not $asset) { throw "未在最新 Release 中找到 sub-store.min.js" }

        $ver = ($rel.tag_name -replace "^v", "")
        $target = Join-Path $assetsDir "sub-store.min.js"
        $verFile = Join-Path $assetsDir "backend.version"

        if (-not $Force -and (Get-LocalVersion $verFile) -eq $ver -and (Test-Path -LiteralPath $target)) {
            Write-Skip "sub-store.min.js 已是最新 ($ver)"
        }
        else {
            $tmp = Join-Path $tmpDir "sub-store.min.js"
            Invoke-Download $asset.browser_download_url $tmp (Get-AssetSha $asset)
            Copy-Item -LiteralPath $tmp -Destination $target -Force
            Set-Content -LiteralPath $verFile -Value $ver -NoNewline -Encoding ASCII
            Write-Ok "sub-store.min.js -> $ver"
        }
    }

    # ---------------- GeoLite2 ----------------
    if (-not $SkipGeo) {
        Write-Step "检查 GeoLite2-Country.mmdb"
        $rel = Get-LatestRelease "mojolabs-id/GeoLite2-Database"
        $asset = $rel.assets | Where-Object { $_.name -eq "GeoLite2-Country.mmdb" } | Select-Object -First 1
        if (-not $asset) { throw "未在最新 Release 中找到 GeoLite2-Country.mmdb" }

        $ver = ($rel.tag_name -replace "^v", "")
        $zst = Join-Path $assetsDir "GeoLite2-Country.mmdb.zst"
        $verFile = Join-Path $assetsDir "geolite2.version"

        if (-not $Force -and (Get-LocalVersion $verFile) -eq $ver -and (Test-Path -LiteralPath $zst)) {
            Write-Skip "GeoLite2 已是最新 ($ver)"
        }
        else {
            $tmp = Join-Path $tmpDir "GeoLite2-Country.mmdb"
            Invoke-Download $asset.browser_download_url $tmp (Get-AssetSha $asset)
            Compress-Zstd $tmp $zst
            Set-Content -LiteralPath $verFile -Value $ver -NoNewline -Encoding ASCII
            Write-Ok "GeoLite2-Country.mmdb.zst -> $ver"
        }
    }

    Write-Host ""
    Write-Host "完成。建议执行: go build ./... 验证" -ForegroundColor Yellow
}
finally {
    Remove-Item -LiteralPath $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
