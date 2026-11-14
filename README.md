# nec8mon · NEC8 传感器监控与风扇调速

在 **NEC8（联想 ThinkCentre M720q，Windows 10 LTSC）** 本机查看
**CPU 温度** 与 **风扇转速**，并**调节风扇转速**（冬天或低负载时降速降噪）。

Go 编写、交叉编译成 **单个 exe**，无运行时依赖；自带 Web 仪表盘，
远程桌面登录后在 NEC8 上打开 `http://127.0.0.1:8080` 即可。

```
┌──────────────────────── NEC8 (Win10 LTSC) ────────────────────────┐
│                                                                   │
│   ┌──────────┐    DeviceIoControl    ┌───────────────────────┐   │
│   │  NEC8    │◀─────────────────────▶│    PawnIO 内核驱动     │   │
│   │  仪表盘  │                        └───────────┬───────────┘   │
│   │ (浏览器) │                                    │ 端口 I/O       │
│   └────┬─────┘                                    ▼                │
│        │            ┌───────────────────────────────────────┐     │
│        │            │  Super I/O: Nuvoton NCT6686D-L         │     │
│        │            │  · 温度传感器  0x100 … 0x114           │     │
│        │            │  · 风扇转速    0x140 … 0x15E           │     │
│        │            │  · PWM 占空比  0xA28 / 0xA00          │     │
│        │            └───────────────────────────────────────┘     │
│        │                    读温度 / 读转速 / 写占空比            │
│        ▼                                                         │
│  ┌──────────┐                                                    │
│  │ nec8mon  │  单 exe，内嵌 PawnIO 模块 (LpcIO.bin / IntelMSR.bin)│
│  │ (8 MB)   │  无需 LibreHardwareMonitor 常驻                    │
│  └──────────┘                                                    │
└───────────────────────────────────────────────────────────────────┘
```

---

## 一、数据从哪来：为什么不靠 LibreHardwareMonitor

Windows 用户态**读不到** CPU 的 MSR（温度）和主板 Super I/O 芯片（风扇转速）。
这是操作系统的隔离机制决定的，任何纯用户态程序都绕不过去 —— 必须借助
**PawnIO 内核驱动**代劳端口 I/O。

各数据源的可用性：

| 数据源 | CPU 温度 | 风扇转速 | 说明 |
| --- | --- | --- | --- |
| `MSAcpi_ThermalZoneTemperature`（WMI） | 不可靠 | — | 多为机箱温度，甚至不存在 |
| `Win32_Fan`（WMI） | — | 不可靠 | `DesiredSpeed` 常年为 0 |
| `nvidia-smi` | 只有 GPU | 只有 GPU 百分比 | 需有独显 |
| **PawnIO + Super I/O 芯片** | ✅ | ✅ | 主板硬件监控控制器，最可靠 |

### 2.1 为什么 nec8mon 不再依赖 LibreHardwareMonitor

早期版本让 LibreHardwareMonitor 常驻后台，nec8mon 通过它的 `data.json` 取数。
实测发现两个问题：

1. **LHM 在无头会话（session 0）下枚举不到硬件** —— 以计划任务方式启动时
   常常返回一棵空树，表现为"温度无 / 转速无 / 只有 1 条读数"。
2. **多一个常驻进程、多一份 30 MB 目录、多一个计划任务** —— 与"程序要清理"的要求相悖。

从 2.1 版起，nec8mon **自己**内嵌 PawnIO 模块、直接读 Super I/O 芯片与 MSR
（2.2 版修掉了实机上暴露的三个问题：端口白名单被清空、基址字节序、
以及温度小数位寄存器填错；2.3 版修掉了"网页风扇转速恒为 0"和"调速没反应"
两个交互问题，详见第五章）：

- 读温度：`0x100`（CPU）、`0x102`（系统）、`0x104`（MOS）…… 11 路传感器
- 读转速：`0x140`（CPU 风扇）、`0x142`（水冷/泵）、`0x144`…`0x15E`（机箱风扇）
- Intel CPU 额外读 MSR `0x1B1` / `0x19C` 拿封装温度
- 写占空比：`0xA28`（目标值）、`0xA00`（手动模式位）

LHM 变成**可选补充**（`install.ps1 -WithLHM`），用于显卡 / 内存 / 每核心温度等
额外传感器，不再是必需项。

