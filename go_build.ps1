# ==============================================================================
# Go 应用程序跨平台编译脚本 (PowerShell) - Zig 交叉编译 (支持 QuickJS CGO)
# ==============================================================================

param(
    [string]$Version,
    [switch]$Clean,
    [switch]$Debug,
    [switch]$Experiment,
    [switch]$Win,
    [switch]$Lin,
    [switch]$Android
)

# --- 1. 配置 ---
$relativeOutputDir = "bin"
$outputDir = Join-Path $PSScriptRoot $relativeOutputDir
$execName = "subs-check-pro"

# --- 2. Zig 交叉编译器设置 ---
function Set-CrossCompiler($goos, $goarch) {
    # 引入 quickjs-go 后，所有平台都必须开启 CGO
    $env:CGO_ENABLED = "1"

    # Android 使用 NDK 的 clang，不需要 Zig
    if ($goos -eq "android") {
        $ndk = $env:ANDROID_NDK_HOME
        if (-not $ndk -and (Test-Path "C:\Android\android-ndk-r27d")) {
            $ndk = "C:\Android\android-ndk-r27d"
        }
        if (-not $ndk -or -not (Test-Path $ndk)) {
            Write-Host "❌ 未找到 Android NDK，请设置环境变量 ANDROID_NDK_HOME" -ForegroundColor Red
            throw "Android NDK not found"
        }
        $api = "24"
        if ($env:ANDROID_API) { $api = $env:ANDROID_API }
        $tc = Join-Path $ndk "toolchains\llvm\prebuilt\windows-x86_64\bin"
        $env:CC = "$tc\aarch64-linux-android$api-clang.cmd"
        $env:CXX = "$tc\aarch64-linux-android$api-clang++.cmd"
        Write-Host "🔧 启用 CGO, 使用 Android NDK clang (API $api): $env:CC" -ForegroundColor Cyan
        return
    }

    # 检查 Zig 是否存在
    if (-not (Get-Command zig -ErrorAction SilentlyContinue)) {
        Write-Host "❌ Zig 未安装，无法编译开启 CGO 的目标平台！" -ForegroundColor Red
        throw "Zig not found"
    }

    switch ($goos) {
        "linux" {
            # Linux 推荐使用 musl，完美支持 -extldflags '-static' 静态链接
            switch ($goarch) {
                "amd64" { $env:CC = "zig cc -target x86_64-linux-musl"; $env:CXX = "zig c++ -target x86_64-linux-musl" }
                "arm64" { $env:CC = "zig cc -target aarch64-linux-musl"; $env:CXX = "zig c++ -target aarch64-linux-musl" }
                "arm" { $env:CC = "zig cc -target arm-linux-musleabihf"; $env:CXX = "zig c++ -target arm-linux-musleabihf" }
                default { throw "Unsupported GOARCH for Linux: $goarch" }
            }
        }
        "windows" {
            # Windows 使用 gnu target 即可
            switch ($goarch) {
                "amd64" { $env:CC = "zig cc -target x86_64-windows-gnu"; $env:CXX = "zig c++ -target x86_64-windows-gnu" }
                "386" { $env:CC = "zig cc -target x86-windows-gnu"; $env:CXX = "zig c++ -target x86-windows-gnu" }
                default { throw "Unsupported GOARCH for Windows: $goarch" }
            }
        }
        "darwin" {
            # macOS 支持
            switch ($goarch) {
                "amd64" { $env:CC = "zig cc -target x86_64-macos"; $env:CXX = "zig c++ -target x86_64-macos" }
                "arm64" { $env:CC = "zig cc -target aarch64-macos"; $env:CXX = "zig c++ -target aarch64-macos" }
                default { throw "Unsupported GOARCH for Darwin: $goarch" }
            }
        }
        default {
            throw "Unsupported GOOS: $goos"
        }
    }

    Write-Host "🔧 启用 CGO, 使用 Zig 交叉编译器: $env:CC" -ForegroundColor Cyan
}

# --- 3. 编译目标平台 ---
$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; OutputFile = "$execName.exe" },
    @{ GOOS = "windows"; GOARCH = "386"; OutputFile = "$execName.exe" },
    @{ GOOS = "linux"; GOARCH = "amd64"; OutputFile = "$execName" },
    @{ GOOS = "linux"; GOARCH = "arm64"; OutputFile = "$execName" },
    @{ GOOS = "linux"; GOARCH = "arm"; OutputFile = "$execName" },
    @{ GOOS = "darwin"; GOARCH = "amd64"; OutputFile = "$execName" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; OutputFile = "$execName" }
)

# Android 目标（需要 NDK，使用 -Android 启用）
if ($Android) {
    $Clean = $false
    $targets = @(
        @{ GOOS = "android"; GOARCH = "arm64"; OutputFile = "$execName" }
    )
    Write-Host "🤖 仅编译 Android/arm64（需 ANDROID_NDK_HOME）" -ForegroundColor Blue
}
# Debug 模式
elseif ($Debug) {
    $Clean = $false

    if ($Win -and -not $Lin) {
        # 仅 Windows
        $targets = @(
            @{ GOOS = "windows"; GOARCH = "amd64"; OutputFile = "$execName.exe" }
        )
        Write-Host "🐞 Debug 模式，仅编译 Windows/amd64" -ForegroundColor Blue
    }
    elseif ($Lin -and -not $Win) {
        # 仅 Linux
        $targets = @(
            @{ GOOS = "linux"; GOARCH = "amd64"; OutputFile = "$execName" }
        )
        Write-Host "🐞 Debug 模式，仅编译 Linux/amd64" -ForegroundColor Blue
    }
    else {
        # 默认：Windows + Linux
        $targets = @(
            @{ GOOS = "windows"; GOARCH = "amd64"; OutputFile = "$execName.exe" },
            @{ GOOS = "linux"; GOARCH = "amd64"; OutputFile = "$execName" }
        )
        Write-Host "🐞 Debug：默认 Win+Lin（单独编译用 -Win / -Lin）" -ForegroundColor Blue
    }
}

