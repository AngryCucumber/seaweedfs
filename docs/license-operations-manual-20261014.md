# MassDB OSS 授权证书运维手册

日期：2026-10-14。适用于 `3.84-license` 分支 `79aa9b1d1` 及之后的构建（该提交修复了 goraft 多 master 全部重启后集群无法恢复的问题，更早的构建不要用于多 master 部署）。设计与边界见[执行计划](license-certificate-execution-plan-20261013.md)与 [P0 契约](license-p0-contract-20261013.md)，两者冲突时以契约为准。

## 1. 证书管什么

- **只管“读数据”。** 证书无效时，volume 与 filer 的 HTTP `GET` 文件内容、S3 `GetObject`、webdav 下载、`weed mount` 读文件、volume 的 7 个原始数据 gRPC 被拒绝；写入、删除、元数据查询（`HEAD`、`?metadata=true`、目录列表、S3 `ListObjects`/`HeadObject`）照常可用。filer 自身的维护性读取（manifest 解析、小块合并、元数据日志回放、系统配置文件）经内部令牌放行，不受影响。
- **管节点数量。** 证书写明 master、volume server、filer 三类节点的上限。超额的新节点注册被拒，被拒节点本地读取返回 403。**从未导入证书时，volume 与 filer 的额度为 0**，只能启动单 master。
- **master 是唯一管理点。** 证书只在 leader master 导入，自动同步到其它 master，再随心跳下发到 volume 与 filer；节点用内置公钥验签后缓存。
- **证书到期不踢节点。** 已注册的节点不会因到期被移除；到期后沿用最后一张证书的额度约束新注册。
- 证书本身不是数据保密手段：数据文件不加密，有机器权限的人能绕过（见计划 1.3）。

## 2. 关键概念与文件

| 项 | 说明 |
| --- | --- |
| `cluster_id` / `deployment_id` | 集群标识（UUID）。由 `weed license.init` 或首个 leader 生成；证书里的 `deployment_id` 必须等于它 |
| `sequence` | 证书序号，续期必须严格递增；同序号不同内容会被拒绝 |
| `not_before` / `expires_at` | 生效与到期时间（UTC 秒）；到期那一秒起即失效，无宽限 |
| `newest` / `prev` | 导入一张尚未生效的证书时，旧证书作为 `prev` 继续生效到新证书的 `not_before`；期间额度取两者较小值 |
| 宽限期 | 节点断开后 **10 分钟**内仍占名额，期间重连不会被别人挤掉 |
| `<mdir>/license/state.json` | master 的授权状态（证书、`cluster_id`、内部令牌、成员账本）。权限 0600，目录 0700 |
| volume 第一个 `-dir` 下的 `license-cache.json` | volume 的证书缓存，重启后不等 master 即按缓存放行 |
| filer `-defaultStoreDir` 下的 `license-cache.json` | filer 的证书缓存 |

## 3. 部署要求（上线前逐条确认）

1. **持久化并备份 `<mdir>`。** `weed master` 的 `-mdir` 默认是系统临时目录，重启后可能被清空，集群会回到“无证书、volume 与 filer 额度为 0”的状态。master 启动时发现 `-mdir` 在临时目录下会打 warning。`<mdir>/license/` 随 `<mdir>` 一起备份。
2. **持久化 volume 的第一个 `-dir` 与 filer 的 `-defaultStoreDir`**，否则节点重启后要等 master 首帧才能放行读取。
3. **升级顺序：先升级全部 master，再同批升级全部 volume 与 filer。** 旧版 volume 与 filer 能注册到新 master 并占名额，但不执行授权检查；证书无效时旧 filer 还读不到新 volume 上的数据（返回 500），混部比全旧或全新都差（计划 8.5 的 S14）。
4. **master 之间的地址。** master 之间的授权 RPC 只接受来自 raft 成员、`-peers` 与本机地址的调用。master 经 NAT、另一块网卡或 Kubernetes 非 headless Service 互连时，对端看到的来源 IP 与登记地址不同，须在每台 master 的 `master.toml` 中补充：

   ```toml
   [master.license]
   peer_ips = "10.0.0.0/24, 192.168.1.20"
   ```

   漏配的后果是新 leader 对账拿不到多数派、一直处于 `RECONCILING`，所有 volume 与 filer 注册被延后（第 12 节）。