> PawnIO 模块来自 LibreHardwareMonitor v0.9.6（MPL-2.0），已内嵌进 exe，
> 部署时不需要拷贝任何额外文件。

---

## 二、Super I/O 芯片识别（踩过的坑都在这）

M720q 的 Super I/O 是 **Nuvoton NCT6686D-L**（板级原理图确认，兼作 LPC 接口、
风扇 PWM 管理、硬件遥测）。这一族芯片的寄存器布局与常见的 NCT679x **完全不同**。

识别流程严格对齐 LibreHardwareMonitor 的 `LpcIO.cs`：

```
选择槽位 (0x2E/0x2F 或 0x4E/0x4F)
   └─ 对配置端口连写两次 0x87          ← 关键：两次都写端口本身，不是写数据口
        └─ 读 0x20 芯片 ID / 0x21 修订号
             └─ ioctl_find_bars          ← 关键：扫描所有逻辑设备的基址，
                                            填充驱动的端口白名单
                  └─ 选 LDN 0x0B → 读 0x60/0x61 基址（间隔 1ms 双读校验）
                       └─ 退出配置态（写 0xAA）
```

### 2.1 四个必须记住的细节

**① `ioctl_find_bars` 不能省。**
PawnIO 的 LPC 模块只允许访问"已发现 BAR 内的端口"（`LpcIO.p` 的 `is_port_allowed`）。
不先扫描基址就去读 `基址+0x04/0x05/0x06`，一律返回 `STATUS_ACCESS_DENIED`。

**② `ioctl_select_slot` 会清空白名单 —— 选好芯片后必须重新扫描基址。**
这是 v2.1.0 实机失败的直接原因。模块源码里：

```cpp
DEFINE_IOCTL_SIZED(ioctl_select_slot, 1, 0) {
    reset();               // ← g_bars_count = 0，已扫描到的基址列表被抹掉
    ...
}
```

探测过程要依次试 `0x2E` 与 `0x4E` 两个槽位，**最后一次 `SelectSlot` 就把白名单清空了**；
此时再读运行时寄存器，驱动因为找不到白名单条目而一律拒绝。
所以选定芯片之后必须重新走一遍：选槽位 → 进配置态 → 校验芯片 ID →
`ioctl_find_bars` → 选 LDN 0x0B → 退出配置态。
（v2.2.0 里这段逻辑叫 `rearmSuperIO`，并且探测一成功就提前结束，不再去动别的槽位。）

**③ 解锁要连写两次 0x87 到配置端口本身。**
两次都写 0x2E 才对；一个写 0x2E 一个写 0x2F 是错的。

**④ 芯片表要覆盖两个修订号。**
NCT6686D 有 `0xD4/0x40` 和 `0xD4/0x41` 两个修订，漏掉任一个都会导致探测失败。

### 2.2 基址字节序：必须与驱动用同一种读法

这条是 v2.1.0 的第二个坑。PawnIO 模块的 `find_bars` 是这样读基址的：

```cpp
vals[i][0] = superio_inw(BASE_ADDRESS_REGISTER);   // 0x60
...
g_bars[g_bars_count] = val;                        // 规范化后进白名单
```

而 `superio_inw` 的字节序是**高位在前**：

```cpp
superio_inw(reg) { return (superio_inb(reg) << 8) | superio_inb(reg + 1); }
```

驱动只放行白名单里的端口，所以基址**必须**用同一个 `superio_inw` 的结果算出来。
用小端拼会得到 `0x200A`，它通不过合法性校验（`& 0xF007 != 0`），
即便蒙对了也会因为不在白名单里而被拒绝访问。

NEC8 实测：硬件监控逻辑设备 0x0B 上，寄存器 `0x60 = 0x0A`、`0x61 = 0x20`，
因此 `superio_inw(0x60) = 0x0A20`，**运行时基址就是 0x0A20**
（温度寄存器在 `0x0A24/0x0A25/0x0A26`）。

这条事实已经写进单元测试 `TestBaseFromSuperIOWord`，防止回归。

### 2.3 温度的 0.5 度小数位

