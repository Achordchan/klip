# app：Klip 1.0.11 的 Go 实现

阶段 2 的内容：与 1.0.10 及更低版本互通的数据层，还没有界面。

| 包 | 内容 |
|---|---|
| `internal/model` | `Item`（ClipboardItem）、`Settings`（AppSettings），带 C# 的 JSON 格式：大驼峰字段、枚举为整数、时间为 DateTimeOffset（保留 `+08:00` 之类的偏移，最多 7 位小数，UTC 写成 `+00:00`） |
| `internal/paths` | 数据目录：Windows 沿用 `%LOCALAPPDATA%\maccy`，macOS `~/Library/Application Support/Klip`，Linux `$XDG_DATA_HOME/klip` |
| `internal/settings` | `settings.json` 读写；不认识的字段原样保留；旧同步地址迁移；先写临时文件再替换 |
| `internal/history` | `history.json` 读写；启动时按 `Normalize` 丢弃内容已不存在的条目、校正大小 |
| `internal/snapshot` | 同步快照（SQLite，schema 2，兼容读 schema 1）的导出与导入 |
| `internal/history`（服务） | 剪贴板历史列表（C# `ClipboardHistoryService` + `ClipboardPersistenceService`）：去重合并、置顶、备注、拖动排序、按条数/体积清理、被移出的条目的图片和文件缓存一起删除、`ReplaceAll`、防抖保存 `history.json` |
| `internal/platform` | 系统相关的部分：MyGo 剪贴板接到 `capture.Clipboard`、剪贴板变化轮询（`Watcher`）、变化计数器、前台应用识别 |
| `internal/capture` | 把剪贴板内容变成历史条目（`ClipboardCaptureService`）和把条目写回剪贴板（`ClipboardApplyService`）；剪贴板本身在接口后面，平台适配（MyGo）以后接 |
| `internal/syncer` | 同步编排（C# `SyncService` 加上调用它的主窗口/偏好设置里的流程）：何时上传、本地与远端谁新、合并、冲突、`SyncLast*` 字段维护、指纹、错误文案、后台同步的触发时机 |
| `internal/syncclient` | 同步服务器（nas-agent-py）的客户端：账号（登录/注册/刷新/改密码）、订阅状态与卡密、manifest、快照与 blob 的 gzip 上传下载、SSE 事件流，以及保存登录态并自动续期的 `Session`、断线重连的 `Watch` |

## 对照测试

```bash
cd app && go test ./...
```

测试里有四类对照：

1. **直接读 C# 源码比较**（`../klip/Services/*.cs`、`Models/*.cs`）：`AppSettings` 的字段名、顺序和默认值，`ClipboardItem` 的字段顺序，`ClipboardContentKind` 的枚举顺序，快照的 DDL 文本和 schema 版本。C# 代码一改，这些测试就会失败。C# 源码不在仓库时这几项自动跳过。
2. **C# 写出的样例文件**（`testdata/`）：读进来再写回去，所有字段、null、非 ASCII 转义和未知字段都不变。
3. **独立手写 SQL 模拟 C# 的写法**造出 schema 1 和 schema 2 的快照，确认 Go 能读。
4. **两台设备往返**：设备 A 导出快照，设备 B 没有任何内容，通过 blob 回调按哈希取回后导入，图片和文件字节一致。

## 没有验证的部分

- **没有真正运行 C# 客户端**：本机没有 dotnet，所以“C# 读 Go 写出的快照/JSON”只能靠上面的源码对照和格式推理保证，没有端到端验证。需要在装有 .NET 的机器上补一次：用 Go 导出的 `snapshot.sqlite` 和 `history.json` 喂给 1.0.10。
- 时间格式只在字符串层面比对，没有用 .NET 的解析器验证。
- `Normalize` 里 Windows 路径的行为（盘符、大小写）只在 macOS 上测过。

## SQLite：已换成 MyGo 的 plugins/sqlite