5. **时钟。** master 与各节点开启 NTP。master 墙钟回拨超过 5 秒会进入 `CLOCK_SUSPECT`（全集群拒读，第 12 节）；节点时钟比 master 快 5 分钟以上会比 master 早判定到期。
6. **`-saveToFilerLimit` 建议为 0。** 大于 0 时小文件正文存在 filer 元数据里，证书无效时这些正文对元数据接口隐藏，filer 启动时会打 warning。
7. **容器里的独立 `weed s3` 加 `-ip.bind=0.0.0.0`**（上游默认只监听 localhost）。
8. gRPC mTLS 与 HTTPS 可选，见第 11 节。

## 4. 首次部署

### 4.1 单 master（含 `weed server`）

无证书也能启动，但 volume 与 filer 在导入前注册被拒（filer 端口不监听，volume 读取 403 `NODE_REJECTED`）。

1. 启动 master，取申请文件：

   ```bash
   curl -s http://<master>:9333/license/deployment > request.json
   ```

2. 把 `request.json` 交给签发方，拿回证书（如 `cert.jws`）。
3. 导入（第 6 节）。volume 与 filer 在下一次重试（约 5 秒内）自动注册。

`weed server` 相同；`weed server` 用 `-master.license.file=<证书>` 预置证书。评估与开发环境也需要证书，可签发短期试用证书。

### 4.2 多 master

无证书时多 master 不会组建集群（进程以 `license: NODE_LIMIT_EXCEEDED master <n>/0` 退出），所以先离线取证：

1. 在**任一台** master 上，用新版二进制对它的 `-mdir` 生成 `cluster_id` 并打印申请：

   ```bash
   weed license.init -mdir=/data/master
   ```

2. 在**其余每台**写入同一个 id（取上一步输出的 `deployment_id`）：

   ```bash
   weed license.init -mdir=/data/master -clusterId=<deployment_id>
   ```

3. 用第 1 步输出的申请签发证书，把证书放到每台 master 上。
4. 各台以 `-license.file` 启动：

   ```bash
   weed master -mdir=/data/master -peers=<...> -license.file=/etc/massdb/cert.jws
   ```

`-license.file` 只在启动时把证书写入本地状态，已应用或已被更高序号取代的文件会被跳过；之后续期一律走导入（第 6 节），不必改启动参数。`-peers` 的数量不能超过证书的 `max_master_nodes`。

docker-compose 参考 [local-license-compose.yml](../docker/compose/local-license-compose.yml)，文件头写有同样的步骤（`docker compose run --rm master0 license.init -mdir=/data` 等）。

## 5. 申请与签发

- 申请内容是 JSON：`schema_version`、`product`（`MassDB OSS`）、`deployment_id` 与当前已注册的三类节点数。来源二选一：`GET /license/deployment`，或 `weed license.init` 的输出。
- 签发由独立的签发项目完成（项目待定，计划 7.3）。签发方按申请中的 `deployment_id` 填证书，并确定额度（`max_master_nodes`、`max_volume_servers`、`max_filers`）、有效期与 `sequence`。
- 新额度不能小于集群当前账本里的节点数（在线 + 宽限），否则导入失败（第 7 节）。

## 6. 导入、校验与查看

导入只在 leader 上执行。在 follower 上导入返回 503 `NOT_LEADER`，响应里的 `leader` 字段给出 leader 地址；`weed shell` 会自动连到 leader。

```bash
# 先校验，不落盘（每个 master 每分钟最多 10 次）
curl -s --data-binary @cert.jws http://<leader>:9333/license/validate
# 导入
curl -s --data-binary @cert.jws http://<leader>:9333/license/import
```

也可以用 `weed shell`：

```bash
echo "license.validate -file=cert.jws" | weed shell -master=<master>:9333
echo "license.import -file=cert.jws" | weed shell -master=<master>:9333
echo "license.show" | weed shell -master=<master>:9333
```

请求体可以是证书原文、`{"certificate":"<证书>"}` 或 multipart 的 `file` 字段，上限 96 KB。

导入成功返回 200，主要字段：

| 字段 | 含义 |
| --- | --- |
| `accepted` / `unchanged` | 是否接受；`unchanged=true` 表示同一证书重复导入，只重新推送 |
| `effective_now` | 是否立即生效（`not_before` 在未来则为 false） |
| `voters_ok` / `peers_failed` | 已确认的 master 与推送失败的 master |
| `changes` | 这张证书相对当前生效证书拿走了什么，见下表；为空表示没有缩减。校验与导入返回的内容相同 |