NCT668x 的温度是「有符号整数（1 度为单位）+ 下一寄存器的第 7 位表示 0.5 度」。
LibreHardwareMonitor 的 NCT668x 分支是硬编码 `ReadByte(reg + 1) >> 7` 的，
初始化时如果漏填这个字段，程序就会去读寄存器 `0x000` 并给温度平白加上 0.5 度。
`TestTempSourceHalfRegisters` 守着这一点。

### 2.4 寄存器布局对照

| 用途 | NCT668x（M720q） | NCT679x |
| --- | --- | --- |
| 访问方式 | EC 风格：页/索引/数据（`+0x04/+0x05/+0x06`） | bank 选择风格 |
| 温度 | `0x100`…`0x114`（有符号 + 0.5° 小数位） | `0x075`/`0x077`…（LSB 1°） |
| 转速 | `0x140`…`0x15E` + `0x852`（13 位计数器） | `0x4B0`…`0x4CC`（13 位计数器） |
| 占空比写入 | `0xA28`…`0xA2F` | `0x109`/`0x209`/… |
| 手动模式 | `0xA00`（8 通道共用，第 N 位 = 通道 N） | `0x102`/`0x202`/… |
| 访问握手 | `0xA01`（`0x80` 申请 → `0x40` 释放） | 无 |

---

## 三、风扇调速是怎么做到的

**调速和读数走的是同一条路：PawnIO。**

用户态无法直接写 I/O 端口，但 PawnIO 驱动代劳了这件事。nec8mon 内嵌了
LHM 同款 PawnIO 模块（`assets/LpcIO.bin`，17 KB），因此 exe 里带着
"能写 Super I/O 寄存器"的能力，不需要额外拷贝任何文件。

### 3.1 通道（channel）是什么

NCT668x 一共暴露 **8 个可调速通道**（对应 8 路 PWM 输出），
外加 9 个只能读的测速口。程序里：

| 概念 | 含义 |
| --- | --- |
| **通道** | 一个可以接风扇、可以写占空比的 PWM 输出，编号 0–7 |
| **占空比寄存器** | `0x160`+N 读实际值、`0xA28`+N 写目标值 |
| **手动模式位** | `0xA00` 的第 N 位（8 个通道共用这一个字节） |
| **只读测速口** | 只有转速寄存器、没有 PWM 输出，编号 8–16，不参与调速 |

**通道号和"CPU 风扇"这种名字不是一回事。** LHM 给 NCT668x 的通道起名
是"CPU 风扇 / PUMP 风扇 / 机箱风扇 1…6"，那只是**通用假设**，不是实测接线。
NEC8 实测：通道 0（名字叫"CPU 风扇"）**根本没接风扇，恒为 0 RPM**；
真正在转的是**通道 1**（1015 RPM / 39%）。

所以程序里**一切以转速为准，不以名字为准**：

- 主风扇 = 当前**正在转**的那个通道（名字只作为次级规则）；
- 通道名统一渲染成 `通道 N · <名字>`，通道号排在最前面；
- 仪表盘新增「**风扇通道明细**」表，一眼就能看出哪个位置有风扇、
  哪个位置是空的、当前正在控制哪一个。

### 3.2 写寄存器的完整时序（NCT668x）

与 LibreHardwareMonitor 完全一致：

```
写 0xA01 = 0x80      ← 向芯片申请访问权
   （等待 50 ms）
读 0xA00             ← 当前手动模式位
写 0xA00 |= (1<<N)   ← 置上本通道的手动位
写 0xA28+N = 占空比   ← 目标值（百分比 × 2.55）
写 0xA01 = 0x40      ← 释放访问权
   （等待 50 ms）
```

交还硬件时反过来：清掉 `0xA00` 的对应位 → `0x80` → 写回原始占空比 → `0x40`。

**安全设计**（这是调速部分最要紧的地方）：

- 默认模式是 **自动**，程序绝不主动接管风扇 —— 必须由你在页面上明确选择。
- 占空比**下限 20%**，低于此值请求会被直接拒绝，避免风扇停转。
- **过温保护始终生效**：CPU 温度超过阈值（默认 75 °C）立刻恢复 100%，
  温度回落后再恢复你设定的目标值。无论处于哪种模式都管用。
- 接管风扇前会**保存芯片的原始寄存器值**；切回"自动"时原样写回，
  把风扇完整交还给 BIOS。
