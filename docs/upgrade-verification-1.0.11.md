# 1.0.11 升级验证方案

目标：1.0.10 及更早版本的用户点一下更新（或运行新安装包）就升到 1.0.11，**历史、设置、登录状态、云同步、开机启动、快捷方式都原样保留**，失败时能回到可用的 1.0.10。

1.0.10 里写死的东西决定了这条路怎么走（`klip/Services/UpdateService.cs`、`UpdateCoordinator.cs`）：

- 清单地址 `https://gitee.com/Achordchan/maccy/raw/master/docs/updates/manifest.json`；
- 轻量更新 = 清单里 `package`（`kind: zip`，`runtime: win-x64`）指向的 zip，里面有 `maccy-package.json`（`appId: maccy`、`version`、`runtime`、每个文件的 `path`/`sha256`/`size`）和 `maccy.exe`；
- 1.0.10 下载后校验大小和 SHA-256，解到暂存目录，再逐项校验，然后**由它自己写的 PowerShell 脚本**在它退出后把文件拷进安装目录、校验、失败回滚、再启动 `maccy.exe`；
- 轻量更新失败时，如果清单里有 `installer`，改用完整安装包（Inno，`AppId` 与 `AppMutex=maccy_mutex` 不变）。

所以 1.0.11 的 zip 必须是“1.0.10 看得懂的 zip”：程序文件名 `maccy.exe`、包内清单格式不变、不含用户数据路径（`images/ files/ blobs/ sync/ updates/ settings.json history.json …`）。1.0.11 的 Go 程序不是 .NET，所以包里是 Go 编译的 `maccy.exe` 加 `mygo-sqlite3.dll`；1.0.10 的 .NET 文件（`maccy.dll`、Avalonia 的 dll 等，现行 1.0.10 包里共 41 个，是依赖 .NET 运行时的“框架依赖”发布）它不会删，包清单里多写的 `legacyFiles` 它读不懂也不会管，**1.0.11 第一次启动时自己清掉**（`update.CleanLegacy`）。

## 一、自动化测试已经覆盖的

在 macOS 上能跑的都跑了，`go test -race ./...`（其中几项需要环境变量，见 `app/README.md`）：

| 项目 | 测试 |
|---|---|
| 版本比较与 .NET 的 `System.Version` 一致（`1.0.11` < `1.0.11.0`，一个数字当 `主版本.0`，`v` 前缀与 `+构建号` 后缀） | `update_test.go` `TestNormalizeVersion`、`TestVersionComparisonIsDotNets` |
| 保护路径清单、版本正则、重试次数、超时与 C# 源码逐项对照（源码在则对照，不在则跳过） | `TestProtectedListsMatchCSharp` |
| 清单键名不分大小写；仓库里现有的 `manifest.json` 能读 | `TestManifestKeysAreCaseInsensitive`、`TestTheManifestInTheRepoIsRead` |
| 检查更新：已是最新 / 有新版本 / 缺版本 / 缺地址 / 版本解析失败；503 重试 3 次，404 不重试；8 秒超时；取消 | `TestCheckDecisions`、`TestCheckRetriesLikeCSharp`、`TestCheckTimesOut` |
| 下载校验：大小、SHA-256、临时文件清理、重试 | `TestDownloadChecks` |
| zip 校验的全部规则（appId、版本、runtime、空清单、缺 `maccy.exe`、保护路径、文件缺失、大小/哈希不符、`..`、绝对路径、zip-slip、重名条目） | `TestPackageValidationRules`、`TestZipSlipIsRefused`、`TestDuplicateEntriesAreRefused` |
| 应用更新：先校验再拷、备份、拷后校验、删上一版有而新版没有的文件、记下文件清单、**用户数据不动**；中途失败回滚；拷后校验失败回滚；保护路径与缺 `maccy.exe` 拒绝 | `apply_test.go` |
| 真实二进制当辅助程序跑：`maccy.exe --apply-update plan.json` → 换文件 → 删旧 .NET 文件 → 重新启动新程序 | `cmd/klip/main_test.go` |
| 发布工具（Python）打出的 zip 被 Go 的客户端校验接受；`legacyFiles` 正确；清单被客户端读懂，1.0.10 的版本比较认为 1.0.11 更新 | `release_test.go` |
| 更新窗口：检查中 / 已最新 / 有新版本 / 必须更新 / 下载进度 / 失败重试 | `updatewin_test.go` |

