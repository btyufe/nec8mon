﻿#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
    nec8mon 卸载脚本 —— 干净移除，不留残留。

.DESCRIPTION
    依次完成：
      1. 停止并删除 nec8mon Windows 服务
      2. 停止并删除 nec8mon-lhm / nec8mon-open 计划任务，结束 LibreHardwareMonitor 进程
      3. 删除桌面快捷方式与防火墙规则
      4. 删除安装目录 C:\nec8mon
      5. 可选卸载 PawnIO 内核驱动（-RemovePawnIO）

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\uninstall.ps1

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -RemovePawnIO
#>
[CmdletBinding()]
param(
    [string] $InstallDir    = 'C:\nec8mon',
    [int]    $Port          = 8080,
    [switch] $RemovePawnIO,
    [switch] $KeepFiles
)

$ErrorActionPreference = 'Continue'
$ProgressPreference    = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$ServiceName = 'nec8mon'
$TaskName    = 'nec8mon-lhm'
$LogonTask   = 'nec8mon-open'
$RuleName    = "nec8mon Dashboard ($Port)"

function Step($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "    [OK]   $m" -ForegroundColor Green }
function Info($m) { Write-Host "    [--]   $m" -ForegroundColor Gray }
function Warn($m) { Write-Host "    [!!]   $m" -ForegroundColor Yellow }

# ---------------------------------------------------------------- 1. 服务
Step "停止并删除服务 $ServiceName"
$svc = Get-Service $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne 'Stopped') {
        Stop-Service $ServiceName -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }
    & sc.exe delete $ServiceName *> $null
    if ($LASTEXITCODE -eq 0) { Ok "服务已删除" } else { Warn "删除服务失败（返回 $LASTEXITCODE）" }
} else {
    Info "服务不存在"
}

# ---------------------------------------------------------------- 2. 计划任务与 LHM 进程
Step "清理计划任务 $TaskName 与 LibreHardwareMonitor"
Get-Process -Name 'LibreHardwareMonitor' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue; Ok "已结束进程 $($_.Id)" } catch { }
}
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Ok "计划任务已删除"
} else {
    Info "计划任务不存在"
}
if (Get-ScheduledTask -TaskName $LogonTask -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $LogonTask -Confirm:$false
    Ok "登录自启任务已删除"
} else {
    Info "登录自启任务不存在"
}

# ---------------------------------------------------------------- 3. 快捷方式与防火墙
Step "删除桌面快捷方式与防火墙规则"
$desktop = [Environment]::GetFolderPath('Desktop')
$commonDesktop = [Environment]::GetFolderPath('CommonDesktopDirectory')
$removedAny = $false
foreach ($d in @($desktop, $commonDesktop)) {
    if ($d -and (Test-Path $d)) {
        foreach ($name in @('NEC8 监控面板.lnk', 'NEC8 监控面板.url')) {
            $f = Join-Path $d $name
            if (Test-Path $f) {
                Remove-Item $f -Force -ErrorAction SilentlyContinue
                $removedAny = $true
            }
        }
    }
}
if ($removedAny) {
    Ok "桌面快捷方式已删除"
} else {
    Info "桌面快捷方式不存在"
}

if (Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue) {
    Get-NetFirewallRule -DisplayName $RuleName | Remove-NetFirewallRule
    Ok "防火墙规则已删除"
} else {
    Info "防火墙规则不存在"
}

# ---------------------------------------------------------------- 4. 文件
if ($KeepFiles) {
    Info "已指定 -KeepFiles，保留 $InstallDir"
} else {
    Step "删除安装目录 $InstallDir"
    if (Test-Path $InstallDir) {
        Remove-Item $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path $InstallDir) {
            Warn "目录仍存在，可能被占用，请手动删除"
        } else {
            Ok "目录已删除"
        }
    } else {
        Info "目录不存在"
    }
}

# ---------------------------------------------------------------- 5. PawnIO（可选）
if ($RemovePawnIO) {
    Step "卸载 PawnIO 内核驱动"
    $pawnExe = Join-Path $env:TEMP 'PawnIO_setup.exe'
    try {
        Invoke-WebRequest -Uri 'https://github.com/namazso/PawnIO.Setup/releases/latest/download/PawnIO_setup.exe' `
            -OutFile $pawnExe -UseBasicParsing
        $p = Start-Process -FilePath $pawnExe -ArgumentList '-uninstall', '-silent' -Wait -PassThru
        if ($p.ExitCode -eq 0) { Ok "PawnIO 已卸载" } else { Warn "PawnIO 卸载返回 $($p.ExitCode)" }
    } catch {
        Warn "PawnIO 卸载失败: $($_.Exception.Message)"
    } finally {
        Remove-Item $pawnExe -ErrorAction SilentlyContinue
    }
} else {
    Info "PawnIO 驱动已保留（其它监控软件可能仍在使用；加 -RemovePawnIO 可一并移除）"
}

Write-Host ''
Ok "卸载完成"
Write-Host ''