- 传统布局的芯片采用「**先写占空比、再切手动**」的顺序，
  避免中途异常导致风扇失去目标转速。
- 所有 ISA 总线访问都持有 `Global\Access_ISABUS.HTP.Method` 互斥量
  —— 与 LibreHardwareMonitor 共用同一把锁，不会两个程序抢总线。
- 每次写入后**读回占空比寄存器校验**；对不上就在日志里记一笔，
  提示"该通道可能没接风扇"。

> ⚠️ 调速属于写硬件寄存器操作。若你的主板 BIOS 也接管风扇，可能出现"两边打架"。
> 如果发现风扇行为异常，第一时间把模式切回「自动」，程序会把寄存器原样还原。

---

## 四、快速开始

### 1. 在 Mac 上构建发行包

```bash
cd nec8mon
./deploy/build.sh          # 或 bash deploy/build.sh
```

产物在 `dist/`：

```
dist/
├── nec8mon.exe        # 主程序（Windows amd64）
├── nec8mon-386.exe    # 32 位备用
├── install.ps1        # 一键安装
├── uninstall.ps1      # 干净卸载
├── README.txt         # 速查卡
├── THIRD-PARTY-NOTICE.txt
└── SHA256SUMS.txt
```

### 2. 拷贝到 NEC8 并安装

把整个 `dist/` 目录拷到 NEC8（U 盘 / 共享 / scp 均可），然后：

```powershell
# 以管理员身份运行 PowerShell
cd <dist 所在目录>
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

安装脚本会依次完成：

1. 安装 `nec8mon.exe` → `C:\nec8mon\`
2. 静默安装 **PawnIO** 驱动（`PawnIO_setup.exe -install -silent`）并校验注册表版本
3. 注册 **Windows 服务** `nec8mon`（开机自启、崩溃自动重启）
4. 创建桌面快捷方式 **「NEC8 监控面板」**
5. 注册 **计划任务** `nec8mon-open`（用户登录后自动弹出面板）
6. 验证传感器读数与调速能力
7. **运行 `nec8mon -diag` 打印完整现场诊断报告**

常用参数：

```powershell
.\install.ps1 -Port 8090            # 换一个端口
.\install.ps1 -Expose               # 允许局域网其它设备访问（默认只监听本机）
.\install.ps1 -RemoteAddress Any    # 配合 -Expose，允许任意来源
.\install.ps1 -SkipPawnIO           # 已自行装好 PawnIO 时跳过
.\install.ps1 -WithLHM              # 额外装 LHM，补充显卡/内存/每核心温度
.\install.ps1 -SkipShortcut         # 不建桌面快捷方式与登录自启
```

### 3. 在 NEC8 上使用

远程桌面登录后：

- **自动**：登录时计划任务已把面板弹到浏览器里；
- **手动**：双击桌面上的「NEC8 监控面板」。

地址是 `http://127.0.0.1:8080`（默认只监听本机，不对局域网开放）。

页面顶部两个大卡片：**CPU 温度**、**风扇转速**（带阈值配色与趋势图）；
中部是 **风扇调速** 面板；下方是全部传感器明细与数据源健康状态。

---

## 五、风扇调速面板

| 模式 | 行为 |
| --- | --- |
| **自动（交还 BIOS）** | 默认模式。完全不碰寄存器，风扇由 BIOS 曲线控制。 |
| **手动固定转速** | 滑块设定占空比（20%-100%），拖动即生效。 |
| **按温度曲线** | 按 CPU 温度自动调整；三套预设：静音 / 均衡 / 强劲。 |

其它控件：

- **占空比滑块** —— 目标占空比；旁边实时显示「已写入 X%」和「芯片读回 Y%」
  （后者是从芯片寄存器读回的真实值，能发现是否被别的程序改过）。
  **在「自动」模式下拖动滑块会自动切到「手动」并立刻写入** ——
  否则 BIOS 还在控制风扇，拖滑块不会有任何效果。
- **过温保护** —— 默认 75 °C，超过即强制满速。
- **风扇通道** —— 下拉里直接标出通道号、名字和**实时转速**，
  并提示「当前在转的是通道 N，调速请选它」。
- **风扇通道明细表**（面板下方）—— 列出每个通道的转速、占空比与状态：
  `当前在转` / `正在控制` / `未转动 / 未接风扇` / `只读测速口`。