`internal/snapshot` 现在用 `github.com/egoist/mygo/plugins/sqlite`（SQLite 3.53.4，Zig 编译的动态库，通过 purego 加载，无 cgo）。数据库访问集中在 `writeDB`、`itemStatements` 和 `Read` 里的几个读取函数。全部测试不改动通过；测试里另外用 modernc 打开快照，作为独立的 SQLite 实现互相校验（只在测试里，不进程序）。

| 项目 | 结果 |
|---|---|
| 库文件大小 | darwin-arm64 1.48MB，darwin-amd64 1.38MB，linux-amd64 1.40MB，windows-amd64 1.40MB，windows-arm64 1.27MB |
| 对二进制的影响 | 纯 Go 的 modernc 约增加 5MB（只含快照读写的程序 7.0MB，纯标准库对照 1.8MB）；插件方式下库不进二进制，只多一个 1.3–1.5MB 的文件，代码部分很小。整个应用里 MyGo 本身已经在，所以净节省约 2MB 左右（估算，未用完整应用实测） |
| 加载方式 | macOS / Linux：`dlopen`（Linux 加 `RTLD_DEEPBIND`，避免和 WebKit 的系统 SQLite 冲突）；Windows：`syscall.LoadLibrary` + `GetProcAddress`，调用走 `purego.SyscallN` |
| 库的位置 | 依次找：环境变量 `MYGO_SQLITE_LIBRARY`、应用资源目录、可执行文件所在目录；都没有且未打包时，下载到用户缓存目录并校验 SHA-256；已打包的应用缺库会直接报错，不下载 |
| 支持的平台 | darwin/linux/windows 的 amd64 和 arm64。**不支持 32 位**：现在 C# 版的 `win-x86` 用户升级不了，需要确认是否还有这类用户 |
| 性能 | 5000 条文本：导出 42ms（840KB），读取 18ms |

要注意的限制：
- **打包要带上库**：Windows 是 `mygo-sqlite3.dll`，macOS 是 `libmygo-sqlite3.dylib`。`mygo build` 会自动放进应用；我们自己的发布流程（升级 zip 的文件清单、Inno 安装包）要把它列进去，并且 `maccy-package.json` 里要有它的 sha256。
- **首次运行测试需要联网**，下载库到 `~/Library/Caches/mygo/natives/`；CI 里可以预置或设置 `MYGO_SQLITE_LIBRARY`。
- 插件每次只执行一条语句，所以建表脚本在代码里按分号拆开，PRAGMA 单独执行；插件拒绝事务控制语句，批量写入用它的 `Transaction`（`BEGIN IMMEDIATE`）。
- 插件依赖整个 `mygo` 包（取资源目录），所以数据层的单元测试也会链接 MyGo 核心；这对最终应用没有额外代价。

## 同步客户端（internal/syncclient）

行为照 C# 的 `NasAgentClient`、`AuthService`、`SyncEventStreamService`：同样的请求路径和超时（健康检查 8s、状态 20s、manifest 30s、兑换 10s、blob 检查 30s、传输 5 分钟），GET 在超时或连接错误后重试一次（间隔 350ms），401/403 的订阅状态当作“未订阅”，失败信息里把 `WWW-Authenticate`、`X-Auth-Failure` 放在响应体前面，账号类错误只显示服务器给的 `error` 文字。SSE 解析、重连延迟（2s 起翻倍到 15s）、事件合并（250ms）和令牌续期（剩余不足 30s 就刷新；刷新被拒绝就登出，网络不通则保持登录）也都一致。

### 测试

```bash
cd app && go test ./...                        # 不需要 Python：路由对照 + 假服务器
# 对真实服务器的端到端测试：
python3 -m venv /tmp/nasvenv && /tmp/nasvenv/bin/pip install -r ../nas-agent-py/requirements.txt
KLIP_NAS_PYTHON=/tmp/nasvenv/bin/python go test -v -run TestAgainstTheRealServer ./internal/syncclient/
```