## 二、必须在 Windows 上人工验证的

需要一台 Windows 10/11（虚拟机即可，**别用自己日常那台**，拍好快照），装有 .NET 版 1.0.10。以下命令里 `<repo>` 是本仓库。

### 0. 准备

1. 从 Gitee Release 下载 `maccy-1.0.10-setup.exe`，安装；启动，**造数据**：复制十几条文本、一张图片、一个文件；置顶两条、写两条备注；在偏好设置里改几项（最大条目、主题、勾上开机启动）；登录一个**有订阅的测试账号**并同步一次。记下：历史条数、置顶与备注、设置值、`%LOCALAPPDATA%\maccy` 的内容、注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 里 `maccy` 的值、开始菜单与桌面快捷方式。
2. 备份整个 `%LOCALAPPDATA%\maccy`（以及安装目录，默认安装时两者是同一个文件夹）。
3. 先把现有清单拷一份当底稿：`mkdir artifacts\testbed` 然后 `copy docs\updates\manifest.json artifacts\testbed\manifest.json`（发布工具改的是这份拷贝，不动仓库里的清单）。构建 1.0.11（Windows 上；`release_go.py` 在任何装有 Go 的系统都能跑 `build` 和 `package`，但 `installer` 需要 Inno Setup）：

   ```bash
   python tools/release_go.py release --version 1.0.11 --notes-file notes.txt ^
       --legacy-from manifest --base-url https://gitee.com/Achordchan/maccy/releases/download ^
       --manifest artifacts/testbed/manifest.json
   ```

   `--legacy-from manifest` 让它去下载现在清单里的 1.0.10 包，取出文件清单，写进新包的 `legacyFiles`。结果放在 `artifacts/installer/`（`maccy-1.0.11-setup.exe`、`maccy-1.0.11-win-x64.zip`）和 `artifacts/testbed/manifest.json`；把两个文件也拷到 `artifacts/testbed/`。
4. 启动测试服务（管理员终端，它要监听 443）：

   ```bash
   cd app && go run ./cmd/klip-testbed -dir ../artifacts/testbed
   ```

   它会写出 `gitee.com.cer` 并打印两条命令：把证书加入当前用户的受信任根（`certutil -user -addstore Root …`），hosts 里加 `127.0.0.1 gitee.com`。**测完务必删证书、还原 hosts。**

### A. 轻量更新（主路径）

