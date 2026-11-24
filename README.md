# nec8mon · NEC8 / ThinkCentre 硬件监控与智能风扇调速

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go" alt="Go Version">
  <img src="https://img.shields.io/badge/Platform-Windows%2010%20%2F%2011%20(x64%2Fx86)-0078D6?style=flat&logo=windows" alt="Platform">
  <img src="https://img.shields.io/badge/Hardware-ThinkCentre%20M720q%20%2F%20NCT6686D-orange" alt="Hardware">
  <img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License">
  <img src="https://img.shields.io/badge/Tests-Passing-brightgreen" alt="Tests">
</p>

专为 **联想 ThinkCentre M720q / NEC8 等小主机（Windows 10/11 LTSC）** 深度定制的高性能硬件传感器监控与风扇智能调速工具。

通过底层内核驱动直读 Super I/O 硬件与 CPU MSR，在提供极低延迟硬件遥测的同时，支持按需对风扇进行**静音降噪调速**与**动态温度曲线控温**。

---

## 🌟 核心特性 (Features)

- 🚀 **极轻量单可执行文件**：Go 语言编写，编译后仅约 8MB，内嵌现代化响应式 Web 仪表盘；**无需**安装臃肿的第三方后台或庞大的运行时环境。
- ⚡ **硬件级毫秒遥测**：绕过迟钝受限的 Windows WMI，直接通过 PawnIO 内核驱动访问 **Nuvoton NCT6686D-L** 芯片寄存器与 **Intel MSR**，CPU 温度与风扇转速精准无误。
- ❄️ **多模式风扇调速**：
  - **自动模式**：完全不干预，风扇交还主板 BIOS 托管；
  - **手动固定模式**：滑块任意设定占空比（20% ~ 100%），即调即生效；
  - **按温度曲线模式**：根据 CPU 实时温度动态平滑调速，内置「**静音**」、「**均衡**」、「**强劲**」三套预设并支持按钮高亮反显。
- 🛡️ **双重安全防护机制**：
  - **全天候过温保护**：CPU 温度一旦超过设定阈值（默认 75°C），底层立刻强制风扇满速运转；
  - **异常退出安全兜底**：采用硬件寄存器还原点机制，即使进程被强杀或系统意外断电，下次启动优先清理残局、复位 BIOS 自动控制。
- 💾 **开机自启与配置持久化**：注册为 Windows 系统服务，随系统开机无感启动；网页中进行的任何配置实时写入本地磁盘，重启电脑设置不丢。
- 🛠️ **完善的自检与诊断工具**：
  - `-fanset`：一行命令自检任意通道风扇调速响应；
  - `-diag`：现场输出 8 大板块的深度硬件诊断报告，快速排查总线与芯片状态。

---

## 📐 系统架构 (Architecture)

```
┌──────────────────────── NEC8 / ThinkCentre M720q (Win10 LTSC) ────────────────────────┐
│                                                                                        │
│   ┌──────────────────────────┐                   ┌─────────────────────────────────┐   │
│   │   Web 监控控制台         │   HTTP / JSON     │      nec8mon 后台服务           │   │
│   │   (浏览器 / 桌面快捷方式)│◀─────────────────▶│      (Windows Service)          │   │
│   └──────────────────────────┘                   └────────────────┬────────────────┘   │
│                                                                   │ DeviceIoControl    │
│                                                                   ▼                    │
│                                                  ┌─────────────────────────────────┐   │
│                                                  │        PawnIO 内核驱动          │   │
│                                                  │ (LpcIO.bin / IntelMSR.bin 内嵌) │   │
│                                                  └────────────────┬────────────────┘   │
│                                                                   │ 端口 I/O / MSR     │
│                                                                   ▼                    │
│               ┌─────────────────────────────────────────────────────────────────┐      │
│               │ Super I/O: Nuvoton NCT6686D-L (基址: 0x0A20)                    │      │
│               │ · 温度传感器  : 0x100 … 0x114 (11 路)                           │      │
│               │ · 风扇测速    : 0x140 … 0x15E (13 位转速计数器)                  │      │
│               │ · PWM 占空比  : 0xA28 + N / 手动寄存器 0xA00                    │      │
│               │ · Intel CPU   : MSR 0x19C / 0x1B1 (封装真实温度)                │      │
│               └─────────────────────────────────────────────────────────────────┘      │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## 🚀 快速上手 (Quick Start)

### 1. 下载与一键安装

将打包好的 `dist/` 目录复制到目标主机（如 `C:\dist`），以**管理员身份打开 PowerShell** 运行：

```powershell
cd C:\dist
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

