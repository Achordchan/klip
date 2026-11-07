# Klip 云同步后端

跑在 Cloudflare 免费版上的 Worker，提供账号、订阅与卡密、快照与 blob 同步、推送和后台管理。接口沿用旧版，客户端不用改。

## 结构

| 组件 | 用途 |
|---|---|
| Worker（`src/index.ts`） | 路由、JWT 校验、读写 D1、直接从 R2 返回文件 |
| D1 `klip-sync` | 用户、刷新令牌、卡密、订阅、验证码（表结构见 `migrations/`） |
| R2 `klip-sync` | 快照（每个版本一个文件，旧版本就是备份）、blob、manifest |
| Durable Object `UserStore`（`src/user_store.ts`） | 每个用户一个，负责密码哈希、上传的解压和校验、备份清理、用量记账，以及推送 |
| 静态资源（`public/`） | 后台页面 `/admin` |

**为什么要 Durable Object：** 免费版 Worker 每个请求只有 10ms CPU。实测在 Worker 里做密码哈希（PBKDF2-SHA512 10 万次），30 次有 11 次超限；解压并校验 20MB 上传，8 次全部超限。同样的活放在 DO 里全部成功，因为 DO 每个请求有 30 秒 CPU。所以重活都交给 DO，Worker 只做便宜的事。

**容量：** 快照、备份和 blob 都算进档位容量。上传过程中逐块检查，同一用户的并发上传一起算；超额就不写入 R2，返回 409 `storage_limit_exceeded`（单个快照本身就超过档位上限时返回 413 `snapshot_too_large`）。备份不算“占着的”：判断新上传能不能放下时不计备份，新快照发布后会从最旧的开始清理备份，直到不超额。快照放不下时，当前快照、manifest 和备份都保持原样。

**推送：** `/sync/events` 仍是 SSE。Worker 在用户的 DO 上挂一条 WebSocket（Hibernation API，空闲时 DO 休眠，不计时长），收到更新就以 SSE 写给客户端。每条连接最长 15 分钟，到时正常结束，客户端会立刻重连并顺带同步一次。

## 首次部署

需要 Node。以下命令都在 `server/` 目录里运行。

```bash
npm install
npx wrangler login
```

创建数据库和存储桶：

```bash
npx wrangler d1 create klip-sync
```

把输出里的 `database_id` 填进 `wrangler.jsonc` 里 `d1_databases` 那一项的 `database_id`，替换掉现在的值（仓库里填的是 Klip 线上用的数据库，换个账号部署时必须改）。

```bash
npx wrangler r2 bucket create klip-sync
```

```bash
npx wrangler d1 migrations apply DB --remote
```

设置机密（每条命令会提示输入值）：

```bash
npx wrangler secret put AUTH_SIGNING_KEY
```

```bash
npx wrangler secret put ADMIN_PASSWORD
```

```bash
npx wrangler secret put SMTP_PASSWORD
```

- `AUTH_SIGNING_KEY`：签发令牌的密钥，用一串足够长的随机字符，比如 `openssl rand -base64 48` 的输出。换掉它会让所有人重新登录。
- `ADMIN_PASSWORD`：后台登录密码，后台邮箱填在 `wrangler.jsonc` 的 `ADMIN_EMAIL` 里。
- `ADMIN_KEY`（可选）：后台的固定密钥，用来写脚本调后台接口。不设就只能用邮箱和密码登录后台。
- 邮件：`wrangler.jsonc` 里填 `SMTP_HOST`、`SMTP_PORT`、`SMTP_USER`、`SMTP_FROM`。465 端口设 `SMTP_SSL` 为 `true`；587 端口设 `SMTP_SSL` 为 `false`、`SMTP_STARTTLS` 为 `true`。Cloudflare 不允许连 25 端口。

部署：

```bash
npx wrangler deploy
```

`wrangler.jsonc` 里的 `routes` 会把 Worker 绑到 `klip.achord.cn`，Cloudflare 自动建 DNS 记录和证书（`achord.cn` 需要托管在同一个 Cloudflare 账号下）。

## 日常

- 改了代码：`npx wrangler deploy`
- 加表或改表：在 `migrations/` 里新增一个编号更大的 `.sql` 文件，然后跑 `npx wrangler d1 migrations apply DB --remote`
- 看线上日志：`npx wrangler tail`
- 类型检查：`npm run typecheck`

## 本地运行与测试

```bash
npx wrangler d1 migrations apply DB --local
```

```bash
npx wrangler dev --var ADMIN_KEY:dev-key --var AUTH_SIGNING_KEY:dev-signing-key-long-enough
```

客户端的集成测试会自己启动这个 Worker（临时数据目录、随机端口），把真实的客户端同步流程完整走一遍：

```bash
cd ../app && KLIP_TEST_SERVER=1 go test ./...
```

## 免费额度要留意的地方

- **请求数**：Worker 和 DO 各 10 万次/天。一台一直在线的设备每天大约用掉几百次（推送每 15 分钟重连一次，每次顺带查一次 manifest），所以同时在线的设备数大约在两三百台以内。
- **存储**：R2 免费 10GB 是整个账号共用的（和其他桶一起算），超出按 $0.015/GB·月计费。Pro（2GB）和 Ultra（10GB）档位只要有一两个用户用满，就会超出免费额度。
- **单次上传**：Cloudflare 免费版的请求体上限是 100MB，超过 100MB 的快照会被拒绝（413）。
- **中国大陆访问**：免费版在大陆没有节点，请求会绕到海外，延迟偏高。
- **防刷**：每次登录（包括密码错误）都要算一次密码哈希，会占用一次 DO 请求。有人恶意刷 `/auth/login` 就可能耗光当天额度。建议在 Cloudflare 后台给 `klip.achord.cn/auth/*` 加一条限速规则，免费版自带一条。

部署后可以用客户端的集成测试验收线上环境（先临时设置一个 `ADMIN_KEY`，测试会在线上建几个测试账号）：

```bash
cd ../app && KLIP_TEST_SERVER_URL=https://klip.achord.cn KLIP_TEST_ADMIN_KEY=<ADMIN_KEY> go test ./internal/syncclient/ ./internal/syncer/
```
