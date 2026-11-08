# Klip 云同步后端

跑在 Cloudflare 免费版上的 Worker，提供账号、订阅与卡密、快照与 blob 同步、推送和后台管理。接口沿用旧版，只多了上传快照时声明引用的 blob（见“blob 回收”），旧客户端照常能用。

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

## blob 回收

图片和文件（blob）按哈希存，多份快照可以共用。服务端看不懂快照内容，所以由客户端在同步时声明“这份快照引用了哪些 blob”，服务端据此回收没人用的 blob：

1. 客户端查缺失 blob（`POST /sync/blobs/check`）时交上的哈希列表，就是它接下来要上传的快照引用的全部 blob。Worker 每 2000 个一批转给 DO，各批记进同一条**引用声明**；响应里多给一个 `refsToken`。声明在 24 小时内保护列表里的 blob：查完说“已经有了”的 blob，不会在这台设备传完快照之前被回收。
2. 客户端上传快照（`PUT /sync/snapshot`）时带请求头 `x-klip-blob-refs: <refsToken>`；快照里没有 blob 时带 `x-klip-blob-refs: none`，不用先查。
3. 服务端为每份保留着的快照（当前、备份、正在发布）记下它引用的 blob。一个 blob 同时满足下面几条才会被回收：没有任何保留快照引用它、没有未过期的声明引用它、没有正在进行的上传、上传超过 24 小时（宽限期）。回收时从账目里去掉、用量随之下降，R2 里的文件随后删除。
4. 引用变少（备份被清理、发布失败撤销）后，大约 10 分钟内做一次回收，把一段时间内的变化攒在一起处理；宽限期没到的 blob，到期时再用 alarm 处理。
5. 一份快照被删之后，它独占的 blob 再保留 24 小时（`BLOB_GC_DROP_HOLD_SECONDS`）：可能还有设备刚下载了这份快照，正在一个个取附件。删掉的是最后一份“引用未知”的快照时，整个账号的回收推迟 24 小时。
6. 上线前就有的 blob，上传时间按这版第一次启动的时间算，过了宽限期会安排一次回收，从来没被快照用到的会被收掉。

**存储代数：** 每个用户有一个代数，每次回收已记账的 blob、每次管理员清空都加一。声明记下查缺失时的代数，上传快照时代数已经变了（查完之后有 blob 被删过，“已经有了”的结论可能不再成立），就拒绝上传。

上传快照可能遇到的 409（客户端遇到这几种都会重新查缺失、补传 blob 后再上传一次）：

| code | 含义 |
|---|---|
| `storage_generation_stale` | 查缺失之后有 blob 被删过（回收或清空）；不带声明的上传在刚清空过时也是这个（见下面“旧客户端”） |
| `blob_refs_expired` | 声明找不到或过了 24 小时 |
| `blob_refs_missing` | 声明里有服务端还没有的 blob |

查缺失本身也可能返回 409 `blob_check_interrupted`：分批记声明时声明中途没了，不应该发生，让客户端重查。

声明不计入档位容量，所以设了上限：一个账号同时最多 200 条、8 MiB（声明自己的哈希列表），超出时从最旧的声明开始作废；一次查缺失最多 25 万个哈希，超过返回 413 `blob_check_too_large`。

列表超过 6 万个哈希（一份放不下）时声明不收成一份，token 照给、代数照样核对，但这份快照按下面说的“引用未知”记账。

**备份也算引用：** 用户删掉附件并同步后，之前的快照变成备份，它还引用着那些 blob，所以要等这份备份过了档位的保留期（Mini 30 天、Plus 90 天、Pro 365 天、Ultra 730 天）被清理之后，附件才会被回收、用量才会下降。还有一个已知的卡住情形：只被备份引用的 blob 把容量占满时，新的 blob 上传会返回 409，没有上传成功就不会触发备份清理（备份只在提交后、且总用量超过档位时才按超额清理），只能等保留期到或者管理员清空。要不要在容量不够时优先清理备份来腾出它们独占的 blob，留待以后决定。

**旧客户端：** 这里指已经连新后端、但还没用上引用声明的 Go 客户端；已发布的 C# 客户端（1.0.10 及更早）连的是旧服务器 maccy.baymaxgroup.com，不会访问这里。不带 `x-klip-blob-refs` 上传的快照“引用未知”：只要还保留着任何一份这样的快照（包括它变成的备份），这个用户的 blob 就一个也不回收。上线前已有的快照全都是“引用未知”，所以对老用户来说，回收要等这些快照被新快照替换、作为备份过了保留期（或因超额被清理）之后才开始。

旧客户端的上传不看代数。回收不会让它的快照悬空：它查缺失时同样会记下声明，声明有效期内不会回收它查到的 blob。清空会：它可能清空前查过（结论是“都在”）、清空后才上传。服务端按调用方（访问令牌的哈希，每台设备、每次刷新各不相同）把它的查缺失和上传对上：这台设备最近一次查缺失在最近一次清空之前，就拒绝这次上传（409 `storage_generation_stale`），同时删掉这条检查记录。每台设备最近一次检查的代数单独记着，不随声明过期，保留到它的访问令牌过期之后：令牌过期了，上传过不了鉴权。它的这次同步失败，下次同步会重新查，那时就能通过；只有文字、从来不查缺失的客户端不会被拦。旧客户端刷新令牌后会重新查一遍缺失，所以令牌变了也不会让清空前的检查混过去。没有检查记录的令牌，如果是在这个账号开始记录之前签发的（这版上线前），可能查过缺失而没留下记录，清空后保守地拒绝它一次；之后签发、没有记录的令牌说明从没查过（只有文字），直接放行。

管理员可以清空某个用户的云端数据，马上释放空间：

```bash
curl -X POST https://klip.achord.cn/admin/user/reset-storage \
  -H "Authorization: Bearer <ADMIN_KEY 或后台登录得到的令牌>" \
  -d '{"emailOrUserId":"user@example.com"}'
```

先删 manifest，再在一个事务里把账目里的全部快照和 blob 记进待清理表、清空账目、代数加一，然后删 R2 里的文件。删到一半失败或 DO 重启，剩下的会由 alarm 和启动恢复接着删，账目里不会留下已删的文件。之后推送一次更新：客户端发现云端没有 manifest，会把本地的快照和用到的 blob 重新上传，只保留现在还用得到的那部分；其他设备随后下载这份新快照。清空前查过缺失的设备，上传快照时会被拒（新客户端按代数，旧客户端按上面说的调用方），重查、补传后再上传。

几个时间可以用变量调（单位秒，一般不用设）：`BLOB_GC_GRACE_SECONDS`（宽限期，默认 86400）、`BLOB_GC_DELAY_SECONDS`（引用变少后多久回收，默认 600）、`BLOB_GC_DROP_HOLD_SECONDS`（快照被删后它的 blob 再留多久，默认 86400，最多 86400）。`BACKUP_RETENTION_SECONDS` 会覆盖所有档位的备份保留期，只给测试用，线上不要设。

## 密码哈希的限制

密码用 PBKDF2-SHA512、100000 次迭代存储（ASP.NET Core Identity v3 格式）。Workers 运行时的 PBKDF2 单次最多只能算 100000 次迭代，所以迭代次数更高的哈希在这里无法验证，会被当作密码错误。目前库里的哈希都是这个后端自己按 100000 次生成的，不受影响。将来如果从别处导入账号，迭代次数更高的用户需要走“找回密码”，或者由管理员在后台重置密码（`POST /admin/user/reset-password`）。

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
