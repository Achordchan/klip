# Klip

Klip 是一个剪贴板管理工具：记下你复制过的文字、图片和文件，随时搜索、置顶、备注，再贴回去；可以登录账号，在多台设备间同步历史。

[English](README.en.md)

## 下载

到 [Releases](https://github.com/Achordchan/klip/releases/latest) 下载 `Klip-<版本>-setup.exe`，运行即可（Windows 10 / 11，64 位）。

- 装好后按 `Ctrl + Alt + 2` 呼出面板。
- **自动更新是无感的**：有新版本时 Klip 在后台下载好，等你离开电脑几分钟、窗口都关着时自动换上并重新启动；装不上会回到原来的版本。
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
| `tools/e2e_update.py` | Windows 上的端到端测试：覆盖安装旧版、无人值守地自动更新到下一个版本 |
| `.github/workflows/release.yml` | GitHub Actions：测试、构建、端到端测试，推送 tag 时发布 |
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
3. GitHub Actions 在 Windows 上跑全部测试、构建安装包和更新包、跑端到端更新测试，全部通过后发布 Release。已安装的 Klip 从最新 Release 的 `manifest.json` 得知新版本。

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