`changes` 的每一项都只是陈述事实，不会阻止导入：

| `code` | 含义 | 附带字段 |
| --- | --- | --- |
| `EXPIRY_SHORTENED` | 到期时间早于当前证书 | `previous_expires_at`、`expires_at` |
| `COVERAGE_GAP` | 当前证书到期后，这张证书还要再过一段时间才生效，期间读取按 `EXPIRED` 被拒 | `gap_seconds` |
| `MASTER_LIMIT_REDUCED` / `VOLUME_SERVER_LIMIT_REDUCED` / `FILER_LIMIT_REDUCED` | 该类节点额度低于当前证书 | `previous_limit`、`limit` |
| `PENDING_DISCARDED` | 已导入但尚未生效的证书会被这张证书取代 | `license_id`、`sequence` |
| `FEATURES_REMOVED` | 去掉了当前证书的功能（去掉 `DATA_READ` 后状态为 `FEATURE_MISSING`） | `features` |

**导入前先校验，确认 `changes` 符合预期。** shell 会在结果后另起一行 `changes: ...`；master 页面点 Import 时会先校验，在确认框里列出这些变化。

导入返回 503 `PARTIAL_COMMIT` 时，证书只在不足半数的 master 上落盘：等故障 master 恢复后**重新导入同一个文件**即可（幂等）。

查看状态：`GET /license`（或 `license.show`、master 页面的 License 区块）。每台 master 返回自己的视图，主要字段：

| 字段 | 含义 |
| --- | --- |
| `status` | 授权状态，见附录 A |
| `role` / `leader` | 本 master 的角色与当前 leader |
| `cluster_id`、`trust_root` | 集群标识；`trust_root` 为 `production` 或 `test`（测试根构建） |
| `effective` / `newest` / `prev` | 当前生效、最新、过渡期旧证书的摘要（不含证书原文） |
| `expires_at`、`days_remaining` | 生效证书的到期时间与剩余天数 |
| `nodes` | 三类节点的 `online`、`grace`、`limit` |
| `grace_nodes` | 宽限中的节点：`type`、`address`、`last_seen`、`release_at`（第 7 节） |
| `voters_acked` / `voters_pending` | 只在 leader 上有值：已确认、未确认当前授权状态的 master |

导入后 volume 与 filer 在一个心跳周期（约 5 秒）内生效。

## 7. 续期、降额与释放名额

- **续期**：签发 `sequence` 更大的证书并导入。建议在 `days_remaining` 低于 30 时续期。序号更大的证书总是整体取代当前证书：期限可以更短、额度可以更低、功能可以更少，只要额度容纳当前账本（见下条）就接受，校验与导入的 `changes` 会列出这些变化（第 6 节）。导入错了只能再签一张序号更大的证书纠正，被取代的旧证书不能再导入（`LICENSE_SEQUENCE_NOT_INCREASING`）。
- **提前签发**：`not_before` 在未来的证书可以提前导入，到点各节点按本地时钟自动切换，不需要再操作；期间额度取新旧证书的较小值，所以额度更低的未来证书导入后立即限制新加入的节点。若之后又导入一张序号更高的证书，它会整体替换前两张（`changes` 里的 `PENDING_DISCARDED`）。注意与 MassDB SQL 不同：MassDB SQL 拒绝额度更低的未来证书，OSS 接受。
- **降额**：新证书任一类额度小于当前账本里该类的节点数（在线 + 宽限）时，导入返回 409 `LICENSE_NODE_LIMIT_TOO_SMALL`，原证书不变。先下线多余节点，等宽限期过去或手动释放，再导入。
- **换钥**：公钥编译在二进制里，换钥只能发新版本。新公钥先随一个版本发布，所有可能回退到的版本都包含新公钥后，才改用新私钥签发；旧公钥在不再有版本或证书依赖它之后才能移除。
- **释放宽限中的名额**：节点下线后 10 分钟内仍占名额。要立即释放，在 leader 上：

  ```bash
  curl -s -X DELETE http://<leader>:9333/license/nodes/<ip:port>
  ```

  宽限中的节点列在 `GET /license`（或 `license.show`）的 `grace_nodes` 里，每项有 `type`、`address`、`last_seen` 与自动释放时间 `release_at`；master 页面 License 区块的 “In grace” 行也会列出。地址是节点注册时的 `ip:port`：volume 为心跳里的地址（与 `volume.list` 中 DataNode 的地址相同），filer 为它的 HTTP 地址。仍在线的节点返回 409 `NODE_ONLINE`，账本里没有的地址返回 404 `NODE_NOT_FOUND`。

