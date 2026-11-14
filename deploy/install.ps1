#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
    nec8mon 一键安装脚本（NEC8 / Windows 10 LTSC）。

.DESCRIPTION
    依次完成：
      1. 安装 nec8mon.exe 到 C:\nec8mon
      2. 安装 PawnIO 内核驱动（读硬件与调速风扇所必需）
      3. 注册 nec8mon Windows 服务（开机自启）
      4. 创建桌面快捷方式「NEC8 监控面板」，登录后一键打开本机仪表盘
      5. 放行防火墙端口（仅 -Expose 时，默认只监听本机）
      6. 运行 nec8mon -diag 现场诊断并展示结果

    从 2.1 版起 nec8mon 自带 PawnIO 模块，能直接读取 Super I/O 芯片的
    温度与风扇转速，不再依赖 LibreHardwareMonitor 常驻。
    如果还想要显卡、内存、每核心温度等额外传感器，可加 -WithLHM。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\install.ps1

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\install.ps1 -Port 8090

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\install.ps1 -WithLHM
#>
[CmdletBinding()]
param(
    [string] $InstallDir     = 'C:\nec8mon',
    [int]    $Port           = 8080,
    [int]    $LhmPort        = 8085,
    [string] $LhmVersion     = 'v0.9.6',
    [string] $RemoteAddress  = 'LocalSubnet',
    [switch] $LocalOnly,
    [switch] $Expose,
    [switch] $SkipPawnIO,
    [switch] $WithLHM,
    [switch] $SkipFirewall,
    [switch] $SkipShortcut
)

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$ServiceName  = 'nec8mon'
$TaskName     = 'nec8mon-lhm'
$LogonTask    = 'nec8mon-open'
$RuleName     = "nec8mon Dashboard ($Port)"
$LhmUrl       = "https://github.com/LibreHardwareMonitor/LibreHardwareMonitor/releases/download/$LhmVersion/LibreHardwareMonitor.zip"
$PawnIOUrl    = 'https://github.com/namazso/PawnIO.Setup/releases/latest/download/PawnIO_setup.exe'

# 默认开放局域网访问（用于局域网内其它设备查看与调速，传 -LocalOnly 仅限本机）
$BindAddr = if ($LocalOnly) { "127.0.0.1:$Port" } else { ":$Port" }
$NeedFirewall = -not $LocalOnly