1. 打开 1.0.10；约 6 秒后主窗口出现更新按钮（或偏好设置里点“检查更新”）。点更新。
2. 预期：进度窗口 → 程序退出 → 几秒后 Klip 自己启动。
3. 检查（逐条打勾）：
   - [ ] 任务管理器里是新程序，`maccy.exe` 属性里“文件版本/产品版本”是 1.0.11，图标是 Klip 的；
   - [ ] 历史条数、置顶、备注、图片、文件缓存全部在；
   - [ ] 偏好设置里各项与更新前一致；仍是登录状态，立即同步成功，另一台设备上的改动能同步下来；
   - [ ] 注册表 `maccy` 值未变，且重启电脑后仍自启；
   - [ ] 开始菜单与桌面快捷方式还指向 `maccy.exe`，能启动；
   - [ ] 启动后一两秒，安装目录里的 `maccy.dll`、`Avalonia*.dll`、`runtimes\` 等旧文件消失（`legacyFiles`），`settings.json`、`history.json`、`images\` 等一个没少；
   - [ ] `updates\last_light_update.log` 里有 `light update applied`；`updates\installed-package-files.json` 存在；
   - [ ] 再按一次 `Ctrl+Alt+2`、复制几条、搜索、置顶、备注、删除、云同步都正常。

### B. 轻量更新失败 → 改用完整安装包

1. 在 `artifacts/testbed/manifest.json` 里把 `package.sha256` 改错一位，重复 A 的第 1 步。
2. 预期：弹出“轻量更新失败 … 将改用完整安装包更新。”，下载安装包，Inno 安装向导出现（覆盖安装，沿用原目录），装完启动。
3. 检查同 A；另外 [ ] “添加或删除程序”里版本变为 1.0.11；[ ] 旧的名为 “Maccy” 的快捷方式没有重复出现（`[InstallDelete]` 删了，新建的是 “Klip”）。

### C. 手动运行新安装包覆盖安装

1.0.10 在运行时双击 `maccy-1.0.11-setup.exe`：安装程序应识别 `maccy_mutex` 提示关闭正在运行的 Klip，装完后数据与设置同 A。

### D. 回滚

1. 复原快照，重复 A，但在 1.0.10 退出之前，把 `%LOCALAPPDATA%\maccy\updates\staging\<版本>-<guid>\mygo-sqlite3.dll` 改一个字节。
2. 预期：1.0.10 的脚本在拷贝前发现 `sha256 mismatch`，弹窗“轻量更新失败，已尝试回滚”，原程序重新启动，数据与版本不变。日志在 `updates\last_light_update.log`。

### E. 1.0.11 之后的更新（Go 辅助程序）

发布一个测试用的 1.0.12（`--version 1.0.12`，不带 `--legacy-from`），在 1.0.11 上更新：
- [ ] 进度窗口 → 退出 → 新版本启动；`updates\helper\` 下有辅助程序副本（下次启动时被清理）；
- [ ] 日志 `last_light_update.log` 有 `light update applied`；
- [ ] 同样把某个文件改坏做一次回滚测试（预期弹窗并保留 1.0.11）。

### F. 其他

- [ ] 卸载（“添加或删除程序”）后，用户数据目录 `%LOCALAPPDATA%\maccy`（若与安装目录同一个文件夹，Inno 只删它装过的文件）仍在；
- [ ] 杀毒软件/Defender：Go 编译的未签名 `maccy.exe` 与 `mygo-sqlite3.dll` 是否被拦截或弹 SmartScreen（影响首次体验，见“风险”）；
- [ ] 第一次更新时电脑上同时有第二个 Windows 用户在运行 Klip 的情况（`maccy_mutex` 是会话内的，不同用户互不影响）。

## 三、风险与对策

| 风险 | 说明 | 对策 |
|---|---|---|
| 第一跳依赖 1.0.10 自己的脚本 | 无法改动；只能保证包符合它的格式（见 `release_test.go`） | 用上面的 A、B、D 在真机演练；发布前再用 `klip-testbed` 走一遍真正的 zip |
| 旧 .NET 文件残留 | 1.0.10 只删“上一次轻量更新清单里有、这次没有”的文件，而从未做过轻量更新的安装没有那份清单 | `legacyFiles` + 首次启动清理；安装包路径用 `[InstallDelete]` |
| 未签名程序被拦 | Go 程序没有签名；1.0.10 的 .NET 程序也没有，但哈希不同、信誉从零开始 | 先发给小范围用户；需要时申请代码签名；`-trimpath -ldflags "-s -w"` 已用，体积和特征尽量接近常规 Go 程序 |
| `mygo-sqlite3.dll` 缺失 | 缺了历史同步的快照读写会失败 | 发布脚本从 MyGo 的清单下载并校验 SHA-256 后放进包；包校验要求它在清单里；A 的检查项里验证同步 |
| 清单撤回不能降级 | 清单只告诉客户端“最新”是谁，客户端不会因为清单变旧而回退 | 出问题就尽快发 1.0.12；安装前的文件在 `updates\backups\<版本>-<时间>\`，可手工还原 |
| 只有 Windows x64 有轻量更新 | 清单里 `package` 是 `win-x64`；清单另有 `packages` 数组给以后的多平台，1.0.10 不读 | macOS/Linux 的更新方式另行决定（现在只提示到发布页下载） |
| 添加/删除程序里版本号 | 轻量更新不改注册表里的版本，保持 1.0.10 | 属于 1.0.10 的行为；走安装包（B、C）会更新 |
| 回滚测试依赖 PowerShell | 1.0.10 的脚本要 PowerShell 5.1 与 `ExecutionPolicy Bypass` | 与 1.0.10 现状相同，没有新增依赖；1.0.11 之后的更新不再用 PowerShell |