# --- 4. 开始编译 ---
Write-Host "🚀 开始交叉编译 Go 应用程序..." -ForegroundColor Cyan

# 未通过参数指定版本时，从 init.go 读取 Version
if ([string]::IsNullOrWhiteSpace($Version)) {
    $initFile = Join-Path $PSScriptRoot "init.go"
    if (Test-Path $initFile) {
        $m = Select-String -Path $initFile -Pattern '^\s*var\s+Version\s*=\s*"([^"]*)"' | Select-Object -First 1
        if ($m) {
            $Version = $m.Matches[0].Groups[1].Value
            Write-Host "🎫 从 init.go 读取版本：$Version" -ForegroundColor Cyan
        }
    }
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = "dev"
    Write-Host "🎫 未指定版本，默认为：dev"
}

if (-not (Test-Path $outputDir)) {
    New-Item -ItemType Directory -Path $outputDir | Out-Null
}

# 清理缓存
if ($Clean) {
    Write-Host "🧹 清理 Go 构建缓存..." -ForegroundColor Yellow
    Push-Location $PSScriptRoot
    go clean -cache -r
}

# 启用实验特性
if ($Experiment) {
    Write-Host "🧪 启用实验特性: JSON v2 编码器" -ForegroundColor Yellow
    $env:GOEXPERIMENT = "jsonv2"
}

$successALL = $true

try {
    foreach ($target in $targets) {
        try {
            $env:GOOS = $target.GOOS
            $env:GOARCH = $target.GOARCH

            # 设置 Zig 交叉编译器（全局生效）
            Set-CrossCompiler $target.GOOS $target.GOARCH

            # 架构映射
            switch ($target.GOARCH) {
                "amd64" { $archStr = "x86_64" }
                "386" { $archStr = "i386" }
                "arm64" { $archStr = "aarch64" }
                "arm" { $archStr = "armv7" }
                default { $archStr = $target.GOARCH }
            }

            $platformIdentifier = "{0}_{1}" -f ($target.GOOS.Substring(0, 1).ToUpper() + $target.GOOS.Substring(1)), $archStr
            $platformDir = Join-Path $outputDir $platformIdentifier
            if (-not (Test-Path $platformDir)) {
                New-Item -ItemType Directory -Path $platformDir | Out-Null
            }

            $outputPath = Join-Path $platformDir $target.OutputFile
            $archiveName = "{0}_{1}_{2}" -f $execName, ($target.GOOS.Substring(0, 1).ToUpper() + $target.GOOS.Substring(1)), $archStr
            $archiveExt = "tar.gz"
            if ($target.GOOS -eq "windows") { $archiveExt = "zip" }
            $archivePath = Join-Path $outputDir "$archiveName.$archiveExt"

            Write-Host "  -> 编译 $platformIdentifier ..." -ForegroundColor White

            # 针对不同平台的 ldflags 处理
            $ldflags = "-s -w -X main.Version=$Version"
            
            # darwin 依赖系统 libSystem；android 使用 bionic，均不做全静态链接
            if ($target.GOOS -ne "darwin" -and $target.GOOS -ne "android") {
                $ldflags += " -extldflags '-static'"
            }

            go build -ldflags $ldflags -trimpath -o $outputPath

            if ($LASTEXITCODE -ne 0) {
                Write-Host "❌ 编译 $platformIdentifier 失败！" -ForegroundColor Red
                $successALL = $false
                continue
            }

            # Docker 兼容输出
            if ($target.GOOS -eq "linux") {
                $dockerArch = switch ($target.GOARCH) {
                    "amd64" { "amd64" }
                    "arm64" { "arm64" }
                    "arm" { "arm" }
                    default { "" }
                }
                if ($dockerArch -ne "") {
                    $dockerBin = Join-Path $outputDir "$execName-linux-$dockerArch"
                    Copy-Item $outputPath $dockerBin -Force
                    Write-Host "  -> Docker 二进制: bin/$execName-linux-$dockerArch" -ForegroundColor DarkCyan
                }
            }

            if (!$Debug) {
                # 打包
                Write-Host "  -> 打包为: $relativeOutputDir/$archiveName.$archiveExt" -ForegroundColor DarkGray
                if ($target.GOOS -eq "windows") {
                    Compress-Archive -Path "$platformDir/*" -DestinationPath $archivePath -Force
                }
                else {
                    Push-Location $outputDir
                    tar -czf $archivePath $platformIdentifier
                    Pop-Location
                }
            }
        }
        finally {
            Remove-Item Env:GOOS -ErrorAction SilentlyContinue
            Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
            Remove-Item Env:CC -ErrorAction SilentlyContinue
            Remove-Item Env:CXX -ErrorAction SilentlyContinue
        }
    }
}
catch {
    $successALL = $false
    Write-Host "❌ 编译过程中出现错误: $_" -ForegroundColor Red
}
finally {
    Write-Host "🧹 清理全局环境变量..." -ForegroundColor Yellow
    Remove-Item Env:GOEXPERIMENT -ErrorAction SilentlyContinue
    Remove-Item Env:GODEBUG -ErrorAction SilentlyContinue
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

if ($successALL) {
    Write-Host "✅ 所有目标平台编译完成！" -ForegroundColor Green
    Write-Host "📂 输出文件位于 '$outputDir' 目录中。" -ForegroundColor Green
}
else {
    Write-Host "⚠️ 部分目标平台编译失败，请检查并重试。" -ForegroundColor Red
}