脚本将自动执行以下操作：
1. 安装主程序至 `C:\nec8mon\nec8mon.exe`；
2. 静默安装 PawnIO 内核驱动并校验就绪状态；
3. 注册 Windows 系统服务 `nec8mon`（设为开机自动启动、故障自愈）；
4. 放行防火墙端口 8080（仅限局域网子网安全访问）；
5. 生成桌面快捷方式「**NEC8 监控面板**」；
6. 自动启动服务并进行硬件传感器读取与调速自检。

### 2. 访问监控面板

- **方式 A**：双击桌面上的「**NEC8 监控面板**」快捷方式；
- **方式 B**：在局域网或本机浏览器中访问：`http://127.0.0.1:8080/`。

### 3. 日常服务管理与自动重启

```powershell
# 重启服务（修改配置或更新可执行文件后自动重启）
Restart-Service nec8mon

# 查看服务运行状态
Get-Service nec8mon

# 停止服务
Stop-Service nec8mon
```

### 4. 干净卸载

```powershell
cd C:\dist
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1
# 若需要连同内核驱动一起彻底卸载：
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -RemovePawnIO
```

---

## 🎛️ 风扇调速与监控指南

### 1. 控制模式

| 模式 | 运行逻辑 | 适用场景 |
| :--- | :--- | :--- |
| **自动 (交还 BIOS)** | 释放 Super I/O 寄存器，完全恢复主板 BIOS 原厂风扇调控策略。 | 高负载、跑分测试或排除异常时。 |
| **手动固定转速** | 拖动滑块锁定指定占空比（20% ~ 100%），即时下发至硬件。 | 日常低负载静音运行（如设为 25% ~ 30%）。 |
| **按温度曲线** | 依据 CPU 实时温度动态线性插值调速，内置回差（Hysteresis）防抖。 | 兼顾静音与散热自动平衡。 |

### 2. 曲线预设说明

- **静音预设 (Quiet)**：低温极静音，轻载转速压到最低，适合办公与轻度影音：
  `30°C → 20%` · `45°C → 22%` · `60°C → 35%` · `70°C → 55%` · `80°C → 85%` · `88°C → 100%`
- **均衡预设 (Balanced)**：平滑上升曲线，兼顾静音与散热效率：
  `35°C → 30%` · `55°C → 42%` · `70°C → 62%` · `80°C → 85%` · `90°C → 100%`
- **强劲预设 (Performance)**：激进散热策略，快速响应负载上升：
  `30°C → 50%` · `60°C → 72%` · `75°C → 90%` · `85°C → 100%`

### 3. 设置持久化

在网页上做出的任何风扇调整，都会实时保存至 `C:\nec8mon\fan-state.json`：
- **关闭浏览器网页**：风扇控制在后台持续运行；
- **重启电脑 / 重启服务**：服务启动时自动加载并恢复上次的模式与目标转速，无需二次配置。

---

## 🔬 核心技术突破与硬件踩坑纪实 (Deep-Dive)

在联想 ThinkCentre M720q（小主机）实机适配过程中，本项目彻底攻克了多个硬件底层的工程难题：