function Step($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "    [OK]   $m" -ForegroundColor Green }
function Info($m) { Write-Host "    [--]   $m" -ForegroundColor Gray }
function Warn($m) { Write-Host "    [!!]   $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "    [ERR]  $m" -ForegroundColor Red; exit 1 }

# ---------------------------------------------------------------- 0. 前置检查
Step "检查环境"
$id  = [Security.Principal.WindowsIdentity]::GetCurrent()
$pr  = New-Object Security.Principal.WindowsPrincipal($id)
if (-not $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Die "需要管理员权限运行（右键以管理员身份运行 PowerShell，或加 -RunAsAdministrator）"
}
Info "系统: $((Get-CimInstance Win32_OperatingSystem).Caption)"
Info "架构: $env:PROCESSOR_ARCHITECTURE"

# 服务注册的命令行不含任何引号转义，安装路径必须没有空格
if ($InstallDir -match '\s') {
    Die "安装路径不能包含空格（与服务注册的命令行解析冲突）：$InstallDir"
}

# 定位 nec8mon.exe：优先脚本同级，其次 ../dist
$exeCandidate = @(
    (Join-Path $PSScriptRoot 'nec8mon.exe'),
    (Join-Path $PSScriptRoot '..\dist\nec8mon.exe')
) | Where-Object { Test-Path $_ } | Select-Object -First 1

if (-not $exeCandidate) {
    Die "找不到 nec8mon.exe，请把 dist\nec8mon.exe 放到脚本同级目录后重试"
}
Info "安装包: $exeCandidate"

# ---------------------------------------------------------------- 1. 目录与主程序
Step "安装 nec8mon"
# 若服务正在运行，先停止，避免文件占用导致覆盖失败
$runningSvc = Get-Service $ServiceName -ErrorAction SilentlyContinue
if ($runningSvc -and $runningSvc.Status -eq 'Running') {
    Info "检测到正在运行的 $ServiceName 服务，先停止以便覆盖更新…"
    Stop-Service $ServiceName -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
}
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $exeCandidate (Join-Path $InstallDir 'nec8mon.exe') -Force
Ok "nec8mon.exe -> $InstallDir"

# ---------------------------------------------------------------- 2. PawnIO 驱动
if (-not $SkipPawnIO) {
    Step "安装 PawnIO 内核驱动（读取硬件与调速风扇所必需）"
    $pawnExe = Join-Path $env:TEMP 'PawnIO_setup.exe'
    try {
        Info "下载 $PawnIOUrl"
        Invoke-WebRequest -Uri $PawnIOUrl -OutFile $pawnExe -UseBasicParsing
        Info "静默安装中…"
        $p = Start-Process -FilePath $pawnExe -ArgumentList '-install', '-silent' -Wait -PassThru
        if ($p.ExitCode -eq 0) {
            Ok "PawnIO 安装完成"
        } else {
            Warn "PawnIO 安装返回码 $($p.ExitCode)，若已安装可忽略"
        }
    } catch {
        Warn "PawnIO 安装失败: $($_.Exception.Message)"
        Warn "可手动运行 $PawnIOUrl 下载后以 -install -silent 安装"
    } finally {
        Remove-Item $pawnExe -ErrorAction SilentlyContinue
    }
} else {
    Info "已跳过 PawnIO 安装"
}

# 校验 PawnIO 是否真的装上了 —— 没有它就读不到 Super I/O 芯片。
$pawnKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PawnIO'
if (-not (Test-Path $pawnKey)) {
    $pawnKeyWow = 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\PawnIO'
    if (Test-Path $pawnKeyWow) { $pawnKey = $pawnKeyWow }
}
if (Test-Path $pawnKey) {
    $pawnVer = (Get-ItemProperty $pawnKey -ErrorAction SilentlyContinue).DisplayVersion
    Ok "PawnIO 已就绪（版本 $pawnVer）"
} else {
    Warn "未检测到 PawnIO！没有它就读不到 Super I/O 芯片的温度与风扇转速。"
    Warn "请手动安装：https://github.com/namazso/PawnIO.Setup/releases/latest/download/PawnIO_setup.exe"
    Warn "命令行：PawnIO_setup.exe -install -silent"
}

# ---------------------------------------------------------------- 3. LibreHardwareMonitor（可选）
$lhmDir = Join-Path $InstallDir 'lhm'
if ($WithLHM) {
    Step "部署 LibreHardwareMonitor（可选的补充数据源）"
    $zip = Join-Path $env:TEMP 'LibreHardwareMonitor.zip'
    try {
        Info "下载 $LhmUrl"
        Invoke-WebRequest -Uri $LhmUrl -OutFile $zip -UseBasicParsing
        if (Test-Path $lhmDir) { Remove-Item $lhmDir -Recurse -Force }
        New-Item -ItemType Directory -Force -Path $lhmDir | Out-Null
        Expand-Archive -Path $zip -DestinationPath $lhmDir -Force
        Ok "LibreHardwareMonitor -> $lhmDir"
    } catch {
        Die "下载/解压 LibreHardwareMonitor 失败: $($_.Exception.Message)"
    } finally {
        Remove-Item $zip -ErrorAction SilentlyContinue
    }

    # 预置配置：开启内置 Web 服务器、静默驻留托盘
    Step "写入 LibreHardwareMonitor 配置"
    $cfg = @"
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <appSettings>
    <add key="runWebServerMenuItem" value="true" />
    <add key="listenerIp" value="127.0.0.1" />
    <add key="listenerPort" value="$LhmPort" />
    <add key="authenticationEnabled" value="false" />
    <add key="startMinMenuItem" value="true" />
    <add key="minTrayMenuItem" value="true" />
    <add key="minCloseMenuItem" value="true" />
    <add key="logSensorsMenuItem" value="false" />
    <add key="theme" value="auto" />
  </appSettings>
</configuration>
"@
    $cfgPath = Join-Path $lhmDir 'LibreHardwareMonitor.config'
    [IO.File]::WriteAllText($cfgPath, $cfg, (New-Object Text.UTF8Encoding($false)))
    Ok "配置已写入 $cfgPath"
} else {
    Info "跳过 LibreHardwareMonitor（默认不需要，nec8mon 自己就能读 Super I/O 芯片）"
    Info "如需显卡 / 内存 / 每核心温度等额外传感器，重跑本脚本并加 -WithLHM"
}

# ---------------------------------------------------------------- 4. Windows 服务
Step "注册 Windows 服务 $ServiceName"
$exePath = Join-Path $InstallDir 'nec8mon.exe'
# 拆成 "binPath=" 与取值两个参数传入 sc.exe，与 cmd 下 `sc create n binPath= "..."` 的解析方式一致。
# 安装路径不含空格，因此无需任何引号转义。
$binPathValue = "$exePath -svc -addr $BindAddr -poll 2s"

if ($WithLHM) {
    # 让 nec8mon 服务在 LHM 未运行时能自愈拉起它（会先检查同名进程，避免与计划任务重复启动）。
    $lhmExePath = Join-Path $lhmDir 'LibreHardwareMonitor.exe'
    if (Test-Path $lhmExePath) {
        $binPathValue += " -with-lhm -lhm http://127.0.0.1:$LhmPort -lhm-exe $lhmExePath"
    }
}

& sc.exe query $ServiceName *> $null
if ($LASTEXITCODE -eq 0) {
    Info "检测到已存在的服务，先删除"
    & sc.exe stop  $ServiceName *> $null
    & sc.exe delete $ServiceName *> $null
    Start-Sleep -Seconds 2
}

& sc.exe create $ServiceName 'binPath=' $binPathValue 'start=' 'auto' 'obj=' 'LocalSystem' 'DisplayName=' 'nec8mon Sensor Monitor (CPU Temp / Fan RPM)' | Out-Null
if ($LASTEXITCODE -ne 0) { Die "创建服务失败（sc.exe 返回 $LASTEXITCODE）" }
Ok "服务已创建"

& sc.exe failure $ServiceName 'reset=' '86400' 'actions=' 'restart/5000/restart/15000/restart/60000' | Out-Null
& sc.exe failureflag $ServiceName '1' | Out-Null

# 服务描述用 CIM 写入，避免 sc.exe 的 ANSI 代码页把中文变成问号
try {
    $svcWmi = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'" -ErrorAction Stop
    if ($svcWmi) {
        Set-CimInstance -InputObject $svcWmi -Property @{ Description = 'NEC8 传感器监控：CPU 温度与风扇转速的局域网仪表盘' } -ErrorAction Stop | Out-Null
        Ok "服务描述已设置"
    }
} catch {
    & sc.exe description $ServiceName 'nec8mon sensor monitor: CPU temperature and fan speed dashboard' | Out-Null
}

# ---------------------------------------------------------------- 5. LHM 计划任务（仅在启用 LHM 时）
if ($WithLHM) {
    Step "注册计划任务 $TaskName（开机启动 LibreHardwareMonitor）"
    $lhmExe = Join-Path $lhmDir 'LibreHardwareMonitor.exe'
    if (-not (Test-Path $lhmExe)) { Die "找不到 $lhmExe" }

    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue

    $action = New-ScheduledTaskAction -Execute $lhmExe -WorkingDirectory $lhmDir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet `
        -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries `
        -StartWhenAvailable `
        -RestartCount 5 `
        -RestartInterval (New-TimeSpan -Minutes 1) `
        -MultipleInstances IgnoreNew `
        -ExecutionTimeLimit ([TimeSpan]::Zero)

    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Principal $principal -Settings $settings `
        -Description '开机启动 LibreHardwareMonitor，为 nec8mon 补充额外传感器' -Force | Out-Null
    Ok "计划任务已创建（SYSTEM 权限，崩溃后自动重启）"
} else {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    if (Test-Path $lhmDir) { Remove-Item $lhmDir -Recurse -Force -ErrorAction SilentlyContinue }
    # 旧版本可能还留着运行中的 LibreHardwareMonitor 进程，一并结束。
    # 它会占用 ISA 总线并加载 PawnIO 模块，与本程序抢硬件访问权。
    Get-Process -Name 'LibreHardwareMonitor' -ErrorAction SilentlyContinue | ForEach-Object {
        try {
            Stop-Process -Id $_.Id -Force -ErrorAction Stop
            Info "已结束残留的 LibreHardwareMonitor 进程 (PID $($_.Id))"
        } catch { }
    }
    Info "已清理旧的 LibreHardwareMonitor 计划任务、目录与进程"
}

# ---------------------------------------------------------------- 6. 防火墙
if ($NeedFirewall -and -not $SkipFirewall) {
    Step "放行防火墙端口 $Port（局域网安全访问）"
    Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow `
        -Protocol TCP -LocalPort $Port -Profile Any -RemoteAddress $RemoteAddress | Out-Null
    Ok "已放行（来源 $RemoteAddress）"
} elseif (-not $SkipFirewall) {
    Info "已按 -LocalOnly 仅监听 127.0.0.1，不开放防火墙端口"
}

# ---------------------------------------------------------------- 7. 桌面快捷方式 + 登录自启
if (-not $SkipShortcut) {
    Step "创建桌面快捷方式「NEC8 监控面板」"
    $desktop = [Environment]::GetFolderPath('Desktop')
    $commonDesktop = [Environment]::GetFolderPath('CommonDesktopDirectory')
    foreach ($d in @($desktop, $commonDesktop)) {
        if ($d -and (Test-Path $d)) {
            $oldUrl = Join-Path $d 'NEC8 监控面板.url'
            if (Test-Path $oldUrl) { Remove-Item $oldUrl -Force -ErrorAction SilentlyContinue }
        }
    }

    $lnkPath = Join-Path $desktop 'NEC8 监控面板.lnk'
    $url = "http://127.0.0.1:$Port/"
    try {
        $wsh = New-Object -ComObject WScript.Shell
        $lnk = $wsh.CreateShortcut($lnkPath)
        $lnk.TargetPath = "$env:SystemRoot\explorer.exe"
        $lnk.Arguments = $url
        $lnk.IconLocation = "$env:SystemRoot\System32\shell32.dll,14"
        $lnk.Description = "NEC8 传感器监控面板 (CPU 温度 / 风扇转速)"
        $lnk.Save()
        [System.Runtime.InteropServices.Marshal]::ReleaseComObject($wsh) | Out-Null
        Ok "已创建 $lnkPath"
    } catch {
        Warn "创建快捷方式失败: $_"
    }

    # 登录时自动弹出面板：远程桌面连进来就能直接看到温度与风扇转速
    Step "注册计划任务 $LogonTask（用户登录后打开面板）"
    Unregister-ScheduledTask -TaskName $LogonTask -Confirm:$false -ErrorAction SilentlyContinue
    $logonAction = New-ScheduledTaskAction -Execute 'rundll32.exe' `
        -Argument "url.dll,FileProtocolHandler http://127.0.0.1:$Port/"
    $logonTrigger = New-ScheduledTaskTrigger -AtLogOn
    $logonSettings = New-ScheduledTaskSettingsSet `
        -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries `
        -StartWhenAvailable `
        -MultipleInstances IgnoreNew `
        -ExecutionTimeLimit ([TimeSpan]::Zero)
    Register-ScheduledTask -TaskName $LogonTask -Action $logonAction -Trigger $logonTrigger `
        -Settings $logonSettings `
        -Description '用户登录后自动打开 nec8mon 本机监控面板' -Force | Out-Null
    Ok "计划任务已创建"
}

# ---------------------------------------------------------------- 8. 启动
Step "启动服务"
Start-Service $ServiceName -ErrorAction SilentlyContinue
Start-Sleep -Seconds 4
$svc = Get-Service $ServiceName -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq 'Running') {
    Ok "服务状态: $($svc.Status)"
} else {
    Warn "服务未处于 Running，请执行：Get-Service $ServiceName"
}

if ($WithLHM) {
    Start-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Info "已触发计划任务，等待 LibreHardwareMonitor 就绪…"
}

# ---------------------------------------------------------------- 9. 验证
Step "验证"
$ok = $false
for ($i = 0; $i -lt 15; $i++) {
    Start-Sleep -Seconds 2
    try {
        $r = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/sensors" -TimeoutSec 3
        if ($r.readings.Count -gt 0) {
            $ok = $true
            $cpu = if ($r.cpuTemperature) { "$($r.cpuTemperature.value) $($r.cpuTemperature.unit) / $($r.cpuTemperature.name)" } else { '无' }
            $fan = if ($r.fan) { "$($r.fan.value) $($r.fan.unit) / $($r.fan.name)" } else { '无' }
            Ok "CPU 温度: $cpu"
            Ok "风扇转速: $fan"
            Ok "共 $($r.readings.Count) 条传感器读数"
            break
        }
    } catch { }
}
if (-not $ok) {
    Warn "暂未取到传感器读数。下面的诊断输出会指出问题所在。"
}

# 调速能力检测
try {
    $f = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/fan" -TimeoutSec 3
    if ($f.available) {
        Ok "风扇调速: 可用（$($f.backend.chip)）"
    } else {
        Warn "风扇调速: 不可用 —— $($f.backend.detail)"
        Warn "调速依赖 PawnIO 驱动与受支持的 Super I/O 芯片；查看温度与转速不受影响。"
    }
} catch {
    Warn "无法读取调速状态：$($_.Exception.Message)"
}

# 现场诊断：这一段输出直接反映了芯片识别与传感器读取的全过程
Step "现场诊断（nec8mon -diag）"
try {
    & (Join-Path $InstallDir 'nec8mon.exe') -diag 2>&1 | ForEach-Object { Write-Host "    $_" }
} catch {
    Warn "诊断命令执行失败：$($_.Exception.Message)"
}
$diagFile = Join-Path $InstallDir 'nec8mon-diag.txt'
if (Test-Path $diagFile) {
    Ok "诊断报告已保存到 $diagFile（可直接回传该文件）"
}

Write-Host ''
Ok "安装完成并已自动启动/重启 nec8mon 服务"
Write-Host "    本机访问: http://127.0.0.1:$Port" -ForegroundColor White
$lanIP = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
       Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254*' } |
       Select-Object -First 1).IPAddress
if ($lanIP) {
    Write-Host "    局域网访问: http://${lanIP}:$Port" -ForegroundColor Cyan
}
Write-Host "    JSON API: http://127.0.0.1:$Port/api/sensors" -ForegroundColor White
Write-Host "    诊断:     C:\nec8mon\nec8mon.exe -diag" -ForegroundColor White
Write-Host "    调速自检: C:\nec8mon\nec8mon.exe -fanset 1:40" -ForegroundColor White
Write-Host "    卸载:     powershell -ExecutionPolicy Bypass -File .\uninstall.ps1" -ForegroundColor White
Write-Host ''
Write-Host "    提示：仪表盘下方的「风扇通道明细」会标出哪个通道正在转，" -ForegroundColor DarkGray
Write-Host "          调速时在「风扇通道」里选那个通道即可。" -ForegroundColor DarkGray
Write-Host ''