### 5.1 设置会一直生效吗

**会。** 在网页上改完转速之后，下面这些情况都不会让设置丢失：

| 场景 | 风扇转速设置 |
| --- | --- |
| 关掉网页标签页 | **保持**（设置写在芯片寄存器里，不在浏览器里） |
| 关掉远程桌面 / 断开 RDP | **保持**（服务以 LocalSystem 身份在后台跑） |
| 重启 nec8mon 服务 | **保持**（设置从 `fan-state.json` 自动恢复） |
| 重启电脑 | **保持**（同上） |
| 手动切回「自动」模式 | 按用户的选择交还 BIOS |

原理是**设置有两份，各司其职**：

- **硬件寄存器**（芯片里的 PWM 命令寄存器）—— 真正决定风扇转速的东西。
- **`C:\nec8mon\fan-state.json`** —— 记住"用户想要什么"（模式、通道、占空比、
  曲线、过温阈值）。每次在网页上改动都会立刻落盘。

服务启动时先读回这个文件并重新接管通道，所以重启之后是你上次设的那个转速，
而不是 BIOS 的默认曲线。

> 💡 网页只是**控制面板**，不是执行者。关掉它没有任何副作用。

### 5.2 异常退出的兜底

如果服务被**强杀**（任务管理器"结束进程"、断电、系统崩溃），
程序来不及把风扇交还 BIOS，芯片会停留在手动模式上 —— 更麻烦的是
**过温保护也跟着失效了**，CPU 再热也不会自动提速。

程序对此有兜底：

1. 接管通道前，把"接管前的原始寄存器值"写进 `fan-restore-point.json`。
2. 正常退出时把风扇交还 BIOS，并删掉这个还原点。
3. **下次启动时发现还原点还在**，就知道上次是异常退出，
   先把芯片恢复成安全状态（BIOS 接管、过温保护重新生效），
   然后再按 `fan-state.json` 恢复用户的设置。

顺序很重要：**先清残局，再恢复设置** —— 残局没清干净就接管，
等于在手动模式上再叠一层手动模式。

### 5.3 2.3 版修掉的两个问题

**① 网页上风扇转速一直显示 0。**

原因：主风扇是**按名字**挑的 —— "CPU 风扇"命中 `cpu` 关键词得 100 分，
"真正在转的 PUMP 风扇"只有 60 分，于是永远选中那个**没接风扇的通道**。
硬件数据本身完全正确，是选错了对象。

修法：`pickPrimaryFan()` 改成**先看转速再看名字** —— 正在转的通道优先，
其次才是名字像不像 CPU 风扇；全部停转时才退回按名字打分。
（`TestPickPrimaryFanPrefersRunning` 守着这条。）

**② 调速没反应。**

三个原因叠在一起：

1. 每轮采集都会调用 `AutoChannel()`，把控制通道强行拉回"主风扇"，
   于是**每 2 秒就把你在页面上选的通道覆盖掉**，
   而且拉回去的还是那个空通道 0。
   → 修法：用户手动选过通道之后**锁定**，采集循环不再改动；
   自动对齐时也改成跟着"正在转的"通道走。
2. 切到「手动」模式时**不立即写寄存器**，要等下一轮采集（最多 2 秒）才生效。
   → 修法：`SetMode` / `SetChannel` 立刻把目标占空比写下去。
3. 在「自动」模式下拖滑块，`SetDuty` 只改内存里的目标值，
   硬件完全没被碰过。
   → 修法：前端检测到当前是自动模式时，连同 `mode: "manual"` 一起提交。

另外还加了 `-fanset` 命令行自检，绕过网页直接验证某个通道能不能被接管
（见 5.4）。

### 5.4 命令行自检：`-fanset`

现场验证"某个通道到底能不能调速"，不用开网页：

```powershell
# 接管通道 1 到 40%，连续观察 6 秒转速变化，然后自动交还 BIOS
C:\nec8mon\nec8mon.exe -fanset 1:40
```

输出形如：