- `routes_test.go`：读 `nas-agent-py/app.py` 的路由装饰器，客户端用到的每条“方法 + 路径”都必须存在；服务器上新增的 `/sync`、`/auth`、`/subscription`、`/card` 路由也会提醒（说明客户端还没跟上）。
- `fake_test.go`：用假服务器覆盖真实服务器碰不到的情况：401/403/500 的处理、gzip 上传下载与服务器不压缩的情况、SSE 的各种写法（注释行、`\r\n`、多行 data、没有事件名）、`Watch` 的刷新/重连/退避/合并、`Session` 的续期和登出、GET 重试。
- `integration_test.go`：启动真实的 nas-agent-py（临时数据目录），完整走一遍：注册、重复注册、错误密码、刷新令牌（旧的被撤销）、未订阅和未分配档位时的 403、卡密兑换、设置档位、设备 A 用 `snapshot.Export` 导出并上传、按哈希补传 blob（错误哈希被拒绝）、设备 B 下载并用 `snapshot.Read` 导入且文件字节一致、事件流收到 `connected` 和 `sync-updated`、过期令牌下 `Watch` 自动续期后收到更新。没有设置 `KLIP_NAS_PYTHON` 时自动跳过。

### 还没做

- C# `SyncService`（974 行）里的**同步编排**：什么时候上传、怎么判断本地和远端谁新、冲突处理、`SyncLast*` 字段的维护。这次只做了它调用的客户端层。
- 没有测的：服务器的 413（快照超过档位容量）和 409，需要 100MB 以上的数据；真实邮件验证码（服务器没配 SMTP）；对线上服务器 `maccy.baymaxgroup.com` 没有发过任何请求。
- 对照的是仓库里这份 `app.py`，不保证和线上正在运行的版本完全一致。

## 同步编排（internal/syncer）

把 `SyncService` 和它的两个调用方（`MainWindowViewModel.RunAutoSyncAsync`、`PreferencesWindowViewModel` 的“立即同步/上传/恢复”）里的逻辑做成一个与界面无关的包：

- `Syncer.Run`：看远端 manifest → `Decide` → 上传 / 下载 / 合并后再上传 / 什么都不做 → 记住状态。进度步骤和百分比（30、52、68、84 等）与 C# 一致。
- `Decide`：和 `DecideDirectionWithState` 同一张表（remote_missing、first_sync_merge、first_sync_remote_exists、remote_rolled_back、state_matched、local_changed_only、remote_changed_only、timestamp_fallback）。远端回退的容差 2 秒，时间戳比较的容差 2 秒。
- `Fingerprint`：本地历史的指纹，**逐字节照 C#**（字段顺序、`schema=1` 头、行尾用平台的 `Environment.NewLine`、.NET ticks、`Path.GetFileName`）。这样从 1.0.10 升级上来的 Windows 设备，保存在 `settings.json` 里的 `SyncLastLocalFingerprint` 在 1.0.11 里仍然相等，不会第一次同步就当成“本地有改动”。
- `MergeItems`：按内容哈希（同种类）或 id 合并重复条目：置顶取或、复制次数相加、体积取大、首次时间取早、备注优先本地、最新的那份为准；按时间倒序，同时间按 id。合并前先把本地历史导出成 `sync/backups/pre-merge-*.sqlite`。
- 令牌：用前不足 60 秒就续期；服务器返回 401 就续期并重试一次（blob、快照、manifest 都一样）；403 且文字含 subscription expired / 订阅已过期 是“订阅已过期”；续期被服务器拒绝则登出，网络不通则保持登录。
- `Auto`：后台同步的触发：启动时、收到事件流的 `sync-updated` 时、历史变化后停 1.2 秒再同步；同时只跑一个；同步自己改历史时不触发；第一次后台同步成功之前，历史变化不触发（C# 的 `_initialSyncCompleted`）。
- `Explain`：同步错误的中文提示，主窗口和偏好设置各一种说法，与 C# 两个函数对应。

### 与 C# 行为不同的地方（有意的）

1. **新账号的第一次同步会自动上传。** C# 里服务器还没有快照时 `/sync/manifest` 返回 404，`RunAutoSyncAsync` 直接报错“云端还没有快照”，决策表里的 `remote_missing` 分支实际走不到；只有手动点“上传”或改了历史才会传。`Run` 把 404 当作空 manifest，于是走 `remote_missing`：本地有数据就上传，没有就什么都不做。
   `Syncer.ManifestInfo` 仍然和 C# 一样把 404 当错误返回。
