[CmdletBinding()]
param(
    [ValidateSet('Start', 'Stop', 'Pack', 'PackClean', 'PackFull')]
    [string]$Action = 'Start',
    [switch]$NoBrowser
)

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($PSScriptRoot).TrimEnd('\')
$configPath = Join-Path $root 'config.json'
$packageDirectory = Join-Path $root 'packages'
Set-Location -LiteralPath $root

function Get-Executables {
    @(Get-ChildItem -LiteralPath $root -Filter 'gemini-web2api-go*.exe' -File)
}

function Get-ApplicationProcesses {
    @(Get-Process -Name 'gemini-web2api-go*' -ErrorAction SilentlyContinue | Where-Object {
        try {
            $_.Path -and [IO.Path]::GetDirectoryName($_.Path).Equals($root, [StringComparison]::OrdinalIgnoreCase)
        } catch { $false }
    })
}

function Read-Configuration {
    if (-not (Test-Path -LiteralPath $configPath)) {
        Write-Host '首次运行：请设置管理面板密码，至少 12 个字符。输入不会显示。'
        do {
            $secure = Read-Host '管理面板密码' -AsSecureString
            $password = (New-Object Net.NetworkCredential('', $secure)).Password
            if ($password.Length -lt 12) { Write-Host '密码长度不足，请重新输入。' }
        } while ($password.Length -lt 12)
        $initial = [ordered]@{
            host = '127.0.0.1'; port = 8083; admin_enabled = $true
            admin_token = $password; default_model = 'gemini-4-argon'
        }
        [IO.File]::WriteAllText($configPath, ($initial | ConvertTo-Json), (New-Object Text.UTF8Encoding($false)))
        $password = $null
        Write-Host '已创建本机配置；之后启动不会覆盖它。'
    }
    try { $config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json }
    catch { throw 'config.json 不是合法 JSON。请检查引号、逗号和 UTF-8 编码。' }
    $port = 8083
    if ($null -ne $config.port) { $port = [int]$config.port }
    if ($port -lt 1 -or $port -gt 65535) { throw 'config.json 中的端口无效。' }
    if ($config.admin_enabled -eq $false) { throw '管理面板已关闭。请在 config.json 中启用 admin_enabled。' }
    return $config
}

function Test-Ready([string]$BaseUrl) {
    try {
        $request = [Net.HttpWebRequest]::Create($BaseUrl + '/')
        $request.Proxy = $null
        $request.Timeout = 1000
        $request.ReadWriteTimeout = 1000
        $response = $request.GetResponse()
        try {
            $reader = New-Object IO.StreamReader($response.GetResponseStream())
            try { $health = $reader.ReadToEnd() | ConvertFrom-Json } finally { $reader.Dispose() }
            return $health.status -eq 'ok' -and $null -ne $health.version
        } finally { $response.Dispose() }
    } catch { return $false }
}

function Start-ServiceAndPanel {
    $config = Read-Configuration
    $port = 8083
    if ($null -ne $config.port) { $port = [int]$config.port }
    $listenHost = [string]$config.host
    if ([string]::IsNullOrWhiteSpace($listenHost) -or $listenHost -in @('0.0.0.0', '::', '[::]')) { $listenHost = '127.0.0.1' }
    if ($listenHost.Contains(':') -and -not $listenHost.StartsWith('[')) { $listenHost = '[' + $listenHost + ']' }
    $baseUrl = 'http://{0}:{1}' -f $listenHost, $port
    $panelUrl = $baseUrl + '/admin'
    $running = @(Get-ApplicationProcesses)
    $listeners = @(Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue)
    foreach ($listener in $listeners) {
        if ($listener.OwningProcess -notin @($running | Select-Object -ExpandProperty Id)) {
            throw "端口 $port 已被其他程序占用，未启动新实例。"
        }
    }
    if ($running.Count -gt 1) { throw '本目录已有多个服务进程，请先用 stop.bat 停止后再启动。' }
    $startedHere = $false
    if ($running.Count -eq 0) {
        $architecture = 'amd64'
        if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $architecture = 'arm64' }
        $candidates = @(Get-Executables | Where-Object { $_.Name -notmatch '_windows_(amd64|arm64)\.exe$' -or $_.Name.EndsWith("_windows_$architecture.exe") })
        $exe = $candidates | Sort-Object @{Expression = {
            if ($_.Name -match '_v(\d+(?:\.\d+)+)_') { [version]$Matches[1] } else { [version]'0.0' }
        }; Descending = $true} | Select-Object -First 1
        if (-not $exe) { throw "本目录没有适合 $architecture 的 gemini-web2api-go 程序。" }
        $dataDirectory = Join-Path $root 'data'
        [void](New-Item -ItemType Directory -Path $dataDirectory -Force)
        $env:TIKTOKEN_CACHE_DIR = Join-Path $dataDirectory 'tokenizer-cache'
        $service = Start-Process -FilePath $exe.FullName -ArgumentList @('--config', ('"{0}"' -f $configPath)) -WorkingDirectory $root -WindowStyle Hidden -RedirectStandardOutput (Join-Path $dataDirectory 'launcher.stdout.log') -RedirectStandardError (Join-Path $dataDirectory 'launcher.stderr.log') -PassThru
        $startedHere = $true
        Write-Host '服务正在启动，等待就绪……'
    } else {
        $service = $running[0]
        Write-Host '检测到本目录服务已运行，复用现有进程。'
    }
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    while (-not (Test-Ready $baseUrl)) {
        $service.Refresh()
        if ($service.HasExited) { throw '服务已退出。请查看 data\launcher.stderr.log，原配置未改动。' }
        if ([DateTime]::UtcNow -ge $deadline) {
            if ($startedHere -and -not $service.HasExited) { Stop-Process -InputObject $service -Force }
            throw '60 秒内服务未就绪。请检查配置和 data 下的启动日志。'
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "服务已就绪：$panelUrl"
    if (-not $NoBrowser) {
        try { Start-Process -FilePath $panelUrl }
        catch { throw "服务已启动，但无法打开默认浏览器。请手动访问 $panelUrl" }
    }
}

function Stop-Application {
    $running = @(Get-ApplicationProcesses)
    if ($running.Count -eq 0) { Write-Host '本目录没有运行中的服务。'; return }
    foreach ($service in $running) {
        Stop-Process -InputObject $service -Force
        [void]$service.WaitForExit(5000)
    }
    Write-Host '已停止本目录服务，配置和数据均已保留。'
}

function Get-PackageFiles([bool]$IncludeData) {
    if (-not $IncludeData) {
        foreach ($name in @('start.bat', 'stop.bat', 'package.bat', 'launcher.ps1', 'config.example.json', 'README.txt')) {
            $path = Join-Path $root $name
            if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "缺少打包所需文件：$name" }
            Get-Item -LiteralPath $path
        }
        $executables = @(Get-Executables)
        if ($executables.Count -eq 0) { throw '没有可打包的 gemini-web2api-go 程序。' }
        $executables
        return
    }
    if (@(Get-ApplicationProcesses).Count -gt 0) {
        throw '完整打包前请先运行 stop.bat，避免账号数据库仍在写入。'
    }
    if (Test-Path -LiteralPath $configPath) {
        try { $config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json }
        catch { throw '配置 JSON 无效，无法确认完整包的数据范围。' }
        foreach ($field in @('db_path', 'cookie_file')) {
            $value = [string]$config.$field
            if (-not [string]::IsNullOrWhiteSpace($value)) {
                if ([IO.Path]::IsPathRooted($value)) {
                    throw "配置项 $field 使用绝对路径，换电脑后可能失效。请改为本目录内的相对路径再完整打包。"
                }
                $fullPath = [IO.Path]::GetFullPath((Join-Path $root $value))
                if (-not $fullPath.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase)) {
                    throw "配置项 $field 指向本目录之外，无法做完整便携包。请先调整该配置。"
                }
            }
        }
    }
    $directories = New-Object 'Collections.Generic.Queue[string]'
    $directories.Enqueue($root)
    while ($directories.Count -gt 0) {
        foreach ($item in Get-ChildItem -LiteralPath $directories.Dequeue() -Force) {
            if ($item.FullName.Equals($packageDirectory, [StringComparison]::OrdinalIgnoreCase)) { continue }
            if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '目录中存在链接，完整打包已停止以避免包含外部文件。' }
            if ($item.PSIsContainer) { $directories.Enqueue($item.FullName) } else { $item }
        }
    }
}