```
【接管前】各通道现状（* 号是要测试的通道）
    通道  名称                            转速       占空比
  * 1     通道 1 · PUMP 风扇              1015 RPM   39%
    2     通道 2 · 机箱风扇 1                0 RPM     0%

已接管通道 1，占空比 40%，等待风扇响应…
  +2s  转速 980 RPM  芯片读回占空比 40%
  +4s  转速 965 RPM  芯片读回占空比 40%
  +6s  转速 970 RPM  芯片读回占空比 40%

【已交还 BIOS】各通道现状
```

判断方法：

- **转速跟着变、芯片读回也是 40%** → 通道正常，网页里选它就行；
- **芯片读回 40% 但转速是 0** → 这个位置没接风扇，换通道；
- **芯片读回还是原来的值** → 写入没生效（BIOS 接管了 PWM 输出），看日志。

内置的「静音」曲线（低负载时降速的核心）：

```
30°C → 20%    45°C → 22%    60°C → 35%
70°C → 55%    80°C → 85%    88°C → 100%
```

---

## 六、现场诊断：`-diag`

面板上温度或转速为空时，先跑诊断：

```powershell
C:\nec8mon\nec8mon.exe -diag
```

输出包含 8 个部分：

1. **基本信息** —— 计算机名、CPU 型号、操作系统版本、运行账户
2. **PawnIO 驱动** —— 注册表里的驱动版本、设备能否打开、模块能否加载
3. **Super I/O 探测过程** —— 两个配置端口 × 两种解锁序列的完整结果：
   芯片 ID、修订号、识别到的型号、基址字节与最终基址
4. **直接读取硬件传感器** —— 芯片型号、运行时基址、**端口白名单是否生效**、
   实际读到的温度与转速
5. **风扇调速状态与设置** —— 芯片当前被手动接管的通道、磁盘上保存的模式/
   通道/占空比/过温阈值、以及异常退出留下的还原点
6. **Intel MSR 温度** —— TJMax、封装温度、核心温度
7. **ACPI / WMI 兜底数据源** —— 免驱动的读数
8. **LibreHardwareMonitor 接口** —— 如果装了 LHM，看它的 `data.json` 摘要

诊断报告同时会存成 **`C:\nec8mon\nec8mon-diag.txt`**（UTF-8 带 BOM），
直接把这个文件传回来即可，不用复制控制台内容。

> **关于中文乱码**：2.3 版起，程序对真正的控制台句柄改用
> `WriteConsoleW`（Unicode 通道）输出，**完全绕开代码页**，
> 简体中文 Windows 上也不会再显示成乱码；输出被重定向到文件时写 UTF-8 字节。
> `log` 包的日志也走同一条通道。

诊断报告的第 4 节现在会直接告诉你**该调哪个通道**：

```
  风扇读取    : 成功
      通道 0 · CPU 风扇                    0 RPM  占空比  0%
      通道 1 · PUMP 风扇                1015 RPM  占空比 39%
      ...
  正在转的通道: 1（1015 RPM）—— 调速请选这个通道
  → 转速为 0 的通道通常是没接风扇；只有带占空比的通道才能调速
```

第 5 节则回答"设置还在不在"：

```
  芯片手动通道: 1（这些通道正被手动模式接管）
  设置文件    : C:\nec8mon\fan-state.json
  保存的模式  : 手动固定转速
  保存的通道  : 1（已锁定）
  保存的占空比: 30%
  保存的过温线: 75 °C
  还原点      : 无（上次退出正常）
```

这段输出能直接区分这几类故障：

| 现象 | 含义 |
| --- | --- |
| 第 2 节就报"打开设备失败" | PawnIO 驱动没装好 |
| 第 3 节芯片 ID 全是 0x00/0xFF | 解锁序列不对，或该端口没有芯片 |
| 第 3 节识别到型号但基址非法 | 基址读取的字节序/逻辑设备号不对 |
| 第 4 节"端口白名单：异常" | 扫描基址的步骤被后续操作清空了（`ioctl_select_slot` 的副作用） |
| 第 4 节温度读取报"拒绝访问" | 同上，运行时寄存器没被 PawnIO 放行 |
| 第 4 节"正在转的通道"是 0 或 2 | 别选那个通道，按提示选真正在转的那个 |
| 第 5 节"芯片手动通道"列出了通道但服务显示自动 | 上次异常退出，还没重启服务（重启后会自动还原） |
| 第 5 节"还原点"非空 | 同上 —— 上次进程被强杀，下次启动会自动还原 |
| 第 6 节 MSR 有值但第 4 节没有 | Super I/O 侧的问题，与 CPU 无关 |