### 1. ISA 总线锁与 Win32 Mutex 跨物理线程死锁彻底根治 (v2.4.7)
- **现象**：程序长期运行后（如第二天），温度显示正常，但风扇转速无法显示，调速失效，控制模式按钮卡顿 2 秒并报错 `获取 ISA 总线锁超时`。
- **根因**：Windows 的 `Named Mutex` 严格归属于 OS 物理线程。旧版本在 `WaitForSingleObject` 返回后才调用 `LockOSThread()`；但在高频长时轮询下，若 `WaitForSingleObject` 发生微秒级阻塞，Go 的 `exitsyscall` 可能会将协程调度至另一个物理线程。随后的 `ReleaseMutex` 在非持有者线程上执行，触发 `ERROR_NOT_OWNER (288)` 静默失败，导致全局互斥量在原物理线程上发生永久死锁。
- **解法**：将 `runtime.LockOSThread()` 严格**前置**到调用 `WaitForSingleObject` 之前，确保整个等待、获取、硬件读写与释放过程 100% 绑定在同一操作系统物理线程上；并在 `Unlock()` 中校验释放结果与增加连续超时告警。

### 2. EC 总线未释放导致传感器冻结残留值 (v2.4.3)
- **现象**：传感器读数偶发全通道显示为恒定的 `8994 RPM` 与 `35.0°C`。
- **根因**：NCT6686D 芯片通过 EC 空间（端口 `0x0A24/0x0A25/0x0A26`）读写寄存器。根据芯片数据手册规范，每次单字节访问完成后，**必须向控制端口 `0x0A24` 显式写入 `0xFF` 释放 EC 总线**。若未释放，芯片内部状态机挂起，导致后续所有读取只能返回总线浮空的残余数据。
- **解法**：在 `ecAccess` 退出时严格补齐 `base+0x04 = 0xFF` 释放总线。

### 3. 联想 ThinkCentre 专有 EC 握手协议 (v2.4.5)
- **现象**：写入占空比后，芯片寄存器在 1 秒内瞬间回弹至 39%，调速无法保持。
- **根因**：公版 Nuvoton 芯片向握手寄存器 `0xA01` 写入 `0x40` 表示配置完成；但联想专有 BIOS 固件将 `0x40` 解释为“立即交还 BIOS 接管”！在 ThinkCentre 平台上，锁定手动控制的握手完成字节**必须写入 `0x00`**，交还 BIOS 时才写入 `0x40`。
- **解法**：重构联想专有握手逻辑，精准写入 `0x00` 锁定硬件接管。

### 4. Windows 快捷方式 UTF-8 BOM 解析损坏 (v2.4.6)
- **现象**：安装后桌面的快捷方式双击打不开。
- **根因**：手写 `.url` 文本快捷方式时由于附加了 UTF-8 BOM（`0xEF, 0xBB, 0xBF`），导致 Windows 底层 ANSI INI 引擎解析节名失败，丢失目标 URL。
- **解法**：改用 Windows 原生 COM 对象 `WScript.Shell` 生成标准二进制 `.lnk` 快捷方式，直连 `explorer.exe`，100% 可靠打开并配备系统级网络图标。

---

## 💻 CLI 命令行工具

### 一次性风扇调速自检 (`-fanset`)

无需启动服务或打开网页，在控制台直接接管测试指定通道的转速响应（测试 6 秒后自动交还 BIOS）：

```powershell
C:\nec8mon\nec8mon.exe -fanset 1:40
```

### 现场硬件诊断报告 (`-diag`)

生成详尽的 8 节现场诊断报告（同时自动保存为 `C:\nec8mon\nec8mon-diag.txt`）：

```powershell
C:\nec8mon\nec8mon.exe -diag
```

### 常用运行参数