## 8. 证书无效时各接口的表现

| 接口 | 表现 |
| --- | --- |
| volume / filer HTTP `GET` 文件内容（含 Range） | 403，`Content-Type: application/json`，body `{"error":"license_denied","reason":"<原因>","expires_at":<秒>,"cluster_id":"<id>"}`，响应头 `X-MassDB-License-Reason: <原因>` |
| S3 `GetObject` | 403 标准 `AccessDenied` XML，同样带 `X-MassDB-License-Reason` |
| S3 `CopyObject` | 源读取被拒，返回 `InvalidCopySource`（本版本文案为 `InvalidArgument`），目标不变 |
| webdav 下载 | 连接中断，不返回内容 |
| `weed mount` 读文件 | `EIO`，不会返回全零；续期后已打开的文件自动恢复（最长约一个心跳周期加 5 秒） |
| 写入、删除、`HEAD`、`?metadata=true`、目录列表、S3 List/Head | 正常 |
| 小文件内联正文（`-saveToFilerLimit>0`） | 元数据接口与 filer gRPC 中隐藏，`FileSize` 保留，HTTP 带 `X-MassDB-Content-Hidden` |
| `filer.sync`、`filer.backup`、`filer.meta.backup`、`filer.remote.sync`、`filer.remote.gateway` | 读取数据被拒；遇到隐藏正文的事件停在原位置，续期后从该位置续传 |
| `fs.cat`、`filer.cat`、`fs.meta.save` | 报 `CONTENT_HIDDEN` 或 403，不输出不完整的内容 |
| `volume.fsck` | 含 manifest 的文件无法解析时中止并报错（避免把在用数据误判为孤儿），续期后再运行 |
| `weed backup` | 报 `license: NO_LICENSE` 等原因 |
| `volume.move`、`ec.encode`、`ec.balance` 等维护命令 | 正常（数据在 volume 之间用内部令牌传输） |

## 9. 存量集群升级（从不含证书功能的版本）

新版 master 没有证书时，**所有** volume 与 filer 都无法重新注册，写入完全中断。所以必须先取证再升级：

1. 停机前（或滚动前），用**新版二进制**对每台 master 的 `-mdir` 执行 `license.init`：一台生成，其余用 `-clusterId` 写入同一 id（同 4.2 第 1、2 步）。旧数据不受影响。
2. 用申请签发证书，放到每台 master 上。
3. 以新版二进制加 `-license.file` 启动全部 master。leader 对账完成后（`GET /license` 显示 `VALID`）才开放注册。
4. 同批升级全部 volume 与 filer。它们在下一次心跳时注册。
5. 用 `GET /license` 确认 `nodes` 的在线数与实际一致，读取正常。

注意：

- 节点数超过证书额度的部分在升级后注册被拒，签发前按实际规模确认额度。
- 把 master 回退到旧版不会撤销授权：已有缓存的新版节点按缓存证书继续放行，直到缓存证书到期；没有缓存的新版节点为 `NOT_READY`（拒读）。

## 10. master 成员变更

- **hashicorp raft（`-raftHashicorp`）**：新 master 先以 nonvoter 加入，同步授权与账本后再提升为 voter；超过 `max_master_nodes` 的加入被拒。移除 voter（`cluster.raft.remove` 等）前会先把当前授权状态重新提交到剩余成员的多数派，做不到则拒绝移除。
- **goraft（默认，静态 `-peers`）**：成员只能改 `-peers` 重启。**缩容前**先在 leader 上看 `GET /license` 的 `voters_acked`，确认要保留的 master 都在其中，否则可能回退到旧证书。
- goraft 每次启动都清空 raft 日志与配置，所以 goraft 的每台 master 每次启动都要求本地已有证书（`state.json` 里已有，或带 `-license.file`）。
- goraft 全部 master 同时重启后，选主约 15–20 秒，再完成对账后开放注册；docker-compose 实测从启动到全部节点注册约 23 秒。

## 11. 集群内传输加密（可选）

不开 mTLS 不影响证书的生效与拦截。需要防止集群内第三方截获数据或内部令牌时，在 `security.toml` 中同时配置：