2. 偏好设置里对“其他 InvalidOperationException”原样显示文字，Go 里分不出异常类型，统一显示“同步失败：文字”。

### 没有改的 C# 行为（测试里写明了）

- **两台设备都改了之后，较新的一次捕获会覆盖对方的改动。** 先上传的那台的新增条目会被后上传、捕获时间更晚的那台覆盖，不会合并（只有“首次同步”才合并）。`TestDevicesSyncThroughTheRealServer` 里有这个场景，用来说明现状；要改成总是合并，是一个产品决定。

### 测试

```bash
cd app && go test ./...      # 不需要 Python：指纹对照、决策表、合并、错误文案、后台触发
KLIP_NAS_PYTHON=/tmp/nasvenv/bin/python go test ./...   # 加上对真实 nas-agent-py 的端到端
```

- 指纹有两重对照：用 Python 独立按 C# 算法算出的金标准值（Unix 和 Windows 行尾各一个），以及直接读 `SyncService.cs` 比较写进指纹的字段表达式。
- 端到端用真实服务器、同一个账号的多台“设备”（各有自己的数据目录和设置）：首次上传、重复上传跳过、另一台首次下载（含图片 blob）、一台改动另一台跟随、两台都改的冲突、带重复内容的首次合并（备份文件、去重、备注与次数）、令牌被服务器拒绝后续期重试、令牌临近过期的提前续期、续期令牌被撤销后登出、未订阅账号的“订阅已过期”、进度步骤。
- 变异检查：改反上传/下载方向、复制次数不相加、去掉同哈希跳过上传、改指纹里的置顶标记，测试都能抓到。

### 还没做

- 真正的 `ClipboardHistoryService` / `ClipboardPersistenceService` 的 Go 版（测试里用的是内存里的假历史）：条目的去重、置顶、容量清理、`ReplaceAll` 时删除不再用的图片和文件目录。下一阶段。
- 界面状态（正在同步、进度条、Toast）、“只有开启云同步才在本地改动后上传”的开关；`UploadAfterLocalMutation` 已提供，是否调用由界面决定。
- 把 `syncclient.Watch` 的 `onUpdated` 接到 `Auto.OnRemoteUpdated`：只是一行连接，等有应用外壳时做。

## 历史服务与捕获（internal/history、internal/capture）

照 `ClipboardHistoryService`、`ClipboardPersistenceService`、`ClipboardCaptureService`、`ClipboardApplyService`：

- **去重**：开启合并时，同种类、内容哈希相同（不分大小写）的条目不新增，而是把旧条目移到最前，时间、大小、文本换成新的，复制次数加一，置顶和备注保留；新捕获多出来的图片文件和文件缓存目录被删除。
- **限额**：条数和体积（`MaxItems`、`MaxMegabytes`）；默认置顶的不计入限额；不排除置顶时，置顶的仍计数，但从不被删，所以列表可以超过限额。
- **文件跟着条目走**：条目被删、被挤出、或被 `ReplaceAll` 换掉时，它的图片文件和 `files/<id>/` 缓存目录一起删除；`ReplaceAll` 只删新列表里用不到的（图片按路径比较，不分大小写；文件目录按 id）。
- **保存**：变化后 350 毫秒保存一次 `history.json`（期间的更多变化并进同一次保存，不会推迟），`Close` 时再存一次；启动时 `Load` 用 `Normalize` 丢掉内容已不存在的条目。
- **捕获的顺序**：文本，然后文件，然后图片，取第一个有内容的。文本空白就往下走；开启合并且最上面一条文本相同就什么也不做；应用自己写入剪贴板后 250 毫秒内不捕获。
- **哈希与大小**：`text:`、`files:`、`img:` 加上大写十六进制 SHA-256（与 C# 一致，所以和 1.0.10 的历史互相去重）；文本的体积是 UTF-16 长度乘 2；文件列表的体积先看缓存副本，再看原文件，最后按路径长度算。
- **文件过滤**：目录、危险类型（`.exe`、`.dll`、`.ps1`、`.lnk` 等，和 C# 的列表逐项对照）、不在允许扩展名里的、超过大小上限的都不收；允许列表为空表示不限（危险类型除外）；大小上限取“文件上限”和“历史体积上限”中较小的。
- **单个图片文件**当作图片收（转成 PNG 保存）；读不出来就当普通文件。

