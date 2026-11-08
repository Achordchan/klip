# Klip

Klip is a clipboard manager: it keeps the text, images and files you copy, so you can search, pin, annotate and paste them again, and sync your history across devices with an account.

[中文](README.md)

## Download

From [Releases](https://github.com/Achordchan/klip/releases/latest):

- **Windows 10 / 11 (64-bit)**: run `Klip-<version>-setup.exe`.
- **macOS 12 or later**: `Klip-<version>-mac-arm64.zip` for Apple silicon, `Klip-<version>-mac-x64.zip` for Intel. Unzip and drag Klip to Applications. The Mac app is not signed yet: the first time, macOS blocks it; open System Settings > Privacy & Security and click "Open Anyway" next to Klip. It does not update itself yet; it tells you when there is a new version and where to download it.

- Press `Ctrl + Alt + 2` to open the panel.
- **Updates on Windows are seamless**: a new version is downloaded in the background and installed when you have been away for a few minutes with Klip's windows closed, then Klip starts again on its own. If it cannot be installed, the previous version is put back.
- **Upgrading from 1.0.10 or earlier**: those versions do not receive the update by themselves. Download the installer and install over them; history, settings, sign-in, sync and start-at-login are kept.

## Repository

| Folder | Contents |
|---|---|
| `app/` | The client (Go + [MyGo](https://github.com/egoist/mygo)) |
| `server/` | The sync server (Cloudflare Workers + D1 + R2 + Durable Objects), see [`server/README.md`](server/README.md) |
| `installer/klip.iss` | Inno Setup script |
| `tools/release_go.py` | Build and packaging: `Klip.exe`, installer, update package, update manifest |
| `tools/e2e_update.py` | End-to-end test on Windows: installing over the old version, and an unattended update to the next one |
| `.github/workflows/release.yml` | GitHub Actions: tests, build, end-to-end test; publishes the release on a tag |
| `klip/` | The old C# client (1.0.10 and earlier), for reference |

## Development

```bash
cd app
go run ./cmd/klip
go test ./...
```

## Releasing

Set the version in `app/internal/version/version.go`, write `docs/release-notes/v<version>.md`, merge to `main`, then push the tag `v<version>`. GitHub Actions tests, builds, runs the end-to-end update test and publishes the release; installed copies learn about it from the release's `manifest.json`.

## License

See [LICENSE](LICENSE).