- `[grpc]` 的 `ca` 与 `[grpc.master]`、`[grpc.volume]`、`[grpc.filer]`、`[grpc.client]` 等的证书（gRPC mTLS）；
- `[https.volume]` 与 `[https.client]`：filer 读 volume 的内部令牌走 HTTP 请求头，只开 gRPC mTLS 保护不到它。

## 12. 故障排查

授权相关日志都以 `license:` 开头，日志里不会出现证书原文或内部令牌。

| 现象 | 原因与处理 |
| --- | --- |
| master 启动即退出：`-license.file: open ...: no such file or directory` | 证书路径错误或未挂载 |
| master 启动即退出：`this master has no cluster_id; run weed license.init ...` | 该台没做 `license.init`，按 4.2 补做（`-clusterId` 用证书里的 `deployment_id`） |
| master 启动即退出：`license: NODE_LIMIT_EXCEEDED master <n>/<m>` | `-peers` 数超过证书的 `max_master_nodes`，或本地没有证书却配置了多个 `-peers` |
| `GET /license` 长时间为 `RECONCILING`，volume 与 filer 注册不上 | leader 联系不到多数派 master。看 leader 日志 `license: reconcile ...`；若对端日志有 `refused a master-to-master license RPC from <ip>`，把该 IP 加入对端的 `[master.license] peer_ips`（第 3 节第 4 条）。构建早于 `79aa9b1d1` 的 goraft 多 master 在全部重启后会一直停在这里，须升级 |
| volume 日志 `license: master ... refused this volume server: NODE_LIMIT_EXCEEDED`，或 master 日志 `license: refused volume_server <addr>: NODE_LIMIT_EXCEEDED volume_server <n>/<m>` | 名额用尽（含宽限中的节点）。按 `GET /license` 的 `grace_nodes` 释放已下线节点的名额（第 7 节），或签发更高额度；消息含 `no license imported` 时先导入证书 |
| filer 启动后 HTTP 端口一直不监听 | 冷启动即被拒，卡在等待 master；看 filer 日志 `license: master refused this filer`，处理同上 |
| 读取 403，原因 `NO_LICENSE` | 从未导入证书，导入即可 |
| 读取 403，原因 `EXPIRED` | 证书到期，导入续期证书 |
| 读取 403，原因 `NOT_YET_VALID` | 证书尚未生效且没有可用的旧证书；等到 `not_before` 或签发立即生效的证书 |
| 读取 403，原因 `FEATURE_MISSING` | 证书不含 `DATA_READ`，重新签发 |
| 读取 403，原因 `CLOCK_SUSPECT` | leader 墙钟比它记录的最大时间（水位）落后超过 5 秒。先校正时钟，墙钟重新超过水位后自动恢复；若无法等待，导入一张 `sequence` 更高且 `issued_at` 不晚于当前时间的证书完成修复 |
| 读取 403，原因 `NOT_READY` | 节点没有缓存且还没收到 master 首帧：master 不可达，或 master 仍是旧版 |
| 读取 403，原因 `NODE_REJECTED` | 该节点超额被拒，见上面的 `NODE_LIMIT_EXCEEDED` |
| 导入 503 `NOT_LEADER` | 发到了 follower，改发到响应里 `leader` 给出的地址 |
| 导入 503 `PARTIAL_COMMIT` | 多数 master 不可达，恢复后重新导入同一文件 |
| 导入 400 `LICENSE_DEPLOYMENT_MISMATCH` | 证书不是给本集群签的（`deployment_id` 不符） |
| 导入 400 `LICENSE_SIGNATURE_INVALID` / `LICENSE_UNTRUSTED_KID` / `LICENSE_PRODUCT_MISMATCH` / `LICENSE_MALFORMED` / `LICENSE_EXPIRED` | 证书被改动、签发密钥不受本版本信任、产品不符、格式错误或已过期；联系签发方 |
| 导入 409 `LICENSE_SEQUENCE_NOT_INCREASING` / `LICENSE_SEQUENCE_CONFLICT` | 序号没有增加，或同序号内容不同；联系签发方重新签发 |
| 导入 409 `LICENSE_NODE_LIMIT_TOO_SMALL` | 新额度小于当前节点数，见第 7 节 |
| 校验 429 `RATE_LIMITED` | 每分钟超过 10 次校验，稍后再试 |
| S3 在容器外访问不到 | 独立 `weed s3` 未加 `-ip.bind=0.0.0.0` |
| 部分旧版 filer 读取返回 500 `read chunk ... 403` | 新旧版本混部且证书无效，按第 3 节同批升级 |