| 参数 | 默认值 | 详细说明 |
| :--- | :--- | :--- |
| `-addr` | `:8080` | HTTP 服务监听地址（默认支持局域网访问，传 `127.0.0.1:8080` 仅限本机）。 |
| `-poll` | `2s` | 硬件传感器采样轮询间隔。 |
| `-fan` | `true` | 是否启用风扇调速功能（需要管理员权限与 PawnIO）。 |
| `-fan-duty` | `-1` | 手动模式初始占空比（-1 表示沿用磁盘持久化设置）。 |
| `-fan-safety`| `0` | 过温保护阈值摄氏度（0 表示沿用磁盘持久化设置）。 |
| `-fan-channel`| `-1`| 指定控制通道（-1 表示自动锁定在转的主风扇通道）。 |
| `-open` | `false`| 启动后自动使用默认浏览器打开仪表盘。 |
| `-svc` | `false`| 以 Windows 系统服务模式运行。 |
| `-diag` | `false`| 打印硬件现场诊断报告后退出。 |
| `-version` | `false`| 输出程序版本号。 |

---

## 📡 RESTful API 接口

| 路径 | 方法 | 描述 |
| :--- | :--- | :--- |
| `/` | `GET` | Web 仪表盘单页（内嵌式静态资源） |
| `/api/sensors` | `GET` | 当前全量传感器快照（CPU 温度、各通道风扇转速、占空比） |
| `/api/history` | `GET` | 历史采样环形缓冲区数据（供前端折线图绘制） |
| `/api/fan` | `GET` | 当前风扇调速状态（模式、当前通道、目标与芯片读回占空比、曲线） |
| `/api/fan/control`| `POST`| 下发调速指令（调整模式、目标占空比、曲线点集、过温阈值） |
| `/api/health` | `GET` | 服务健康检查接口 |

---

## 🛠️ 源码编译构建 (Build)

### 环境要求
- **Go 1.21+**（推荐最新版 Go）
- macOS / Linux / Windows 交叉编译环境均可

### 跨平台编译
在项目根目录运行编译脚本：

```bash
./deploy/build.sh
```

脚本将自动执行代码检查、单元测试，并生成跨平台的无依赖单文件发布包至 `dist/` 目录：
- `dist/nec8mon.exe`（Windows amd64 主程序）
- `dist/nec8mon-386.exe`（Windows 32 位备用程序）
- `dist/install.ps1`（一键安装脚本，自动注入 UTF-8 BOM）
- `dist/uninstall.ps1`（干净卸载脚本）

---

## 📂 项目文件结构

```
nec8mon/
├── main.go              # 程序入口、CLI 参数解析、生命周期管理
├── model.go             # 统一传感器数据模型与主风扇匹配算法
├── collect.go           # 采样调度、数据源状态维护、历史数据环形缓冲
├── chipdb.go            # Super I/O 芯片识别表、寄存器偏移换算（纯逻辑与单元测试）
├── pawnio_windows.go    # PawnIO 内核驱动 DeviceIoControl 协议封装
├── isabus_windows.go    # ISA 总线互斥量（与 OS 线程严格绑定）
├── superio_windows.go   # Super I/O 芯片探测、EC 总线协议与风扇调速底层读写
├── msr_windows.go       # Intel CPU MSR 封装温度读取
├── fanctl.go            # 风扇控制器：模式管理、温度曲线计算、过温保护
├── fanstate.go          # 设置持久化 (fan-state.json) 与异常恢复还原点
├── fanset.go            # -fanset 命令行调速自检
├── diag_windows.go      # -diag 现场 8 节深度诊断报告生成
├── server.go            # HTTP API 服务端与静态资源路由
├── web/index.html       # 单页 Web 仪表盘（现代化响应式 UI）
├── deploy/              # 构建与安装脚本 (build.sh / install.ps1 / uninstall.ps1)
└── dist/                # 编译成品发行目录
```

---

## 📄 许可证与鸣谢 (License & Acknowledgements)

本项目基于 [MIT 许可证](LICENSE) 开源。

特别鸣谢以下优秀开源项目与技术支持：
- [LibreHardwareMonitor](https://github.com/LibreHardwareMonitor/LibreHardwareMonitor)（硬件传感器遥测领域的标杆）
- [PawnIO](https://github.com/namazso/PawnIO)（安全可靠的用户态与内核 I/O 桥梁）
- [FanControl](https://github.com/Rem0o/FanControl.Releases)（优秀的风扇调速理念）