function New-SharePackage([bool]$IncludeData) {
    $files = @(Get-PackageFiles $IncludeData)
    [void](New-Item -ItemType Directory -Path $packageDirectory -Force)
    $mode = 'clean'
    if ($IncludeData) { $mode = 'full-private' }
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
    $destination = Join-Path $packageDirectory ("gemini-web2api-go-$mode-$stamp.zip")
    $partialPath = $destination + '.partial'
    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::Open($partialPath, [IO.Compression.ZipArchiveMode]::Create)
    try {
        foreach ($file in $files) {
            if ($file.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '不打包符号链接或目录连接。' }
            $relative = $file.FullName.Substring($root.Length + 1).Replace('\', '/')
            [void][IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip, $file.FullName, ('gemini-web2api-go/' + $relative), [IO.Compression.CompressionLevel]::Optimal)
        }
    } finally {
        $zip.Dispose()
    }
    [IO.File]::Move($partialPath, $destination)
    if ($IncludeData) { Write-Host '已生成完整包：包含账号登录态、密钥、配置和日志，请仅交给你明确授权的人。' }
    else { Write-Host '已生成仅程序包：不包含 config.json、账号数据库、Cookie 或现有密钥。接收方首次运行时设置自己的密码。' }
    Write-Host $destination
}

$hashProvider = [Security.Cryptography.SHA256]::Create()
try { $lockId = ([BitConverter]::ToString($hashProvider.ComputeHash([Text.Encoding]::UTF8.GetBytes($root.ToLowerInvariant())))).Replace('-', '') }
finally { $hashProvider.Dispose() }
$mutex = New-Object Threading.Mutex($false, ('Local\GeminiWeb2API-' + $lockId))
$locked = $false
$resultCode = 0
try {
    try { $locked = $mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $locked = $true }
    if (-not $locked) { throw '另一个启动或打包操作正在进行，请稍后再试。' }
    switch ($Action) {
        'Start' { Start-ServiceAndPanel }
        'Stop' { Stop-Application }
        'PackClean' { New-SharePackage $false }
        'PackFull' { New-SharePackage $true }
        'Pack' {
            Write-Host '1. 仅程序（默认）：不包含账号数据、密钥和你的配置。'
            Write-Host '2. 完整打包：包含配置、账号登录态、API Key 和日志。'
            $choice = Read-Host '请选择 1 或 2，直接回车选择 1'
            if ($choice -eq '2') { New-SharePackage $true }
            elseif ([string]::IsNullOrWhiteSpace($choice) -or $choice -eq '1') { New-SharePackage $false }
            else { throw '选项无效，没有生成压缩包。' }
        }
    }
} catch {
    Write-Host ('操作未完成：' + $_.Exception.Message) -ForegroundColor Red
    $resultCode = 1
} finally {
    if ($locked) { $mutex.ReleaseMutex() }
    $mutex.Dispose()
}
exit $resultCode