与 C# 的差别：C# 里每次删除都发一次“已变化”通知，Go 里一次操作只发一次；Windows 的 DIB 手工转 BMP 没有移植（MyGo 的 `ReadImage` 已经处理）；“其他格式里找图片”的兜底只在剪贴板里没有文本和文件时才试，避免每次变化都多读一遍。

测试里直接读 C# 源码核对：历史的默认限额、危险扩展名列表、哈希前缀、尝试的顺序、250 毫秒、`NNNN_名字` 的缓存命名。

## 平台适配（internal/platform）

- **`Clipboard`**：`capture.Clipboard` 的 MyGo 实现。MyGo 把每次剪贴板调用切到主线程，所以可以在轮询协程里用，但必须等应用就绪（`App.WhenReady`）之后。写图片时如果给了文件路径，同一条目里同时提供 PNG 和文件（粘贴到文件夹得到文件，粘贴到编辑器得到图片）。
- **`Watcher` + `ChangeSeq`**：每 400 毫秒问一次变化计数器（macOS：`NSPasteboard.changeCount`；Windows：`GetClipboardSequenceNumber`），数字变了才调 `OnChange`。第一次只记数，所以启动前剪贴板里的内容不会被收进来（与 C# 一致）。没有计数器的平台（Linux）每次都看，靠捕获规则丢掉与最新一条相同的内容。C# 用的是 `AddClipboardFormatListener` 的事件；Go 这边用轮询，延迟最多 400 毫秒，空闲约 0.03% CPU（阶段 1 实测）。
- **`ForegroundApp`**：来源应用。Windows 照 `TryGetForegroundApp`：前台窗口的进程，路径加不带扩展名的文件名；读不到路径的系统进程返回空（C# 会给出进程名，没有移植）。macOS 用 `NSWorkspace.frontmostApplication` 的本地化名称和 bundle 路径。Linux 返回空。

### 测试

- `watcher_test.go`：计数规则、无计数器时的退化、`Run` 的启动和停止（`-race`）。
- `real_test.go`：**真实系统剪贴板**的端到端（macOS 本机跑过，连续三次通过）：文本、图片、文件的写入和读回；从另一个进程（`pbcopy`）复制 → 轮询 → 捕获 → 入历史；重复内容合并；应用到剪贴板后不会被再收进来；缓存文件；前台应用；计数器。这些测试会**覆盖剪贴板内容**，所以默认跳过，需要 `KLIP_REAL_CLIPBOARD=1`；测试结束会尽量把原来的文本放回去。它们在测试里启动一个无窗口的 MyGo 应用（`TestMain`）。

```bash
cd app && KLIP_REAL_CLIPBOARD=1 go test -count=1 -race ./internal/platform/
```

变异检查：去掉计数相等判断、去掉图片同时给文件、去掉应用时的“自己写的”标记，测试都能抓到。

### 没有验证的

- Windows 和 Linux 只验证了能编译（`GOOS=windows|linux go vet`）：`GetClipboardSequenceNumber`、`QueryFullProcessImageNameW`、Windows 上 MyGo 的文件和图片格式都没在真机上跑过。
- macOS 上用 `osascript` 复制文件得到的格式里没有文本，所以先看文本的顺序没问题；**Finder 里真正按 Cmd+C 复制文件**会不会同时带文件名文本没有验证。如果带，会被当成文本收下，需要在 macOS 上把“文件优先”做成平台规则。
- 前台应用是在复制发生之后才问的，所以复制后马上切换窗口会记成新窗口（C# 同样）。
