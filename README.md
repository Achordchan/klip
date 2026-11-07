# Klip

Klip 是一个剪贴板管理工具：记下你复制过的文字、图片和文件，随时搜索、置顶、备注，再贴回去；可以登录账号，在多台设备间同步历史。

[English](README.en.md)

## 下载

到 [Releases](https://github.com/Achordchan/klip/releases/latest) 下载：

- **Windows 10 / 11（64 位）**：`Klip-<版本>-setup.exe`，运行即可。
- **macOS 12 及以上**：Apple 芯片用 `Klip-<版本>-mac-arm64.zip`，Intel 芯片用 `Klip-<版本>-mac-x64.zip`。解压后把 Klip 拖进“应用程序”。Mac 版暂未签名，第一次打开时系统会拦下：到“系统设置 → 隐私与安全性”，点 Klip 旁边的“仍要打开”。Mac 版暂时不能自动更新，有新版本时会提示下载地址。

- 装好后按 `Ctrl + Alt + 2` 呼出面板。
- **Windows 上的自动更新是无感的**：有新版本时 Klip 在后台下载好，等你离开电脑几分钟、窗口都关着时自动换上并重新启动；装不上会回到原来的版本。
- **从旧版（1.0.10 及更早）升级**：旧版不会自动收到新版本，请下载安装包直接覆盖安装。历史、设置、登录状态、云同步和开机启动都会保留。

## 功能

- 剪贴板历史：文本、图片、文件列表；搜索、备注、置顶、拖动排序、重复内容合并
- 按条目数 / 容量自动清理，置顶的可以不算在内；图片可以存成 WebP
- 悬停预览，预览里的文字可以选中复制
- 货架：按住修饰键拖动并晃动鼠标，临时放文件
- 主题、开机启动、捕获开关、文件扩展名和大小限制；存储位置可以迁移
- 账号与订阅，快照同步、Blob 去重传输、服务器推送触发同步

## 仓库结构

| 目录 | 内容 |
|---|---|
| `app/` | 客户端（Go + [MyGo](https://github.com/egoist/mygo)），说明见 [`app/README.md`](app/README.md) |
| `nas-agent-py/` | 同步后端（FastAPI），部署见 [`docs/deploy/baota-python-nas-agent.md`](docs/deploy/baota-python-nas-agent.md) |
| `installer/klip.iss` | Inno Setup 安装包脚本 |
| `tools/release_go.py` | 构建与打包：`Klip.exe`、安装包、更新包、更新清单 |
| `tools/package_mac.sh` | macOS 的 `Klip.app` 与 zip（Apple 芯片 / Intel） |
| `tools/e2e_update.py` | Windows 上的端到端测试：覆盖安装旧版、无人值守地自动更新到下一个版本 |
| `.github/workflows/release.yml` | GitHub Actions：Windows 与 macOS 上测试、构建，Windows 上跑端到端测试，推送 tag 时发布 |
| `docs/release-notes/` | 每个版本的发布说明 |
| `klip/` | 旧的 C# 客户端（1.0.10 及更早），只作参考 |

## 开发

需要 Go（版本见 `app/go.mod`）。

```bash
cd app
go run ./cmd/klip
go test ./...
```

Windows 版本可以在任何系统上交叉编译：

```bash
python3 tools/release_go.py build --version 1.0.11 --publish-dir /path/to/out
```

## 发布

1. 修改 `app/internal/version/version.go` 里的版本号，写好 `docs/release-notes/v<版本>.md`，合并到 `main`。
2. 推送 tag：`git tag v<版本> && git push origin v<版本>`。
3. GitHub Actions 在 Windows 和 macOS 上跑全部测试（发布需要仓库 Secret `KLIP_MANIFEST_KEY`，即更新清单的签名私钥，见下文），构建 Windows 安装包、更新包和两个 Mac 包，在 Windows 上跑端到端更新测试、在 macOS 上把应用启动一次，全部通过后一起发布 Release。已安装的 Klip 从最新 Release 的 `manifest.json` 得知新版本，清单里每个系统各有自己的包。

### 更新清单的签名与国内线路

- 已安装的 Klip（1.0.12 起）读取最新 Release 里的 `manifest.signed.json`，只接受用发布私钥签名、能被程序内置公钥（`app/internal/update/signing.go` 的 `ReleaseKey`）验证的清单；`manifest.json` 原样保留给 1.0.11。
- 检查和下载同时走 GitHub 直连和国内中转镜像（`app/internal/update/routes.go` 的 `DefaultMirrors`），按速度选线路，失败自动换下一条。镜像只用于 `github.com` 的地址，由于有签名和 SHA-256 校验，镜像无法篡改内容。
- 签名私钥只保存在仓库 Secret `KLIP_MANIFEST_KEY` 和发布者自己的备份里。丢失后，已安装的 Klip 将无法再接受任何新版本；泄露后，他人可以向所有用户推送任意程序。工具：`cd app && go run ./cmd/klip-sign`（`genkey` / `pubkey` / `sign` / `verify`）。

## 同步后端

`nas-agent-py` 提供账号、订阅、快照与 Blob 同步、SSE 事件和后台管理。本地启动：

```powershell
python -m venv .venv
.\.venv\Scripts\pip install -r .\nas-agent-py\requirements.txt
Copy-Item .\nas-agent-py\.env.example .\nas-agent-py\.env
.\.venv\Scripts\python -m uvicorn main:app --app-dir .\nas-agent-py --host 127.0.0.1 --port 17655
```

环境变量见 `nas-agent-py/.env.example`；打包上传用 `python tools/package_nas_agent_py.py`。

## 许可

见 [LICENSE](LICENSE)。