> 第 6 节的 MSR 模块要用**另一个句柄**加载：PawnIO 驱动限制一个句柄只能加载
> 一个模块（句柄的 `FsContext` 非空时再加载会返回 `STATUS_ALREADY_INITIALIZED`）。

---

## 七、接口

| 路径 | 说明 |
| --- | --- |
| `GET /` | Web 仪表盘（单页，内嵌于 exe） |
| `GET /api/sensors` | 当前完整快照（JSON） |
| `GET /api/history` | 历史采样点（用于趋势图） |
| `GET /api/health` | 健康检查，有可用数据返回 200，否则 503 |
| `GET /api/fan` | 风扇调速状态（模式、占空比、曲线、后端信息） |
| `POST /api/fan/control` | 下发调速指令 |

`POST /api/fan/control` 请求体（字段都可省略）：

```json
{
  "mode": "curve",
  "duty": 30,
  "curve": { "points": [{ "temp": 30, "duty": 20 }, { "temp": 80, "duty": 85 }], "hysteresis": 3 },
  "safetyTemp": 75,
  "channel": 1
}
```

`channel` 一旦设置就**锁定**，采集循环不会再自动改回去；
`GET /api/fan` 里 `channelLocked: true` 表示已经锁定。

`GET /api/sensors` 返回示例：

```json
{
  "hostname": "NEC8",
  "updatedAt": "2026-11-11T22:41:03.812+08:00",
  "uptimeSeconds": 84213.5,
  "sources": [
    { "name": "superio", "ok": true, "count": 24, "detail": "1.20ms" },
    { "name": "intel-msr", "ok": true, "count": 2, "detail": "0.31ms" }
  ],
  "cpuTemperature": {
    "id": "superio/temp/CPU",
    "name": "CPU",
    "category": "Temperature",
    "hardware": "Nuvoton NCT6686D",
    "value": 34.5,
    "unit": "°C",
    "min": 0, "max": 0,
    "source": "superio"
  },
  "fan": {
    "id": "superio/fan/1",
    "name": "通道 1 · PUMP 风扇",
    "category": "Fan",
    "hardware": "Nuvoton NCT6686D",
    "value": 1015,
    "unit": "RPM",
    "min": 0, "max": 0,
    "source": "superio"
  },
  "fanPercent": {
    "id": "superio/control/1",
    "name": "通道 1 · PUMP 风扇 占空比",
    "category": "Control",
    "hardware": "Nuvoton NCT6686D",
    "value": 39,
    "unit": "%",
    "min": 0, "max": 0,
    "source": "superio"
  },
  "readings": [ "…全部传感器…" ]
}
```

> `fan` 是**当前正在转**的那个通道（不是名字最像 CPU 的那个）。
> `fanPercent` 与 `fan` **同通道**（`superio/control/<N>` 对应 `superio/fan/<N>`）。

---

## 八、日常运维

```powershell
# 服务与任务状态
Get-Service nec8mon
Get-ScheduledTask nec8mon-open

# 手动重启
Restart-Service nec8mon

# 调速状态
Invoke-RestMethod http://127.0.0.1:8080/api/fan | Format-List

# 一次性调速自检（跑完自动交还 BIOS）
C:\nec8mon\nec8mon.exe -fanset 1:40

# 诊断
C:\nec8mon\nec8mon.exe -diag
```

### 卸载（不留残留）

```powershell
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -RemovePawnIO   # 连驱动一起卸
```

卸载会删除：服务、计划任务、LHM 进程、桌面快捷方式、防火墙规则、
`C:\nec8mon` 目录。

---

## 九、直接运行（不装服务）