**监控指标**（只在 master 导出，每 5 秒按本 master 的 `GET /license` 视图刷新；带仓库统一前缀 `SeaweedFS_`）：

| 指标 | 说明 |
| --- | --- |
| `SeaweedFS_license_state{reason}` | 当前状态为 1，其余为 0 |
| `SeaweedFS_license_expires_at_seconds` | 生效证书的到期时间，无证书为 0 |
| `SeaweedFS_license_nodes_online{type}`、`..._grace{type}`、`..._limit{type}` | 三类节点的在线、宽限与上限 |
| `SeaweedFS_license_denied_reads_total{server,path}` | volume 与 filer 上被拒绝的读取次数 |

建议告警：`SeaweedFS_license_state{reason="VALID"} == 0`；`SeaweedFS_license_expires_at_seconds - time() < 30*86400`；`SeaweedFS_license_nodes_online + SeaweedFS_license_nodes_grace >= SeaweedFS_license_nodes_limit`（名额用尽）。

## 13. 状态目录恢复

- **单台 master 丢失 `<mdir>/license/`**：用证书里的 `deployment_id` 执行 `weed license.init -mdir=<dir> -clusterId=<deployment_id>`，再带 `-license.file` 启动（goraft 每次启动都要求本地已有证书）。它加入后，leader 会把最新的授权状态与账本推送给它。
- **全部 master 丢失**：用证书里的 `deployment_id` 在每台执行 `weed license.init -mdir=<dir> -clusterId=<deployment_id>`，再以 `-license.file` 启动。成员账本随 volume 与 filer 重新注册重建。丢失状态不会换来免费额度（无证书时额度为 0）。

## 附录 A：授权状态（`GET /license` 的 `status`、读取 403 的 `reason`）

| 码 | 出现位置 | 含义 |
| --- | --- | --- |
| `VALID` | master、节点 | 生效证书在有效期内且含 `DATA_READ` |
| `NO_LICENSE` | master、节点 | 从未导入证书 |
| `NOT_YET_VALID` | master、节点 | 证书尚未生效且没有可用的旧证书 |
| `EXPIRED` | master、节点 | 生效证书已到期 |
| `FEATURE_MISSING` | master、节点 | 证书不含 `DATA_READ` |
| `CLOCK_SUSPECT` | master、节点 | master 墙钟回拨超过 5 秒，经 master 下发 |
| `RECONCILING` | master | 新 leader 正在与其它 master 对账；期间注册与管理操作延后，不影响节点读写 |
| `NOT_READY` | 节点 | 没有缓存，且尚未收到 master 首帧 |
| `NODE_REJECTED` | 节点 | 本节点超额被拒 |

## 附录 B：管理接口错误码

响应 `{"error":"<码>","message":"..."}`；gRPC 与 `weed shell` 返回同样的码。

| HTTP | 码 |
| --- | --- |
| 400 | `LICENSE_MALFORMED`、`LICENSE_UNTRUSTED_KID`、`LICENSE_SIGNATURE_INVALID`、`LICENSE_PRODUCT_MISMATCH`、`LICENSE_DEPLOYMENT_MISMATCH`、`LICENSE_EXPIRED` |
| 403 | `NODE_LIMIT_EXCEEDED`（节点注册） |
| 404 | `NODE_NOT_FOUND` |
| 405 | `METHOD_NOT_ALLOWED`（`GET /license`、`/license/deployment` 只接受 GET；`/license/validate`、`/license/import` 只接受 POST；`/license/nodes/{addr}` 只接受 DELETE） |
| 409 | `LICENSE_SEQUENCE_NOT_INCREASING`、`LICENSE_SEQUENCE_CONFLICT`、`LICENSE_NODE_LIMIT_TOO_SMALL`、`MEMBERSHIP_CHANGED`（成员变更与导入交错，重试即可）、`NODE_ONLINE` |
| 413 | `REQUEST_TOO_LARGE` |
| 429 | `RATE_LIMITED` |
| 503 | `NOT_LEADER`（带 `leader`）、`RECONCILING`、`PARTIAL_COMMIT`（带 `voters_ok`、`peers_failed`） |