```powershell
cd C:\nec8mon
.\nec8mon.exe -open          # 启动并自动打开仪表盘
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8080` | HTTP 监听地址（想开放局域网用 `:8080`） |
| `-poll` | `2s` | 采集轮询间隔 |
| `-with-lhm` | 开 | 是否同时采集 LibreHardwareMonitor 的数据 |
| `-lhm` | `http://127.0.0.1:8085` | LibreHardwareMonitor 地址 |
| `-lhm-exe` | 空 | 指定后，检测到 LHM 未运行会自动拉起它 |
| `-history` | `900` | 保留的历史采样点数 |
| `-fan` | 开 | 是否启用风扇调速（关掉则纯监控） |
| `-fan-duty` | 沿用上次设置 | 手动模式的初始占空比；显式传值才覆盖已保存的设置 |
| `-fan-safety` | 沿用上次设置 | 过温保护阈值（°C）；显式传值才覆盖已保存的设置 |
| `-fan-channel` | `-1` | 指定要调速的通道；`-1` 表示自动跟随"正在转的"那个 |
| `-fanset` | 空 | 一次性调速自检，格式 `<通道>:<占空比>`，例如 `-fanset 1:40` |
| `-open` | 关 | 启动后用默认浏览器打开仪表盘 |
| `-svc` | 关 | 以 Windows 服务方式运行 |
| `-diag` | 关 | 打印现场诊断报告后退出 |
| `-v` | 关 | 输出逐轮采集日志 |
| `-version` | — | 打印版本号 |

---

## 十、项目结构

```
nec8mon/
├── main.go              # 入口：参数解析、信号处理、启动编排
├── model.go             # 统一数据模型 + CPU/风扇主读数挑选逻辑
├── collect.go           # 采集编排、数据源状态、历史环形缓冲
├── chipdb.go            # Super I/O 芯片表 + 寄存器布局 + 换算（纯逻辑，可测）
├── chipdb_test.go       # 芯片表与寄存器布局的单元测试（含实机基址事实）
├── console_windows.go   # 控制台输出走 WriteConsoleW（彻底避免中文乱码）
├── console_other.go     # 非 Windows 占位
├── lhm.go               # LibreHardwareMonitor HTTP 客户端与 data.json 解析（可选源）
├── collect_windows.go   # Windows 专用：ACPI/WMI、nvidia-smi、LHM 自愈拉起
├── collect_other.go     # 非 Windows 占位（便于本机开发调试）
├── server.go            # HTTP API + 内嵌仪表盘
├── open.go              # 用默认浏览器打开页面
├── fanctl.go            # 调速管理器：模式、曲线、通道锁定、过温保护（跨平台）
├── fanctl_windows.go    # Windows 后端：PawnIO 句柄、Super I/O 数据源、调速后端
├── fanctl_other.go      # 非 Windows 占位
├── fanstate.go          # 调速设置的持久化（fan-state.json）与异常退出还原点
├── fanstate_test.go     # 持久化往返、坏值防护、还原点增删的单元测试
├── fanset.go            # -fanset 命令行一次性调速自检
├── pawnio_windows.go    # PawnIO 驱动客户端（DeviceIoControl 协议）
├── isabus_windows.go    # ISA 总线互斥量（与 LHM 共用）
├── superio_windows.go   # Super I/O 探测与寄存器读写
├── msr_windows.go       # Intel MSR 封装温度读取
├── diag_windows.go      # -diag 现场诊断（同时输出 UTF-8 报告文件）
├── diag_other.go        # 非 Windows 占位
├── web/index.html       # 单页仪表盘（CSS/JS 全内联）
├── service_windows.go   # Windows 服务封装
├── assets/LpcIO.bin     # 内嵌的 PawnIO 模块（MPL-2.0，来自 LHM）
├── assets/IntelMSR.bin  # 内嵌的 PawnIO Intel MSR 模块（MPL-2.0，来自 LHM）
├── nec8mon_test.go      # 单元测试（含 data.json 解析用例）
├── fanctl_test.go       # 调速逻辑单元测试
├── testdata/            # 模拟的 LHM data.json
├── deploy/              # install.ps1 / uninstall.ps1 / build.sh
└── dist/                # 构建产物
```

---

## 十一、参考资料

- LibreHardwareMonitor — https://github.com/LibreHardwareMonitor/LibreHardwareMonitor
- PawnIO — https://github.com/namazso/PawnIO（下载地址 https://pawnio.eu ）
- PawnIO.Modules（LpcIO.p 源码，白名单机制的关键） —
  https://github.com/namazso/PawnIO.Modules
- FanControl — https://github.com/Rem0o/FanControl.Releases （同类工具，可作对照）
- StackExchange/wmi — https://github.com/StackExchange/wmi
