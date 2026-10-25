# MassDB OSS（SeaweedFS 3.84）授权证书执行计划

日期：2026-10-13，v9（v8 合并八轮外部审核修订；v9 为 P0 契约冻结，冻结细节见 [P0 契约](license-p0-contract-20261013.md)）。源码基线：`claude/authorization-certificate-system-cb1fc3` / `dd3b853cc`（3.84，已含 MassDB OSS 品牌页面）。参考方案：MassDB SQL 的 `docs/license-certificate-execution-plan-20260922.md`、`docs/license-protocol-v1-20260922.md` 及 `tools/license-issuer/`。

**一句话方案：** master 是证书唯一管理点（导入/校验/持久化/多 master 同步/节点额度），证书原文沿既有心跳和 KeepConnected 流下发，volume server 与 filer 各自用内置公钥验签后缓存；只在 volume 和 filer 的 HTTP GET 文件内容处加守卫；filer 自身的维护性读取（manifest 解析、元数据日志回放、小块合并、系统配置文件）通过限定在四个读取函数上的内部令牌在 volume 放行，保证入库、更新、元数据、内部维护不受证书影响。不新增 HTTPS、不改 raft 状态机、不改 gRPC 服务语义。

**v4 相对 v3 的变化（来自第四轮审核）：** 撤销 gRPC `Lookup/List` 的 `Content` 剥离（会导致 S3 IAM 配置读空、标签回写清空内联正文、mount 读到补零数据），改为只在 HTTP `?metadata=true` 与目录 JSON 两个只读接口隐藏并打 `X-MassDB-Content-Hidden` 标记，gRPC 面的内联正文列为边界；授权状态改为 `(newest, prev)` 两证书、生效证书由时间推导，不再有 pending 激活转换，排序只看 `newest.sequence`；选主对账选中的状态必须先写回 voter 多数派才退出 `RECONCILING`；任期校验改为与当前 raft 任期相等；时钟修复同步改为三分支；宽限账本改为“在线 + 宽限”的全量成员快照复制；内部通道补入 `StreamContent`（filer.conf、IAM 配置按 chunks 存储时的读取）并保留加密与重试；补 master/filer/volume 准入的原子预占；S12 拆冷启动与运行中；S13 补大目录、内联文件、加密 manifest/merge、日志回放负载。

**v5 相对 v4 的变化（来自第五轮审核）：** 独立 mount 首次打开 manifest 文件时解析失败只打警告、随后按稀疏文件补零返回，必须让打开失败而不是返回伪造正文，因此 mount 需要约 5 行改动；voter 增删与授权状态转移定义顺序，提交与对账绑定 raft 配置索引（2026-10-25 改为比较配置内容，8.22 方案 3）；`RECONCILING` 期间冻结新地址准入；成员账本改为带确认的 RPC 复制并在选主时对账，撤销“最多丢 5 秒”的硬保证；切主只给原在线节点新宽限，已离线条目保留原截止时间；`StreamContent` 的令牌经私有中间函数传递，用户入口传空；补账本复制的后台成本与测量。

**v6 相对 v5 的变化（来自第六轮审核）：** master 成员变更改为完整的“加入两阶段（nonvoter 追平 → 同步授权与账本 → 提升 voter）”与“移除前先把已确认状态重提交到剩余 voter 多数派”，goraft 静态配置模式只能靠运维检查 `voters_acked`；账本版本改为 `(term, revision)` 跨任期可排序，定义同版本不同内容与迟到批次处理，在途批次至多一个；`RECONCILING` 期间所有注册（含旧地址重连）一律延后，对账与接管转换完成后再开放，避免恢复覆盖刚重连的节点；mount 的 manifest 错误改为在失败状态下限频重解析、成功即清错，并补“保留旧句柄续期恢复”验收。

**v7 相对 v6 的变化（来自第七轮审核）：** 移除 voter 的整个流程（重提交确认到实际移除）与导入、其他成员变更共用 manager 临界区，并在执行前重新核对授权版本与配置索引（2026-10-25 改为配置内容，8.22 方案 3）；统一 `RECONCILING` 语义为“只延后注册与管理操作，节点保留现有授权快照”，撤销向节点下发 `RECONCILING` 拒读原因；S1b 的恢复时间改为“目标 volume 新证书生效后，下一次满足重试间隔的读取触发重解析”；补充 manager 锁内 RPC 截止时间与 mount 重解析并发协调的实现要点。

**v8 相对 v7 的变化（来自第八轮审核）：** 普通账本复制改为“锁内取快照、锁外发 RPC、回来带版本校验”，不在注册共用的 manager 锁内等待网络；只有导入、成员变更与选主对账保留锁内同步 RPC；S13 补“账本 RPC 慢响应或不可达时 200 节点集中重连”的注册延迟验收；1.6 的“5 秒内恢复”统一为 S1b 口径。

**v9 相对 v8 的变化（P0 契约冻结，用户确认第 9 节）：** 决策 2 改为对齐 MassDB SQL 的“无证书不能加入节点”（volume、filer 额度 0，master 只允许单 master 新集群引导，启动前做本地证书检查），补 `weed license.init` 与 `-license.file` 的先取证后部署/升级路径（3.4）；决策 4 改为封堵 volume gRPC 中返回数据正文的 7 个 RPC，volume↔volume 经 gRPC 元数据携带内部令牌（1.2 G2）；决策 9 改为封堵 gRPC 面内联正文，`filer.proto` 加 `content_hidden`，写入按合并规则保留正文，系统目录豁免，filer 之间带令牌，订阅/复制/备份遇隐藏正文停在事件处，mount 返回 `EIO` 并限频重取（1.5）。核对 v8 全部源码引用后修订：M1 补目录条目 `?metadata=true` 分支；F1 前移到 `checkPreconditions` 之前、限 GET、免清理对象头；F2 限 GET；`internal_read_key` 只发给 volume 与 filer；任期改用 `CurrentTerm()`；`-mdir` 默认临时目录告警。合计估时约 20.5 人天。

**P2 完成（2026-10-13）：** master 侧 manager、proto（含 `filer.proto` 字段）、gRPC/HTTP 管理接口、选主对账与写回、成员变更两阶段与移除前重提交、账本复制、`license.init`、`-license.file`、master 启动检查、页面与 shell 命令已实现并经单 master、3 master（goraft 与 hashicorp）、3→5 master 扩容与缩容的真实进程验证；实现与本文的差异及未覆盖项见 8.1。

**P3 完成（2026-10-14）：** 按流下发、volume/filer 守卫（V1、F1、F2、M1）、filer gRPC 内联正文隐藏与写入合并规则（G1）、volume gRPC 数据 RPC 守卫与 volume 之间的令牌（G2）、节点缓存、内部令牌传递链、订阅/备份/读取命令的隐藏正文处理、mount 错误传播、S3 两处已实现，并在 1 master + 2 volume + 3 filer（含 `-s3 -webdav` 同进程）、`weed server -filer -s3 -webdav` 与 Linux 虚拟机（mount、`-cipher`）上做了真实进程验证；P3 中修正的 P2 缺陷与上游问题、与本文的差异及未覆盖项见 8.2。

**P4 完成（2026-10-14）：** volume 心跳与 filer KeepConnected 接入成员账本准入（锁内原子预占、按地址计流的断开宽限、超额拒绝帧）、节点侧 `NODE_REJECTED` 的日志与解除、master 侧其余 5 个指标；master 的三个加入入口、无证书额度 0 与启动检查已在 P2 完成。经单 master、hashicorp 3 master 强杀切主、`weed server` 单进程与基线版本 3 master + 7 volume + 2 filer 升级的真实进程验证；与本文的差异及未覆盖项见 8.3。

**P4 后加固（2026-10-14）：** master 之间的 4 个 RPC 只接受 master 地址的调用，节点时钟以本地墙钟减 5 分钟为下限，均不依赖 TLS；见 1.3、4.3 与 8.4。

**P5 完成（2026-10-14）：** 全量检查（Linux 上 CI 命令全部通过、vet 与基线一致）、S14 新旧混部、`weed server` 一体化、docker-compose、运维手册、外部审核 4 项修复与 S13 性能（新旧版本差异均在噪声内）已完成，见 8.5–8.7；其间修复 goraft 全集群重启后对账卡死，补充宽限节点列表。发布仍需独立签发项目产出的生产信任根（7.3）。

**第十五轮审核（2026-10-22，方案已定、代码未改）：** 对分支全部授权改动做了会话内审查，确认 12 项，均未改代码：5 项 P2（mount 截断到 0 后非隐藏类读失败残留、5 秒内 `EIO`；filer.backup 的 `backup.` 检查点键不在 `KvGet` 白名单，拒读期间回退到 1970；hashicorp raft 的 `GetConfiguration().Index()` 恒为 0，`RemoveMaster` 的 `MEMBERSHIP_CHANGED` 配置检查失效；对端地址校验的 DNS 解析失败无负缓存；leader 墙钟超前经 `SyncLicense` 推高 follower 水位）与 7 项 P3。逐项方案、提交顺序与两处待确认取舍见 8.22；2026-10-24 外部复核修正了其中方案 1、8、9 的缺陷（见 8.22 末尾）。

本文是实施计划，不是功能已完成声明。第 9 节的取舍已于 2026-10-13 由用户确认（决策 2、4、9 改选备选），证书声明、原因码、线格式常量与源码挂点以 [P0 契约](license-p0-contract-20261013.md) 为准。

---

## 1. 范围与边界

### 1.1 保留（证书无效、缺失、过期时继续可用）

| 类别 | 具体路径 | 说明 |
| --- | --- | --- |
| 写入 | volume `POST/PUT/DELETE`，filer `POST/PUT/DELETE`（含 autochunk、cipher、merge），S3 `PutObject/Multipart/DeleteObject`，mount/webdav 写 | merge 的读取经 1.4 内部通道 |
| 更新 | filer `UpdateEntry/AppendToEntry/AtomicRenameEntry`、tagging、S3 `PutObjectTagging/ACL` | `UpdateEntry` 对 manifest 文件的块差集经 1.4 内部通道；回写带 `content_hidden` 的条目按 1.5 合并规则保留原正文 |
| 元数据查询 | filer `HEAD`、`GET ?metadata=true`、目录列表（HTML/JSON）、gRPC `LookupDirectoryEntry/ListEntries/TraverseBfsMetadata/SubscribeMetadata/KvGet`，S3 `HeadObject/ListObjects/ListBuckets`，volume `HEAD`、`/status`、`/ui`，master 全部 HTTP/gRPC | 只看不读内容；HTTP 与 gRPC 对内联正文的隐藏见 1.5；`KvGet` 拒读态只放行同步偏移键 |
| 系统配置 | S3 IAM `/etc/iam/identity.json`、`/etc/seaweedfs/filer.conf`、远端存储映射、熔断配置 | 小文件走 gRPC `Lookup` 的 `Content`（系统目录豁免，不隐藏）；按 chunks 存储时走 `StreamContent`，经 1.4 内部通道 |
| 内部维护 | volume gRPC 编排与本地维护（`VolumeCopy`、`VolumeEcShards*`、`VolumeTailReceiver`、`VolumeTierMove*`、`Vacuum*`）及其触发的 volume↔volume 数据拉取；`volume.fsck`；filer 内部的 manifest 解析、删除回收、元数据日志回放、小块合并、filer 之间的元数据订阅与引导 | volume↔volume 数据 RPC 与 filer↔filer gRPC 带内部令牌（1.2 G1/G2）；filer 内部 HTTP 读取经 1.4 内部通道。证书无效时 shell `volume.check.disk` 与 `weed backup` 被拦（决策 4） |
| 管理 | master `/dir/*`、`/vol/*`、`/col/*`、`/cluster/*`、`weed shell`、UI 页面 | 不变 |

### 1.2 拦截（仅“查数据”）

| 出口 | 挂点 | 覆盖的上层入口 |
| --- | --- | --- |
| V1. volume server HTTP GET 文件内容 | [GetOrHeadHandler](../weed/server/volume_server_handlers_read.go:53) 在 JWT 检查之后、`needle.NewVolumeId(vid)` 之前；仅 `r.Method == GET`；带有效内部令牌（1.4）的请求放行。两个 HTTP handler 都经过此处，`ReadMode=proxy/redirect`、Range、`?readDeleted`、图片缩放裁剪全部覆盖 | mount 读、webdav 读（含 `weed server -webdav` 同进程）、`weed download`、`weed filer.cat`、`fs.cat`、filer 流式读的 chunk 拉取、`filer.backup`/`filer.sync`/`filer.remote.sync` 的数据拉取 |
| F1. filer HTTP GET 文件内容 | [GetOrHeadHandler](../weed/server/filer_server_handlers_read.go:161) 在文件 `?metadata=true` 分支之后、`checkPreconditions` 之前；仅 `r.Method == GET`（v9 前移：v8 放在 HEAD 返回之后，需要先清空已设置的对象头；前移后对象头尚未设置，只有 S3 目录对象可能已设 `X-Seaweedfs-Is-Directory-Key`，拒绝时删除即可）。覆盖 `entry.Content` 内联小文件、分片、分段读、`SeaweedFSPartNumber`、图片缩放（`filer.ReadAll`） | S3 `GetObject`（经 5.2 转换为标准 `AccessDenied`）、S3 `CopyObject` 的源读取、filer 页面下载、`readonlyFilerHandler` |
| F2. filer `/?proxyChunkId=` | [filerHandler](../weed/server/filer_server_handlers.go:66) 调 `proxyToVolumeServer` 前；仅 `r.Method == GET`（该代理按原方法转发，写入与 HEAD 不拦） | 明确 403；只带状态码与原因头、无 body（2026-10-25 修订，8.22 方案 10：该分支在 filer JWT 之前） |
| M1. 三处只读 HTTP 元数据出口隐藏内联正文 | 目录条目 `?metadata=true`（[filer_server_handlers_read.go:124](../weed/server/filer_server_handlers_read.go:124)，v9 补：S3 尾斜杠目录标记对象的 `Content` 从这里返回）、文件条目 `?metadata=true`（[filer_server_handlers_read.go:147](../weed/server/filer_server_handlers_read.go:147)）、目录 JSON（[filer_server_handlers_read_dir.go:63](../weed/server/filer_server_handlers_read_dir.go:63)）：当 `!AllowRead()` 且 `len(Content) > 0` 且不在系统目录豁免内（1.5）时，序列化的副本里 `Content` 置空、`ContentHidden=true`，响应头加 `X-MassDB-Content-Hidden: true`，`FileSize` 保留 | 浏览器、curl 直接查 metadata |
| G1. filer gRPC 返回的内联正文（v9，决策 9） | `LookupDirectoryEntry`、`ListEntries`、`TraverseBfsMetadata`、`SubscribeMetadata/SubscribeLocalMetadata`、`StreamRenameEntry`、`CacheRemoteObjectToLocalCluster` 的出站 Entry，及 `KvGet`；规则见 1.5，挂点见 P0 契约 5.3 | 任意 gRPC 客户端、mount、S3 列表、订阅类命令 |
| G2. volume gRPC 数据 RPC（v9，决策 4） | `CopyFile`（非元数据扩展名）、`ReadNeedleBlob`、`ReadAllNeedles`、`VolumeTailSender`、`VolumeIncrementalCopy`、`VolumeEcShardRead`、`Query` 的 handler 入口；拒读态校验 gRPC 元数据 `x-massdb-internal-read`；volume↔volume 的 3 个出站调用点附加令牌（P0 契约 5.2）。不加拦截器 | `weed shell volume.check.disk`、`weed backup`、直连 gRPC 读取 |

两层都拦的原因：filer 若已开始流式返回 200，再被 volume 拒绝只会得到截断响应；filer 先判断能给出正确状态码。volume 层保证绕过 filer 直连也无法读。F1/F2/M1 不识别内部令牌，只看证书状态；G1 只认 filer 之间调用携带的令牌，G2 只认 volume 之间调用携带的令牌。

拒绝响应：HTTP 403，`Content-Type: application/json`，body `{"error":"license_denied","reason":"<REASON>","expires_at":<unix>,"cluster_id":"..."}`，响应头 `X-MassDB-License-Reason: <REASON>`。mount 对分块文件读到 `EIO`（[weedfs_file_read.go:52](../weed/mount/weedfs_file_read.go:52)）；含 manifest 的文件需要 1.6 的 mount 改动才能保证 `EIO`。拒绝日志限速（每节点每分钟 1 条）。

### 1.3 明确不封堵、作为已知边界

- 内部令牌（1.4）是集群内共享的静态密钥，持有它就能在证书无效时直读 volume。取得途径：master 主机上的 `<mdir>/license/state.json`，或在 master 主机上调用 master 之间的 RPC；`ClientType` 不鉴权，能连 master 且有空闲 filer 名额的客户端可自称 filer 经 `KeepConnected` 拿到；默认部署下令牌以明文出现在 master 下发给 volume 与 filer 的 gRPC 流、filer 读 volume 的 HTTP 请求头 `X-MassDB-Internal-Read`、volume 之间与 filer 之间的 gRPC 元数据中，可被截获。不引入每请求 HMAC。master 之间的 `GetLicense(include_state=true)`、`GetLedger`、`SyncLicense`、`SyncLedger` 原先不鉴权，能连 master 的任何客户端都可直接取得令牌（P3 补记只写了前者，`SyncLicense` 的响应同样带令牌）；2026-10-14 起只接受来源 IP 属于 raft 成员、`-peers` 或 `master.toml` 中 `[master.license] peer_ips` 的调用（8.4）。
- **gRPC mTLS 与 HTTPS 不是证书的部署要求**（2026-10-14 修订，此前写为“部署手册要求开启”）。对被授权方本人，开不开强度相同：集群 CA 与 master 主机都在其手中，上一条的途径照样可用。对没有集群证书的第三方，上游默认 volume 的 GET 不鉴权（`security.toml` 的 `jwt.signing.read` 默认为空），证书有效时本就能直读，截获令牌只是在证书无效时得到同样的读取能力，属于数据保密范畴（见本节“证书不是数据保密方案”）。需要保护集群内传输时，同时开启 gRPC mTLS 与 `[https.volume]`、`[https.client]`：filer 读 volume 的令牌在 HTTP 请求头里，只开 gRPC mTLS 保护不到。运维手册列为可选项。
- 节点以 master 下发的 `master_now_unix_ms` 为时钟锚点，但不早于本地墙钟减 5 分钟（4.3，2026-10-14 加固）。让节点连上伪造的 master、或在明文链路上改写该字段，配合一张该集群曾经有效的证书，至多让已过期的证书在节点上多生效 5 分钟；更多则须把节点机器的时钟一起回拨，属于本节“整机时间回滚”。
- G2 只在建流时检查，证书有效期间已开始的 gRPC 数据流在到期后不中断。
- 同进程的 webdav（`weed server -webdav`）解析 manifest 时会经内部通道读到 manifest 块（只含 chunk id 列表）；数据块经 `fetchChunkRange` 读取，不带令牌，仍被拦。
- **内联正文的剩余边界（v9）：** 系统豁免路径（`/etc/` 全部、`/topics/` 下的 `topic.conf` 与 `*.offset`）的正文经 gRPC 照常可读，能经 filer HTTP 写 `/etc/` 的用户可借此存取小文件；`other/java` 客户端（不在构建内）读到空正文，回写依赖 protobuf-java 保留未知字段；证书到期前已发出的订阅事件不追回。
- filer 按 `notification.toml` 向外部队列（Kafka、SQS 等）发送的元数据事件在服务端生成，内联正文不隐藏；该出口由运维配置，与内部维护同级（P3 补记）。
- 超额被拒（`NODE_REJECTED`）的 filer 只拒绝自身的 HTTP/S3 读取与 gRPC 内联正文；它仍接受写入、提供元数据与 `LookupVolume`，经它取得分块位置后直连 volume 读取的客户端（mount、`fs.cat` 等）不受影响，因为 volume 的证书有效。filer 额度约束的是能经 HTTP/S3 对外提供读取的 filer，不阻止超额 filer 充当 mount 的元数据端点（P4 补记）。
- volume server `-dir` 下的 `.dat/.idx` 文件本身不加密；证书不是数据保密方案。整机时间与状态一起回滚、替换二进制，离线证书不能完全防住。
- `filer.sync`、`filer.backup`、`filer.meta.backup`、`filer.remote.sync`、`filer.remote.gateway` 是独立进程，没有内部令牌，证书无效时被拦（第 9 节决策 3），遇到隐藏正文的事件停在原偏移，续期后续传（1.5）。S3 `CopyObject` 的源读取经 F1 被拦，并按 5.2 做状态校验。
- master 的静态 `-peers` 由启动前的本地证书检查约束（v9，3.4）；hashicorp 已有 raft 配置的成员重启只计数不检查。
- 成员账本按批次（≤ 5 秒）复制并要求 voter 多数派确认；切主最多丢最近一个未确认批次内新加入的节点记录，不承诺固定秒数（3.4）。

### 1.4 filer 内部读取通道（限定四个读取函数）

v1 的 volume 全拦会打断以下 filer 内部路径，它们都通过 `weed/util/http` 向 volume 发 GET，且现有代码不携带任何 JWT：

| 内部路径 | 调用链 | 读取函数 | 被拦后果 |
| --- | --- | --- | --- |
| manifest 差集 | `UpdateEntry → cleanupChunks → MinusChunks → ResolveChunkManifest`（[filer_grpc_server.go:208](../weed/server/filer_grpc_server.go:208)） | [fetchWholeChunk](../weed/filer/filechunk_manifest.go:105) → `retriedStreamFetchChunkData(jwt="")` | 块数 ≥ 10000 的文件连属性更新都失败 |
| 删除回收 | `doDeleteChunks → ResolveOneChunkManifest`（[filer_deletion.go:93](../weed/filer/filer_deletion.go:93)） | `fetchWholeChunk` | 数据块成孤儿 |
| 元数据日志回放 | `SubscribeMetadata → ReadPersistedLogBuffer → NewChunkStreamReaderFromFiler`（[filer_notify_read.go:326](../weed/filer/filer_notify_read.go:326)） | [ChunkStreamReader](../weed/filer/stream.go:354) → `ReadUrlAsStream` | 多 filer 追平、mount 重连、`filer.meta.backup` 中断 |
| 小块合并 | [filer_server_handlers_write_merge.go:40](../weed/server/filer_server_handlers_write_merge.go:40) | `ChunkStreamReader` | 写入失败 |
| filer.conf 启动加载与变更重载 | [filer_conf.go:38](../weed/filer/filer_conf.go:38) `ReadEntry`、[filer_on_meta_event.go:54](../weed/filer/filer_on_meta_event.go:54) `readEntry` | [StreamContent](../weed/filer/stream.go:154)（无 JWT 变体） | 按 chunks 存储的 filer.conf 读不到，路径规则、配额、远端映射失效 |
| IAM 配置（同进程 `weed server -iam` 时） | [iamapi_server.go:84](../weed/iamapi/iamapi_server.go:84) `ReadEntry` | `StreamContent` | 大的 identity/policies 文件读不到 |

与之相对，用户读取走另外两个函数，**不附加令牌**：filer HTTP 流式读走 `PrepareStreamContentWithThrottler` 的 jwt 路径（[stream.go:130](../weed/filer/stream.go:130)），webdav 与 mount 走 `reader_at.go` 的 `fetchChunkRange`（[reader_at.go:202](../weed/filer/reader_at.go:202)）。`StreamContent`/`NewFileReader` 在 filer 进程内只有上表的内部调用方；`fs.cat`、`filer.cat`、`filer.replicate`、`filer.remote.sync` 是独立进程，拿不到令牌。

方案：

1. leader 首次需要时生成 32 字节随机 `internal_read_key`，存入 `state.json`，随 `LicenseUpdate.internal_read_key` 下发给 volume 与 filer。**只在 volume 的 `SendHeartbeat` 流与 `ClientType == "filer"` 的 `KeepConnected` 流里填该字段**；shell、broker、follower master 等其它 KeepConnected 客户端收到的 `LicenseUpdate` 不含 key（v9 补；master 之间经 `SyncLicense` 同步）。
2. filer 收到后调用 `filer.SetInternalReadToken(key)`，令牌存放在 `weed/filer` 包级 `atomic.Value`，**只在 `fetchWholeChunk`、`ChunkStreamReader.fetchChunkToBuffer`、`StreamContent`（无 JWT 变体）三处读取**。传递链如下，用户入口与内部入口在同一调用点汇合，所以必须用参数区分而不是写死：
   - `PrepareStreamContent → PrepareStreamContentWithThrottler → stream.go:130 → retriedStreamFetchChunkData`。把 `PrepareStreamContentWithThrottler` 的主体抽成私有 `prepareStreamContent(masterClient, jwtFunc, extraHeader, chunks, offset, size, throttle)`；公开的 `PrepareStreamContentWithThrottler`（filer HTTP 用户路径，[filer_server_handlers_read.go:283](../weed/server/filer_server_handlers_read.go:283)）传空 header，`StreamContent` 传令牌头。`retriedStreamFetchChunkData` 增加 `extraHeader` 参数，并把它传到 `ReadUrlAsStreamAuthenticated` 的加密与非加密两个分支（都在同一函数内构造最终 GET）。
   - `fetchWholeChunk` 直接调用 `retriedStreamFetchChunkData`，传令牌头。
   - `ChunkStreamReader.fetchChunkToBuffer` 调用 `ReadUrlAsStream`，改用新的 `util_http.ReadUrlAsStreamWithHeader`。
   gzip、`util.Retry` 重试、断点偏移都在原函数内，不受影响。
3. volume 的 V1 守卫：证书有效直接放行（快路径不看请求头）；证书无效时才检查：本地 key 已初始化且长度 32 字节、请求头 base64 解码后长度 32 字节、`subtle.ConstantTimeCompare` 相等才放行。`NOT_READY` 时任何请求头都不放行。
4. `weed server -filer -webdav` 同进程：webdav 读取走 `fetchChunkRange`，无令牌，被 V1 拦；S3 经 filer HTTP 走 F1。S18 专门验收。
5. 令牌轮换：第一版不做接口（决策 10）。

### 1.5 `Entry.Content` 内联正文

v3 的“在 gRPC `Lookup/List` 剥离 `Content`”已撤销，原因：

- S3 的 IAM 配置 `/etc/iam/identity.json` 经 `ReadInsideFiler` 用 gRPC `Lookup` 读 `Content`（[read_write.go:39](../weed/filer/read_write.go:39)）。剥离后 S3 重启读到空配置，现有加载失败只告警继续启动，`isAuthEnabled=false` 时鉴权包装器直接放行（[auth_credentials.go:319](../weed/s3api/auth_credentials.go:319)），仍允许的写删会失去 IAM 校验。
- S3 标签修改是 `Lookup → 改 Extended → 整个 Entry 提交 UpdateEntry`（[filer_util_tags.go:40](../weed/s3api/filer_util_tags.go:40)），剥离后的副本回写会永久清空内联正文。
- mount 对 `FileSize > 0` 且无 chunks 的条目在 `Content` 为空时按稀疏文件补零返回（[filechunk_group.go:59](../weed/filer/filechunk_group.go:59)），得到的是错误数据而不是 `EIO`。

v4 处理：只在只读 HTTP 接口（M1）隐藏，gRPC 面列为边界。部署建议 `-saveToFilerLimit=0`（启动时对 >0 打 warn），但内联正文还有 gRPC `CreateEntry/UpdateEntry` 原样接受（[entry.go:111](../weed/filer/entry.go:111)）、S3 尾斜杠 ≤ 1024 字节对象（[s3api_object_handlers_put.go:82](../weed/s3api/s3api_object_handlers_put.go:82)）、`/etc` 下 HTTP 写入强制内联和存量文件等来源，不把该参数写成保证。

**v9 处理（决策 9 改选封堵）。** 上面三条反对理由分别由以下机制解决，完整规则、挂点与客户端清单见 [P0 契约 5.3](license-p0-contract-20261013.md)：

- **配置不受影响：** `/etc/` 全部条目与 `/topics/` 下的 `topic.conf`、`*.offset` 豁免，IAM、熔断、filer.conf、远端映射、MQ 配置照常经 gRPC 读取。
- **回写不丢正文：** `filer.proto` 的 `Entry` 加 `content_hidden = 13`。隐藏时出站副本 `Content` 置空并置标志；`CreateEntry/UpdateEntry` 收到带标志、正文为空且 chunks 与 `FileSize` 未变的条目时保留已有正文（S3 标签、`touch`、mount chmod/xattr/硬链接），无法确定字节的返回 `license: CONTENT_HIDDEN`，存储与事件日志里标志恒为 false。
- **不返回伪造数据：** mount 对隐藏条目返回 `EIO` 并按 1.6 的节流重取；`ReadEntry`/`ReadInsideFiler`、`fs.cat`、`filer.cat` 报错而不是补零；`filer.sync`/`backup`/`meta.backup`/`remote.sync`/`remote.gateway` 经订阅选项 `FailOnHiddenContent` 停在事件处，`fs.meta.save`/`notify` 报错。
- **filer 之间不隐藏：** 元数据聚合订阅（含自身）与新 filer 引导携带内部令牌；Replay 与引导遇到隐藏条目报错重试，不入库。引导的重试最多 5 分钟，master 拒绝本 filer 时立即失败；`filer.store.id` 在引导成功后才写入，引导切点 `filer.bootstrap.since` 先持久化、成功后删除，重启后从同一切点继续；对端日志已不覆盖切点时明确失败（2026-10-25 修订，8.22 方案 9）。
- **`KvGet` 旁路：** 拒读态只放行 `Meta`、`sync.`、`backup.`、`remote.sync.` 前缀的同步偏移键（`backup.` 为 2026-10-25 补入，8.22 方案 2）。

### 1.6 独立 mount 对 manifest 文件的错误传播（v5 新增）

独立 `weed mount` 首次打开含 manifest 的文件时，要经 volume GET 解析 manifest，它没有内部令牌，证书无效时被拒。现有代码在 [filehandle.go:78](../weed/mount/filehandle.go:78) 对解析失败只打 `Warningf`，分块表为空；随后 [filechunk_group.go:59](../weed/filer/filechunk_group.go:59) 把缺失区间当作稀疏文件补零并返回成功，应用会拿到伪造的全零正文而不是错误。

续期后对隐藏正文条目的重取（P3 实现，`refetchHiddenEntry`）**保留句柄上尚未 flush 的本地修改（2026-10-14 修订，8.6；2026-10-15 补充截断与 id 映射，8.8；同日再修订，8.9–8.14）**：读失败状态单独记录“正文隐藏”，重试方式按它决定，不按最近一次错误：查询 filer 失败只替换错误，下次重试仍是重取条目，直到取回正文或写入丢弃正文；重取在锁外等待 filer 应答，期间截断到 0 或句柄装入新条目都会替换句柄的读失败状态；应答回来时若句柄已不是发起时的状态，结果整个丢弃，既不装入正文，也不恢复读错误；取回的正文按句柄上的文件大小截断（未提交的截断），并从句柄在隐藏期间截到过的最小非 0 长度起补零（先截短再扩长时，被截掉的字节不能回来）；隐藏期间截短后又扩长、使截到的最小长度小于当前大小时，flush 返回 `EIO`（`CONTENT_HIDDEN`）而不发送，因为大小回到原值时 filer 会按纯元数据更新补回全部旧字节（其余大小 filer 本就拒绝）；`copy_file_range` 与写入一样丢弃内联正文并清除隐藏标志；整体替换时先把 filer 的 uid/gid 映射为本地 id（与 meta cache 读出时相同），meta cache 仍存 filer 的 id；证书无效期间在打开的句柄上做的 `chmod`、`chown`、`utimes`、`setfattr` 只记在句柄里，等 flush 才写回 filer；重取时若句柄有未提交修改，只取 filer 的正文、清除隐藏标志，其余字段保持本地，meta cache 等 flush 后更新；没有未提交修改时才整体替换。“正文”是内联字节、分块、远端对象描述、大小与 MD5 一起取，分块组按 filer 的分块重建（8.12、8.13）：其间别的客户端可能已改写文件，包括改为分块存储或远端对象。句柄在隐藏期间改过大小（截断或扩长，与隐藏时的大小比较）时，只能把内联字节按本地大小裁剪；filer 已改为分块存储或远端对象则无法合并，保持隐藏与读失败，之后 flush 由 filer 以 `CONTENT_HIDDEN` 拒绝，不会丢掉 filer 上的分块。取回的是远端对象时，首次读取、写入或改变大小前经 `CacheRemoteObjectToLocalCluster` 缓存，缓存结果装入句柄并换上新的分块组，句柄上只可能有的属性修改保留；截断到 0 去掉远端描述（8.14）。写入与 `copy_file_range` 的时间戳晚于条目全部分块与句柄上一次写入，覆盖顺序不依赖 mount 与 filer 的时钟一致；属性修改与读写、filer 事件同持句柄锁（8.15）。分块的最大时间戳在分块进入条目时维护，含重试解析 manifest 成功时，写入不遍历分块（8.16、8.17）。上传完成的分块在 `entryLock` 读锁内加入条目与分块组，`Lseek` 也在该读锁内取条目与分块组，与替换分块组互斥（8.18、8.19）。**截断掉导致失败的分块即清除失败状态（2026-10-25 修订，8.22 方案 1）**：截断到 0 时无条件清除读失败（隐藏类与 manifest 类都清，旧内容一点不剩）；部分截断时按剩余分块重建分块组的结果更新 manifest 类失败——解析成功或剩余分块无 manifest 则清除，失败则记新错误与时间，隐藏类失败不受影响；截断时才解析成功的 manifest 同样抬高 `chunkTsNs`，之后的写入晚于其内部分块。

处理：`FileHandle` 记录 `resolveManifestErr` 与 `lastResolveAttempt`；读取入口（`filehandle_read.go`）在该错误非空时：若距上次尝试 ≥ 5 秒，用同一 entry 重新调用 `NewChunkGroup`，成功则清错并正常读，失败则更新时间戳并返回 `EIO`；5 秒内直接 `EIO`，不发网络请求。只在失败状态重试，正常读取不新增任何调用。这样续期或网络恢复后，原句柄与按 inode 复用的句柄（[filehandle_map.go:44](../weed/mount/filehandle_map.go:44) 只在 entry 指针变化时重解析）都会在目标 volume 新证书生效后、下一次满足重试间隔的读取时触发重解析并恢复，不需要关闭全部旧句柄；端到端时间见 S1b。写入不受该错误影响（写路径不依赖已解析的分块表）。v9 起同一入口还处理 `ContentHidden` 条目（1.5）：返回 `EIO`，失败态下 ≥ 5 秒用 `LookupDirectoryEntry` 重取，未隐藏则替换句柄条目并写回 meta cache；两类重试共用 single-flight 与节流。mount 改动合计约 40 行。同样的补零逻辑在 filer 进程内不触发：filer 的 manifest 解析带内部令牌（1.4）。S1b 验收。

---

## 2. 证书格式（复用 MassDB SQL 协议 v1）

| 项目 | 口径 |
| --- | --- |
| 载体 | compact JWS，`alg=Ed25519`、`typ=massdb-license+jws`、受信 `kid`；原文 ≤ 64 KiB，严格 base64url/UTF-8/JSON、拒绝重复键，按原始签名字节验签 |
| 验签实现 | Go 标准库 `crypto/ed25519` + `encoding/json`，不引入新依赖 |
| 关键声明 | `schema_version=1`、`policy_version=1`、`license_id`、`issuer`、`customer_id`、`edition`、**`product="MassDB OSS"`**、`deployment_id`、`issued_at/not_before/expires_at`（UTC 整数秒）、`sequence`、`features`、`limits` |
| `features` | `DATA_READ`。缺少则读被拦；写、元数据不受影响 |
| `limits` | `max_master_nodes`、`max_volume_servers`、`max_filers`；整数 ≥ 1。S3 网关、webdav、mount 不向 master 注册，不计数 |
| 时间 | `not_before <= now < expires_at`，到期等号即失效，无宽限；节点判断粒度见 4.3 |
| 续期 | 状态持有 `newest` 与 `prev` 两张证书（3.2）；`sequence` 严格递增；`deployment_id` 必须等于本集群 id；导入失败保旧 |
| 签发 | 生产签发工具放在独立项目，不放入 massdb-sql；沿用协议 v1。massdb-sql 的签发工具只用于生成互通测试向量。见 7.3 |
| 信任根 | 生产公钥清单编译进二进制（`weed/license/trust.go`）；测试根只经 `-ldflags -X` 或 build tag 注入 |

申请 DTO（由 `GET /license/deployment` 导出）：

```json
{"schema_version":1,"product":"MassDB OSS","deployment_id":"<uuid>","registered_master_nodes":3,"registered_volume_servers":7,"registered_filers":2}
```

---

## 3. 集群绑定、授权状态与节点额度

### 3.1 集群身份

- leader 首次需要时生成 `cluster_id`（UUID v4），写入 `<mdir>/license/state.json`，与证书、`internal_read_key` 一起按 3.3 同步。不绑定 IP/MAC。
- 冲突规则：已导入有效证书内的 `deployment_id` 为准；follower 本地 id 与 leader 不一致且本地无证书时采用 leader 的 id；两边都有证书且 id 不同时拒绝同步并告警。

### 3.2 授权状态：两张证书，生效由时间推导

v3 的 `(active, pending)` 加“到点激活”转换被审核证明无法定义一致的新旧顺序（已确认的更高 pending 会被本地激活的旧 pending 盖掉）。v4 取消激活转换：

- `LicenseState{cluster_id, newest, prev, internal_read_key}`。`newest` 是接受过的最高 `sequence` 证书；`prev` 只在 `newest.not_before` 尚未到来时保留，是导入 `newest` 那一刻正在生效的证书，否则为空。
- **生效证书是时间的纯函数：** `effective(now) = newest` 若 `now >= newest.not_before`，否则 `prev`。master 与节点各自按本地原子时钟推导，不需要任何状态转换、不需要到点下发、不需要“清除 pending”消息。
- **新旧顺序只看 `newest.sequence`：** 来者更高则整体替换（`newest` 与 `prev` 一起）；相等必须字节相同，否则拒绝（同序号不同内容）；更低拒绝。审核中的反例（A/C 多数派持 `(newest=12, prev=10)`，B 本地把 11 当作生效证书）在 v4 里 B 的状态是 `(newest=11, prev=10)`，序号 11 < 12，被 12 整体替换，B 的生效证书随即按时间推导为 10 或 12，与 A/C 一致。
- **同一 `newest` 下 `prev` 的收敛（2026-10-14 修订，8.6）：** 落后的 master 以 `-license.file` 加载同一张未来证书时，`prev` 取的是它自己当时的生效证书，可能与其它 master 不同；只比较 `newest` 会让各方都认为已同步，而持旧 `prev` 的一方在过渡期误判 `EXPIRED`。规则：`newest` 字节相同时，`prev` 取序号更高的一张（有优于无，同序号按字节序），master 之间同步、对账合并、master 下发到节点都按此替换；确认同步时 `prev` 也须相同，持更好 `prev` 的 follower 不计入确认，leader 采纳后重推，最终收敛到同一张。
- 有效额度：`newest` 未生效期间取 `min(prev.limit, newest.limit)`。
- 导入校验：`new.sequence > newest.sequence`；`new.not_before <= now` 则状态为 `(new, nil)`，否则 `(new, effective(now))`。
- `watermark/repair_seq` 独立于证书部分，同步规则见 4.5。

### 3.3 多 master 同步

不改 raft 状态机。采用“只在 leader 导入 + voter 多数派同步推送 + 选主对账并写回多数派 + 按流对账”：

1. **只有 leader 接受导入。** 导入 handler 自检 `ms.Topo.IsLeader()`，否则 503；不依赖 `proxyToLeader`（找不到 leader 时它会直接调本地 handler，[master_server.go:222](../weed/server/master_server.go:222)）。
2. **voter 多数派。** voter 集合取自当前 raft 配置：hashicorp 用 `GetConfiguration().Configuration().Servers` 中 `Suffrage == Voter` 的成员（现有 [RaftServer.Peers()](../weed/server/raft_server.go:173) 不区分，manager 内另取；`RaftAddServer` 支持 nonvoter，[master_grpc_server_raft.go:51](../weed/server/master_grpc_server_raft.go:51)）；goraft 全部成员是 voter。多数派 = `floor(voters/2)+1`，leader 自身计入。nonvoter 也推送，不计入多数派。
3. **`SyncLicense` 携带 `leader_address` 与 `leader_term`。** follower 只接受 `leader_address == Topo.Leader()` **且 `leader_term == 本地 raft 当前任期`**（hashicorp `CurrentTerm()`，goraft `Term()`）的请求；不用“最高已收到的 Sync 任期”，避免同一地址 term1 当选、落选、term3 再当选时 term1 的迟到请求被接受。
4. leader 先写本地 `state.json`，再同步推送；voter 成功数（含自身）达到多数派返回 200；否则 503 `PARTIAL_COMMIT` 列出失败 peer，leader 本地保留已落盘状态。**相同字节的证书重试导入视为幂等**，只重新推送；同 `sequence` 不同内容 409。**只有状态已写入磁盘的成员才计入确认（2026-10-14 修订，8.6；2026-10-15 再修订，8.8）：** 写 `state.json` 失败时内存已是新状态，重试相同证书会被判为“未变化”而跳过写盘；第一次请求还在写盘时到达的重复请求同样会被判为“未变化”。因此每个 master 记录磁盘上状态对应的版本（授权修订号、账本版本、时钟修复序号），导入、`SyncLicense`、`SyncLedger` 应答与 leader 自身计票前都要求磁盘版本追上内存版本；没追上就等正在进行的写盘结束后再写一次，成功才确认，否则不确认（leader 自身计入 `peers_failed`）。“写盘成功”包括 rename 后同步所在目录（2026-10-15 修订，8.10）：打开或同步目录失败按写盘失败处理；Windows 不同步目录，文件系统不支持目录同步（`EINVAL`、`ENOTSUP`）时也跳过。
5. **选主对账并写回：** 成为 leader 后向所有 peer 调 `GetLicense`，需收到 voter 多数派（含自身）的响应；按 3.2 规则选出最高状态；**然后把选中的状态同步推送到 voter 多数派并得到确认，才退出 `RECONCILING`**。对账中 `GET /license` 为 `RECONCILING`，导入与成员变更返回 503；**不向节点下发任何拒读原因**，节点保留现有授权快照并按本地时钟评估，授权层的读写行为不变（可用性仍受 master 不可用期间既有行为影响，例如 filer 拿不到新的 volume 位置）；未达多数派每 5 秒重试，不在请求路径。审核中的反例（A 的 `PARTIAL_COMMIT` 少数派状态 11 被新 leader B 选中并下发，A/B 随后不可用，C/D/E 只能恢复到 10）在 v4 里 B 必须先把 11 写到 C/D/E 中至少两台才退出对账，之后任何多数派都能看到 11。
6. follower 启动或重连 leader 时经 KeepConnected 首帧和按流对账拿到当前状态（4.2），按 3.2 替换。**P2 实现：** 改为 leader 每 5 秒向尚未确认当前授权修订的成员重推 `SyncLicense`（带任期校验），follower master 不从 KeepConnected 应用授权状态；KeepConnected 的 `LicenseUpdate` 只给 volume/filer（P3）。
7. **加入 voter 的两阶段流程（hashicorp）。** `SyncLicense` 要求接收方本地 raft 已认识当前 leader 且任期相同（第 3 条），全新节点在进入配置前拿不到这些状态，所以不能“先同步再加入”。顺序改为：(a) 三个加入入口先 `CheckMasterJoin()` 预占额度，然后以 **nonvoter** 加入（`AddNonvoter`）；(b) 等它的 raft 追平（`LastIndex` 接近 leader）并通过 KeepConnected 建流；(c) leader 在 manager 锁内向它 `SyncLicense` + `SyncLedger` 并等待确认，锁内同时排斥导入，因此“预同步完成到提升之间”不会漏掉新证书；(d) 确认后 `AddVoter` 提升。任一步失败保持 nonvoter 并告警，不计入 voter 多数派；**同一 leader 任期内在该 master 重连（`OnPeerUpdate` → `RetryPromotion`）或每 5 秒（`tick`）重试提升，只针对 manager 记下的"待提升"集合 `pendingVoters`，以 `-voter=false` 加入的 nonvoter 永不进入该集合；提升成功、`RemoveMaster` 或失去 leader 身份（新任期对账完成）时清除（2026-10-25 修订，8.22 方案 8）。** `OnPeerUpdate` 与 `updatePeers` 的 `AddVoter` 改为走同一流程。
8. **移除 voter 前先重提交。** `RaftRemoveServer`（[master_grpc_server_raft.go:87](../weed/server/master_grpc_server_raft.go:87)）、`updatePeers` 的 `RemoveServer`（[raft_hashicorp.go:107](../weed/server/raft_hashicorp.go:107)）、`OnPeerUpdate` 的远程移除（[master_server.go:383](../weed/server/master_server.go:383)）都先调 `License.BeforeRemoveVoter(target)`：计算移除后的 voter 集合，把当前已确认的 `LicenseState` 与账本重新推送到该集合并要求其多数派确认，成功才执行移除，否则拒绝并告警。审核反例（11 在 A/B 提交，C 仍为 10，先移 B 再移 A）在 v6 里移 B 时要先让 {A, C} 多数派持有 11，C 因此追平。
   **临界区：** “重提交确认 → 实际移除完成”整个区间持有 manager 互斥锁，与导入、加入两阶段、其他移除串行（现有 `RaftRemoveServer` 只拿 `RaftServerAccessLock` 读锁，[master_grpc_server_raft.go:64](../weed/server/master_grpc_server_raft.go:64)，不能提供这项互斥）。执行移除前再次核对“授权状态版本与 raft 配置内容（排序后的 voter 与 nonvoter 集合，`Membership.sameServers`）都等于准备时记录的值”，不等则放弃本次移除并返回 409 让调用方重试（2026-10-25 修订，8.22 方案 3：hashicorp v1.7.2 的 `GetConfiguration().Index()` 恒为 0，原来的配置索引比较从未生效）。审核交错（两个移除分别完成确认后、执行前提交了 11，再依次移除 B、A 只剩持 10 的 C）因此不会发生：第二个移除的准备阶段要等第一个移除执行完，而导入被锁排斥到移除之后。锁内每个 peer 的 RPC 截止 3 秒、整个流程截止 10 秒，超时视为失败。leader 把自己计入剩余 voter 的多数派前，同样要求 `state.json` 已持有当前状态（第 4 条；2026-10-15 修订，8.9：此前移除流程直接把 leader 计为已确认，leader 写盘失败时可以依次移除真正持有状态的 voter）。
9. **goraft（静态 `-peers`）模式的边界。** 成员只能靠重启改 `-peers`（[raft_server.go:188](../weed/server/raft_server.go:188) 的 `DoJoinCommand` 在启动时执行），没有可挂钩的加入/移除入口。该模式下：新 master 启动后经 KeepConnected 首帧拿到状态；manager 对每个 voter 记录已确认的状态版本 `voters_acked`，`GET /license` 与 `weed shell license.show` 展示；运维缩容前必须确认保留的 voter 都已确认当前状态，否则可能回退。这是运维约束，不是代码保证。
10. **绑定配置内容（原“绑定配置索引”，2026-10-25 修订）。** 导入提交与对账都记录开始时的 raft 配置（voter 与 nonvoter 集合；hashicorp 的 `GetConfiguration().Index()` 恒为 0，不用索引）；完成前配置变化则按新配置重新计算多数派并补推送，仍不足则 `PARTIAL_COMMIT`/继续 `RECONCILING`。
11. **`RECONCILING` 期间一律延后注册**（3.4）：授权状态与账本对账、接管转换完成前，`SendHeartbeat` 与 `KeepConnected` 对所有地址（含账本内旧地址）返回可重试错误，volume 按既有 `heartbeat()` 循环、filer 按既有重连循环在数秒后重试；对账完成后再开放。节点保留现有授权快照，授权层读写不变（证书在本地缓存并按本地时钟评估）；注册被延后的节点收不到首帧，也不需要收到任何拒读原因。这样恢复流程不会覆盖刚重连写入的 `online=true`，也避免落后的新 leader 按旧高额度接纳新地址。

### 3.4 计数口径与超额行为

SeaweedFS 没有 `ALTER SYSTEM ADD/DROP`，volume server 通过心跳自动注册、断开即注销；filer 通过 KeepConnected 注册。v3 的“只记录断连节点”在旧 leader 突然宕机时无法保护原在线节点（它们没有断连记录，新 leader 重建拓扑期间可能被新节点占额），v4 改为**成员账本**：

- 账本 `map[addr]{type, last_seen, online}` 记录所有已注册节点（在线与宽限中）。注册时写入；断连时 `online=false, last_seen=now`；宽限 10 分钟后删除。
- 额度已用数 = 账本条目数（在线 + 宽限）。已在账本内的地址重新注册不占新额度；账本外的地址才检查额度。
- **版本：** 快照版本是 `(term, revision)`，`term` 为生成该快照的 leader 当时的 raft 任期，`revision` 在任期内单调递增；比较按字典序。审核反例（旧 leader A 留下未确认的 v100，新 leader B 确认了 v91）在 v6 里是 `(t1,100)` 与 `(t2,91)`，`t2 > t1`，A 恢复后对账不会用旧任期的高修订覆盖。同版本不同内容：拒绝并告警（实现错误）；迟到重发的较低版本：忽略。**raft 任期回退（2026-10-14 修订，8.5）：** goraft 每次启动都删除 raft 日志，全部 master 重启后任期从 0 重新计数，而 `state.json` 保留旧的账本版本；新 leader 若仍以 raft 任期作 `term`，写回的版本低于 follower 已有版本而被拒，对账永远达不到多数派，注册一直延后。因此 leader 每个任期在对账时选定写入用的 `term`（epoch）：raft 任期大于对账所见最高 `term` 时取 raft 任期（正常情况，行为不变），否则取所见最高 `term + 1`；同一任期内的对账重试与之后的批次沿用该 epoch，只有看到更高的版本才重选。hashicorp 的任期持久化，不受影响（`-raftBootstrap` 除外）。
- **复制：** leader 在账本有变更时按批次（≤ 5 秒合并一次）把**全量快照**写入本地 `state.json`，并经 gRPC `SyncLedger` 推送到所有 master，要求 voter 多数派确认。**锁规则：** 后台复制 goroutine 在 manager 锁内只做“拷贝当前快照并分配 `(term, revision)`”，然后**释放锁再发 RPC**；收到确认后重新加锁，核对 leader 任期未变且该 `revision` 仍是最新已发版本才标记已确认，否则丢弃结果。注册路径与复制共用的锁因此只有微秒级持有时间，慢 voter 或不可达 voter 不会让集中重连的节点排队。只有导入、加入两阶段、移除前重提交、选主对账这四类低频操作在锁内同步等待 RPC（带 3.3 的截止时间）。**在途批次至多一个**：上一批未确认前，新变更只合并进内存账本，不生成新版本；确认或超时（5 秒）后再生成下一版本（全量，自然包含合并的变更）并发送。超时未确认的旧版本不再重发，它的内容已包含在新版本里。不经 KeepConnected 流复制（KeepConnected 可能落后数分钟而 raft 仍正常）。
- **选主时账本对账：** 新 leader 与授权状态对账同步进行，向所有 peer 调 `GetLedger`，收到 voter 多数派响应后按 `(term, revision)` 取最高（含自己），再以新任期的 epoch 生成 `(epoch, 1)` 写回 voter 多数派，才退出 `RECONCILING`。切主最多丢最近一个未经多数派确认的批次，不承诺固定秒数。
- **新 leader 起步的宽限规则：** 对账得到的快照中，`online=true` 的条目（旧 leader 认为在线、接管后状态未知）置为 `online=false, last_seen=接管时刻`，获得一个完整宽限期；`online=false` 的条目**保留原 `last_seen`**，不延长。该转换在写回多数派之前完成，之后才退出 `RECONCILING` 并开放注册（3.3 第 11 条），所以不存在“恢复覆盖刚重连节点”的交错；注册逻辑只在首次心跳执行（[master_grpc_server.go:132](../weed/server/master_grpc_server.go:132)），不能依赖后续心跳纠正，因此顺序必须是先恢复再开放。
- **原子预占：** 账本检查与写入在 manager 互斥锁内完成；volume 在 `GetOrCreateDataNode` 前、filer 在 `addClient` 前、master 在三个加入入口前，都在同一把锁内“检查并预占”，并发加入不会同时占到最后一个名额。master 预占在 raft 配置变更失败时回滚。

| 节点 | 注册入口 | 超额时 |
| --- | --- | --- |
| volume server | `SendHeartbeat`：先 `rack.FindDataNode(ip, port)` 与账本查询，命中放行；新地址才检查 | 回 `HeartbeatResponse{license_reject_reason}` 并返回 gRPC 错误；volume 打 `glog.Errorf`，按既有 `heartbeat()` 循环重试（不 `os.Exit`），本地快照置 `NODE_REJECTED` 拒读，直到收到不带拒绝原因的 `LicenseUpdate` |
| filer | `KeepConnected` 在 `addClient` 之前（[master_grpc_server.go:276](../weed/server/master_grpc_server.go:276)） | 返回 `codes.PermissionDenied`，消息以 `license:` 开头；`wdclient.MasterClient` 识别后调用 `OnLicenseUpdate` 置 `NODE_REJECTED`，F1/F2/M1 拒绝。**不能依赖“没有 volume 位置”**：`LookupFileIdWithFallback` 先查本地缓存（[masterclient.go:59](../weed/wdclient/masterclient.go:59)），`resetVidMap` 保留旧缓存（[masterclient.go:335](../weed/wdclient/masterclient.go:335)） |
| master | hashicorp 的 `RaftAddServer`、`OnPeerUpdate` 的 `AddVoter`（[master_server.go:372](../weed/server/master_server.go:372)）、`updatePeers` 的 `AddVoter`（[raft_hashicorp.go:101](../weed/server/raft_hashicorp.go:101)）都先 `License.CheckMasterJoin()` | 拒绝加入并告警；导入时按当前 raft 配置成员数校验 |

- 已注册节点不会因到期被踢；到期沿用最后一次有效证书的额度约束新注册。
- **从未导入证书：不能加入节点**（决策 2，v9 改选，对齐 MassDB SQL“无可信额度仅允许新集群首 FE 引导，增加 BE 需要证书”）。volume 与 filer 额度为 0；master 动态加入额度为 0；master 在 raft 引导/加入前做本地证书检查，无证书时只允许 `-peers` ≤ 1 的单 master 新集群，有证书时 `-peers` 数不得超过 `max_master_nodes`，否则以 `license: NODE_LIMIT_EXCEEDED` 退出。hashicorp 已有 raft 配置的成员重启不受该检查（只计数、不踢）；goraft 默认每次启动清空 raft 状态（[raft_server.go:121](../weed/server/raft_server.go:121)），无法区分新建与重启，多 master 每次启动都要求本地已有证书。导入时按 3.5 校验。细则见 P0 契约 5.1。
- **先取证后部署/升级（v9 新增）。** 无证书时升级后的 master 不接受任何 volume 与 filer 注册，写入完全中断，所以存量集群升级与多 master 首次部署都必须先取证：(1) 用新版二进制离线执行 `weed license.init -mdir=<mdir> [-clusterId=<uuid>]` 生成 `state.json`（只写 `cluster_id`，已有不同 id 时拒绝覆盖）并打印申请 DTO；多 master 时先在一台生成，再用 `-clusterId` 在其余各台写入同一 id；(2) 按 DTO 签发证书；(3) 以 `weed master -license.file=<证书>` 启动：启动检查用它判定 `-peers`，成为 leader 的 master 在退出 `RECONCILING`、开放注册之前按导入流程提交。单 master 也可先启动、导出申请、导入后再启动 volume/filer。`-clusterId` 也用于状态目录全部丢失后按证书里的 `deployment_id` 恢复；它不削弱集群绑定，因为 `state.json` 本就可手改，绑定只防误用、不防有意复制（与 1.3 离线证书边界同级）。`weed server` 单进程在导入前内置 volume/filer 也被拒，评估环境需要试用证书。
- `-mdir` 默认是 `os.TempDir()`（[master.go:70](../weed/command/master.go:70)），`state.json` 可能随重启丢失，集群回到无证书状态（不能加入节点）并换新 `cluster_id`。master 启动时若 `<mdir>` 位于临时目录下打 warn，运维手册要求持久化并备份 `<mdir>/license/`。
- 冷启动即被拒的 filer 会卡在构造阶段等待 master（[filer_server.go:159](../weed/server/filer_server.go:159)），HTTP 端口未监听，表现是无法连接；运行中被拒的 filer 才返回 403。S12 分开验收。

### 3.5 续期与降额

- 导入校验见 3.2；新额度必须 ≥ 当时账本条目数（三类分别校验），否则 `LICENSE_NODE_LIMIT_TOO_SMALL`，失败保旧；`DELETE /license/nodes/{addr}` 可立即删除宽限中的条目（不踢在线节点）。
- `validate` 做同样校验但不落盘。

---

## 4. 状态下发与节点行为

### 4.1 协议改动（`weed/pb/master.proto`，只增不改）

```proto
message LicenseUpdate {
  string newest_certificate = 1;
  string prev_certificate = 2;      // 可空；与 newest 整体应用
  int64  master_now_unix_ms = 3;   // P1 改为毫秒，到期判定误差不再多出 1 秒
  string cluster_id = 4;
  string master_deny_reason = 5;    // CLOCK_SUSPECT / NODE_REJECTED（RECONCILING 不下发，见 3.3）
  bytes  internal_read_key = 6;
  uint64 state_version = 7;         // 仅同流对账与展示；跨 master 不单调
}
message LicenseState { string cluster_id = 1; string newest_certificate = 2; string prev_certificate = 3;
                       bytes internal_read_key = 4; int64 watermark = 5; int64 repair_seq = 6; }   // master 之间复制
message NodeLedgerEntry { string address = 1; string node_type = 2; int64 last_seen_unix = 3; bool online = 4; }
message NodeLedgerSnapshot { repeated NodeLedgerEntry entries = 1; uint64 term = 2; uint64 revision = 3; }
HeartbeatResponse     { ... string license_reject_reason = 8; LicenseUpdate license = 9; }
KeepConnectedResponse { ... LicenseUpdate license = 3; }
rpc GetLicense    (GetLicenseRequest{include_state})          returns (GetLicenseResponse{status_json, state});
rpc ImportLicense (ImportLicenseRequest{certificate, validate_only}) returns (ImportLicenseResponse{result_json});
rpc SyncLicense   (SyncLicenseRequest{leader_address, leader_term, config_index, state}) returns (SyncLicenseResponse{state});
rpc GetLedger     (GetLedgerRequest)                          returns (GetLedgerResponse{ledger});   // 选主对账
rpc SyncLedger    (SyncLedgerRequest{leader_address, leader_term, ledger}) returns (SyncLedgerResponse); // voter 多数派确认
```

P2 实现说明：`status_json`、`result_json` 与 HTTP 的 `GET /license`、`POST /license/import|validate` 是同一份 JSON，供 shell 打印；`include_state` 只给对账的 master 用（带 `internal_read_key`）。`SyncLicenseResponse.state` 是接收方应用后的状态，接收方更新时 leader 据此追平（8.1）。

**带 `license` 的 `HeartbeatResponse` 必须同时填入当前 `VolumeSizeLimit` 与 `Preallocate`**（[volume_grpc_client_to_master.go:133](../weed/server/volume_grpc_client_to_master.go:133) 无条件读取）。

旧版 volume/filer 忽略未知字段，但没有守卫，等于未受限。**升级必须 master、volume、filer 同批完成**，先 master 后其他。

### 4.2 下发：按流对账

| 流 | 机制 | 成本 |
| --- | --- | --- |
| `SendHeartbeat`（volume） | 注册成功后随 `VolumeSizeLimit` 那条响应发首帧；之后每次收到心跳比较本流 `lastSentVersion != manager.Version()`，不同则 `stream.Send` | 每心跳一次整数比较 |
| `KeepConnected`（filer、follower master、broker、shell） | `addClient` 后在流上发首帧（`internal_read_key` 只对 filer 填，1.4）；既有 5 秒 ticker 分支（[master_grpc_server.go:325](../weed/server/master_grpc_server.go:325)）做版本比较并直接 `stream.Send`。**不走 `broadcastToClients`**（队列满丢消息，[master_grpc_server.go:346](../weed/server/master_grpc_server.go:346)） | 每客户端每 5 秒一次整数比较 |

节点侧应用规则（幂等）：
- `master_now_unix_ms`、`master_deny_reason`、`internal_read_key`：每次收到都应用。
- `newest/prev`：验签通过后按 3.2 比较整体替换；相同序号不重复落盘。
- 客户端首帧与后续循环是两段代码（[masterclient.go:226](../weed/wdclient/masterclient.go:226)、`:248`），两处都调用 `OnLicenseUpdate`；KeepConnected 返回 `PermissionDenied` 且消息以 `license:` 开头时也调用，置 `NODE_REJECTED`。
- [master_grpc_server.go:319](../weed/server/master_grpc_server.go:319) 发送失败日志改为只打印消息类型与版本号。

### 4.3 节点时间口径

每个 `license.Node` 实例一个 500 ms ticker 维护原子时钟：有 master 锚点时取 `master_now_unix_ms + 自锚定以来的单调流逝`（按毫秒累计后取整秒），但不早于本地墙钟减 5 分钟；无锚点退回本地墙钟。master 时间落后本地墙钟不超过 5 分钟时完全跟随 master；超过时改用本地墙钟减 5 分钟，并在进入与退出时各打一条日志（2026-10-14 修订，此前为“不与本地墙钟取较大值”，伪造的 master 时间可让过期证书一直有效，见 1.3）。代价：本地时钟比 master 快 5 分钟以上的节点，到期判定比 master 早“快出部分减 5 分钟”，未来证书也相应提前生效。同一 ticker 按 3.2 推导生效证书并在变化时替换快照。

实现要点：manager 锁内同步等待的 RPC 只有四类低频操作（导入多数派推送、加入两阶段同步、移除前重提交、选主对账），都带截止时间（单 peer 3 秒、整体 10 秒）；普通账本复制按 3.4 在锁外发 RPC；mount 的限频重解析用 single-flight 保证同一句柄同时只有一次重建，重建结果只在当前分块表仍为失败态时写入，不覆盖并发写入已追加的块。

到期生效时刻 = 到期秒 + ticker 周期（≤ 0.5 s）+ 调度延迟；CPU 饱和时延迟可能更大，是接受的边界。

### 4.4 节点缓存与失败关闭

- 缓存为 `{"newest","prev","cluster_id"}` JSON，原子写。不缓存 `master_deny_reason`、时间锚点、`internal_read_key`。
- volume：第一个 `-dir/license-cache.json`；filer：`-defaultStoreDir/license-cache.json`。不用 filer store KV。
- 缓存用途是缩短“已连上 master 但未收到首帧”与选主窗口内的 `NOT_READY`。**不承诺离线启动**（[volume_server.go:112](../weed/server/volume_server.go:112)、[filer_server.go:159](../weed/server/filer_server.go:159) 启动都先同步等 master）。
- 状态：无证书无缓存 `NOT_READY`；被拒准入 `NODE_REJECTED`；key 未初始化时内部令牌不生效。
- 收到的证书验签失败：忽略并保留旧快照，告警。

### 4.5 时钟回拨（master）

- 每个 master 用自己的时钟每 60 秒把 `max(now, watermark)` 写入自己的 `state.json`。
- 发现 `now < watermark - 5s`：`CLOCK_SUSPECT`，下发 `master_deny_reason`，节点拒读；墙钟重新超过水位后自动恢复。
- 选主对账只采纳更高 `repair_seq` 的 `(watermark, repair_seq)`；同序号时 leader 保留自己的水位，不取各 peer 的最大值，避免一台时钟超前的 master 让新 leader 进入 `CLOCK_SUSPECT`（P2）。
- 采纳其他 master 的较新状态（`adoptAhead`）时同时按三分支规则合并其时钟修复；重复导入同一张证书时若本地尚未应用它的修复，也会应用（2026-10-15 修订，8.8：此前采纳的证书不带修复，leader 仍为 `CLOCK_SUSPECT`，且重复导入被判为“未变化”而无法修复）。
- 修复是带序号的显式操作：导入 `sequence` 更高且 `issued_at <= now` 的新证书时，leader 把 `watermark = issued_at`、`repair_seq = sequence` 持久化并经 `SyncLicense` 同步。**follower 三分支：** `repair_seq > 本地` 则无条件采用（允许降低水位）；`repair_seq == 本地` 则 `watermark = max`；`repair_seq < 本地` 忽略该请求的水位（v3 的“否则取 max”会让同任期迟到的低序号请求重新抬高水位）。重复导入现有证书不重置水位。**采纳对端水位前先钳位到本地 now（2026-10-25 修订，8.22 方案 5）：** 三分支以 `min(对端 watermark, 本地 now)` 代入，`SyncLicense`、`adoptAhead` 与对账采纳三处同样处理。理由：`CLOCK_SUSPECT` 针对本机时钟相对自己历史的回退，对端超前只是偏差；此前对账路径避免了“一台时钟超前的 master 让新 leader 进入 `CLOCK_SUSPECT`”，`SyncLicense` 路径却把 leader 的当前墙钟原样推给 follower，leader 快 N 秒（N > 5）时 follower 每次推送后都报 `CLOCK_SUSPECT`。代价：新加入且时钟被回拨的 master 不再继承集群水位，该情形仍由节点侧“本地墙钟减 5 分钟”下限（4.3）兜底。
- volume/filer 重启不缓存 `master_deny_reason`，到首帧前按缓存证书放行，秒级窗口，接受。

### 4.6 热路径性能约束（硬性）

量级换算：1 万亿次/天 ≈ 1,160 万次/秒（集群合计）。按 100 个 volume server 摊，单节点约 11.6 万次/秒；一次业务读可能触发多个 chunk GET。守卫目标单次 ≤ 30 ns、0 分配，单节点每秒新增 CPU 约 3.5 ms。**这只说明守卫本身的成本**，整条链路以 S13 实测为准。

新增工作的完整清单：

| 路径 | 新增工作 | 说明 |
| --- | --- | --- |
| volume/filer GET（有效态） | 一次 `AllowRead()` | 三次整数比较 |
| volume GET（拒绝态） | 令牌头解码与恒时比较、预序列化 403 写出 | 只在拒绝态 |
| filer 内部读取（manifest、日志回放、merge、配置文件） | 一次 `atomic.Value.Load` + 一个请求头 `Header.Set` | 每次内部 GET 多一次小分配；属于 `UpdateEntry`/merge/删除回收/订阅回放/配置重载路径，**写路径不是“零新增”**，而是“无新增调用，内部读取多一个请求头” |
| HTTP `?metadata=true`、目录 JSON（M1） | `AllowRead()`；仅在拒绝态且条目有 `Content` 时复制条目置空 | 有效态无复制；拒绝态大目录列表的复制成本在 S13 单独测 |
| gRPC Lookup/List/Subscribe/TraverseBfs（G1，v9） | 有效态每次 RPC 或每条事件一次 `AllowRead()`；拒读态仅对 `len(Content) > 0` 的条目做豁免判断与置空 | 无复制；拒读态大目录与订阅回放在 S13 单独测 |
| volume gRPC 7 个数据 RPC（G2，v9） | 建流时一次 `AllowRead()`；拒读态读一次 incoming metadata 并恒时比较 | 其余 35 个 RPC 不变 |
| filer gRPC `CreateEntry/UpdateEntry`（v9） | 一次 `content_hidden` 布尔判断；只有标志为真时才比较 chunks 并读已有条目 | 来者不带标志时等同零新增 |
| DELETE、PUT、POST | 无 | `maybeAddAuth` 不改 |
| master 心跳/KeepConnected | 每次一次整数比较 | |
| master 成员账本（后台） | 变更批次的快照序列化、落盘、向每个 master 推送并等待确认 | 单次成本随节点数线性，复制量随 master 数线性；不在请求路径，但节点集中重连与切主时会集中发生，S13 单独测 master CPU、分配、写盘与注册延迟 |

| # | 约束 | 实现方式 |
| --- | --- | --- |
| H1 | 写路径无新增调用 | `POST/PUT/DELETE` 分支、S3 写接口、`maybeAddAuth` 不改；diff 只落在 P0 契约第 4 节与 5.2、5.3 列出的挂点。filer gRPC `CreateEntry/UpdateEntry` 只多一次 `content_hidden` 判断（v9） |
| H2 | gRPC 元数据路径有效态零复制、零分配（v9 改） | 目录列表的 HTML 路径、`FindEntry` 不经过守卫；gRPC `Lookup/List/Subscribe/TraverseBfs` 与三处 HTTP JSON 出口有效态只多一次 `AllowRead()` |
| H3 | 读路径零系统调用 | 守卫只读 ticker 维护的 `atomic.Int64` |
| H4 | 读路径零锁 | `atomic.Pointer[Snapshot]` |
| H5 | 有效态零分配、零 map、不看请求头 | 快照预计算；令牌比较只在拒绝路径 |
| H6 | 拒绝路径尽量少分配 | 预序列化 body；`w.Header().Set` 会分配，“零分配”只对 `AllowRead()` 断言 |
| H7 | 不新增每请求 goroutine、channel、context、defer | |
| H8 | 不新增每请求指标标签 | 仅拒绝路径加一个 counter |
| H9 | 验签、解析、落盘不在请求线程 | |
| H10 | master 不在读写路径 | |
| H11 | 不改 gRPC 拦截器 | G1、G2 写在具体 handler 内（v9） |
| H12 | 证书变更不触发重启、缓存清空、连接重建 | |
| H13 | 内存常量级 | |

守卫伪代码：

```go
func (g *Guard) AllowRead() (ok bool, deny *Snapshot) {
    s := g.snap.Load()            // 构造时即为 NOT_READY 快照，永不为 nil
    now := g.clock.Load()
    if s.allowRead && now >= s.notBefore && now < s.expiresAt {
        return true, nil
    }
    return false, s
}
```

基准：`go test -run '^$' -bench 'BenchmarkGuardAllowRead' -benchmem ./weed/license/`，验收机 5 轮取中位数，目标 < 30 ns/op、0 allocs/op；`TestGuardAllowReadNoAlloc` 用 `AllocsPerRun` 断言并纳入常规 `go test`。这些是待验证目标，不能据此认定实际无退化。

---

## 5. 管理接口与错误语义

### 5.1 master 接口（HTTP 明文即可）

| 接口 | 用途 | 说明 |
| --- | --- | --- |
| `GET /license` | 状态 `VALID/EXPIRED/NO_LICENSE/NOT_YET_VALID/CLOCK_SUSPECT/FEATURE_MISSING/RECONCILING`、newest/prev 摘要与当前生效证书、到期时间、三类节点在线/宽限/上限、`state_version`、`repair_seq`、`voters_acked`（已确认当前状态的 voter 列表，goraft 缩容前必查） | 脱敏 |
| `GET /license/deployment` | 申请 DTO | |
| `POST /license/validate` | `{"certificate":"<jws>"}` 或 multipart `file`；返回 `valid`、`unchanged`、`effective_now`、`certificate`、`changes`（2026-10-21 加，8.20） | 每进程 10 次/分钟令牌桶 |
| `POST /license/import` | 落盘、同步、下发；返回 `{"accepted":true,"effective_now":bool,"state_version":n,"voters_ok":[...],"peers_failed":[...],"changes":[...]}`（`changes` 2026-10-21 加，8.20）；非 leader 或对账中 503 | 导入串行；相同字节幂等 |
| `DELETE /license/nodes/{addr}` | 删除宽限中的账本条目 | 不踢在线节点；宽限中的地址见 `GET /license` 的 `grace_nodes`（P5 补，8.5） |
| gRPC `GetLicense/ImportLicense` | `weed shell license.show / license.import -file / license.validate`；`-disableHttp` 的唯一导入入口 | |

安全口径：**导入安全依赖厂商签名与集群绑定，不依赖访问控制。** `guard.white_list` 为空时放行所有请求，`GetActualRemoteHost` 无条件信任 `X-Forwarded-For`（[guard.go:78](../weed/security/guard.go:78)），不能当作鉴权；接口仍套 `guard.WhiteList` 与 `/vol/grow` 一致，但不列为防护。

限制：body ≤ 96 KiB；证书原文不写日志、不进 URL；400（格式/验签/集群不匹配）、409（sequence 不递增或同序号不同内容、额度不足）、503（非 leader、`RECONCILING`、`PARTIAL_COMMIT`）。`/license*` 放在 `if !ms.option.DisableHttp` 块内。

命令行补充（决策 8，做）：`weed master -license.file`、`weed server -master.license.file`，leader 接管时在开放注册前提交（3.4）；离线命令 `weed license.init -mdir=<dir> [-clusterId=<uuid>]` 生成 `cluster_id` 并打印申请 DTO（registered 计数为 0），用于升级前取证与状态丢失后恢复（3.4）。

### 5.2 S3 层的两处改动

- **CopyObject 源状态校验。** `DownloadFile` 不检查状态（[http_global_client_util.go:184](../weed/util/http/http_global_client_util.go:184)），复制 handler 直接写入 `resp.Body`（[s3api_object_handlers_copy.go:91](../weed/s3api/s3api_object_handlers_copy.go:91)）。修正：`StatusCode` 非 200/206 返回 `ErrInvalidCopySource`，目标不变。约 4 行。
- **GetObject 错误映射。** `passThroughResponse` 原样转发 JSON（[s3api_object_handlers.go:251](../weed/s3api/s3api_object_handlers.go:251)）。修正：响应回调里若 `X-MassDB-License-Reason` 非空，改用 `s3err.WriteErrorResponse(w, r, s3err.ErrAccessDenied)`。约 5 行。

---

## 6. 页面

- master 页面（两份模板）在 “Cluster status” 之后新增 “License” 区块：状态表（状态、客户、版本、生效证书与下一张证书、到期、剩余天数、三类节点在线/宽限/上限、cluster_id、repair_seq）+ 导入表单（文件或文本，“校验”“导入”，二次确认，结果就地显示）。约 60 行原生 JS。
- `uiStatusHandler` 增加 `License` 字段。`/ui/index.html` 不代理，follower 显示本地快照。导入提交到 `/license/import`，非 leader 返回 503 时页面提示 leader 地址。
- filer、volume 页面各加一行状态（可选）。页面不保存证书。到期前 30 天变黄、已到期变红。

---

## 7. 改动清单

### 7.1 新增包 `weed/license/`

| 文件 | 内容 |
| --- | --- |
| `reason.go` | 状态码、管理错误码与 HTTP/gRPC 映射（P1 已完成） |
| `strictjson.go`、`text.go` | 协议 v1 严格 JSON 解析与文本码点规则，与签发工具逐条一致（P1 已完成） |
| `claims.go` | 声明结构、字段校验（P1 已完成） |
| `jws.go` | compact JWS 拆分、规范 base64url、Ed25519 验签（P1 已完成） |
| `trust.go` | 信任清单解析、内置公钥清单、测试根注入点（P1 已完成） |
| `state.go` | `(newest, prev)` 比较、`Effective(now)` 推导、有效额度、导入校验、时钟水位与三分支修复（P1 已完成） |
| `guard.go` | 不可变快照、`AllowRead()`、预序列化 403、内部令牌校验（P1 已完成） |
| `ledger.go` | 成员账本、宽限、释放、接管转换、`(term, revision)` 排序（P1 已完成；原子预占由 P2 manager 锁提供） |
| `manager.go`（P2 已完成）、`master_state.go` | master 侧：导入/验证/水位与 repair_seq/voter 多数派推送/任期与配置索引校验/选主对账与写回/账本 `(term,revision)` 复制与对账/对账期间注册延后/内部令牌/`CheckMasterJoin` 两阶段/`BeforeRemoveVoter`/`voters_acked` |
| `node.go` | volume/filer 侧：接收 `LicenseUpdate`、整体替换、缓存、毫秒时间锚定、`NODE_REJECTED`（P1 已完成） |
| `*_test.go` | 已知答案向量、到期边界、回拨与三分支修复、`(newest,prev)` 排序与生效推导、降额、账本与宽限、voter 多数派、任期拒绝、对账写回、`NOT_READY` 下无头/空头/错长度令牌拒绝、`AllocsPerRun`、Benchmark |

### 7.2 修改的既有文件

| 文件 | 改动 | 估计行数 |
| --- | --- | --- |
| `weed/pb/master.proto` + 重新生成 | 4.1（P2 已完成） | proto ~90 |
| `weed/server/master_server.go` | manager 初始化；5 条路由；`OnPeerUpdate` 加入走两阶段（后台等待对账完成）；远程移除经 `RaftRemoveServer` 已覆盖（P2 已完成） | ~30 |
| `weed/server/raft_hashicorp.go`、`raft_server.go` | `updatePeers` 改为后台执行、等对账完成后加入走两阶段、移除前重提交；新配置引导前 `CheckBootstrap`（P2 已完成） | ~40 |
| `weed/server/master_grpc_server_raft.go` | `RaftAddServer` 走两阶段；`RaftRemoveServer` 前重提交；不再跨调用持有 `RaftServerAccessLock`（P2 已完成） | ~15 |
| `weed/server/master_grpc_server.go` | `SendHeartbeat`：账本判新/预占/对账冻结 + 首帧与按流对账 + 响应填容量字段；`KeepConnected`：`addClient` 前预占 + 首帧 + ticker 对账；日志脱敏（P2 已完成对账冻结与日志脱敏；P3 已完成首帧与按流对账；P4 已完成准入预占与断开宽限） | ~60 |
| `weed/server/master_grpc_server_license.go`、`master_license.go`（新） | `GetLicense/ImportLicense/SyncLicense/GetLedger/SyncLedger`；raft 适配、peer RPC、类型转换（P2 已完成） | ~380 |
| `weed/server/master_license_peers.go`（新）、`command/scaffold/master.toml` | master 之间 4 个 RPC 的来源 IP 校验与 `[master.license] peer_ips`（P4 后加固，8.4） | ~200 |
| `weed/server/master_server_handlers_license.go`（新） | 5 个 HTTP handler（P2 已完成） | ~170 |
| `weed/cluster/cluster.go` | `ListAllClusterNodes`（对账时保留在线 filer）（P2 已完成） | ~20 |
| `weed/server/master_server_handlers_ui.go` | args 加 `License`（P2 已完成） | ~4 |
| `weed/server/master_ui/license.html`（新）、`master.html`、`masterNewRaft.html`、`templates.go` | 两份模板共用一个 License 区块（P2 已完成） | ~130 |
| `weed/wdclient/masterclient.go` | `OnLicenseUpdate` 回调；首帧、循环、`PermissionDenied license:` 三处调用（P3 已完成） | ~20 |
| `weed/server/volume_server.go` | `license *license.Node`；启动读缓存（P3 已完成） | ~6 |
| `weed/server/volume_grpc_client_to_master.go` | 处理 `in.License`、`in.LicenseRejectReason`（P3 已完成；P4 拒绝帧跳过卷参数比较、拒绝日志只打一次） | ~15 |
| `weed/server/volume_server_handlers_read.go` | V1 守卫 + 拒绝路径令牌校验；`ReadMode=proxy` 转发沿用原方法（P3 已完成，8.2） | ~10 |
| `weed/server/filer_server.go` | `license *license.Node`；注册回调；启动读缓存；`saveToFilerLimit>0` 告警；`filer.SetInternalReadToken`（P3 已完成） | ~12 |
| `weed/server/filer_server_handlers_read.go` | F1 守卫 + 清理对象头；`?metadata=true` 隐藏 Content 并打标记（P3 已完成） | ~14 |
| `weed/server/filer_server_handlers_read_dir.go` | 目录 JSON 隐藏 Content 并打标记（P3 已完成） | ~8 |
| `weed/server/filer_server_handlers.go` | F2 守卫（P3 已完成） | ~3 |
| `weed/filer/filechunk_manifest.go`、`weed/filer/stream.go` | `retriedStreamFetchChunkData` 加 `extraHeader` 参数；`fetchWholeChunk`、`ChunkStreamReader`、`StreamContent` 三处附加令牌（P3 已完成） | ~15 |
| `weed/filer/internal_token.go`（新） | 包级 `SetInternalReadToken`（P3 已完成） | ~15 |
| `weed/util/http/http_global_client_util.go` | `ReadUrlAsStreamWithHeader`（抽出内部实现）（P3 已完成） | ~12 |
| `weed/mount/filehandle.go`、`weed/mount/filehandle_read.go` | 记录 manifest 解析错误与隐藏正文，失败状态下限频重解析/重取，读取时返回 `EIO`（1.6）（P3 已完成，新增 `filehandle_license.go`；另修复 `weedfs_link.go`、`weedfs_attr.go` 的内联文件硬链接与截断，见 8.2） | ~40 |
| `weed/pb/filer.proto` + 重新生成（含 `other/java` 副本） | `Entry.content_hidden = 13`（v9，1.5） | proto ~2 |
| `weed/filer/entry.go`、`weed/filer/read_write.go`、`weed/filer/meta_aggregator.go`、`weed/filer/filer.go`、`weed/filer/meta_replay.go` | `ContentHidden` 映射；读取辅助报错；filer 间订阅与引导带令牌；Replay/引导拒收隐藏条目（v9）（P3 已完成） | ~40 |
| `weed/server/filer_grpc_server*.go`（含 `_kv.go`、`_sub_meta.go`、`_traverse_meta.go`、`_rename.go`、`_remote.go`） | G1 隐藏点、`KvGet` 白名单、写入合并规则（v9）（P3 已完成，新增 `filer_license.go`） | ~110 |
| `weed/pb/filer_pb_tail.go` 与 5 个复制/备份命令、`fs.cat`/`filer.cat`/`fs.meta.save`/`fs.meta.notify` | `FailOnHiddenContent` 与隐藏正文报错（v9）（P3 已完成；另含 `filer.meta.backup` 首轮遍历、`volume.fsck`、webdav，见 8.2） | ~35 |
| `weed/server/volume_grpc_{copy,read_write,read_all,tail,copy_incremental,erasure_coding,query}.go`、`weed/operation/tail_volume.go`、`weed/storage/store_ec.go` | G2 入口守卫与 3 处出站令牌（v9）（P3 已完成） | ~80 |
| `weed/command/license_init.go`（新） | 离线 `weed license.init`（v9，3.4）（P2 已完成） | ~60 |
| `weed/s3api/s3api_object_handlers_copy.go` | 源状态校验（P3 已完成） | ~4 |
| `weed/s3api/s3api_object_handlers.go` | GetObject 403 映射（P3 已完成） | ~5 |
| `weed/stats/metrics.go` | 指标（P3 做 `license_denied_reads_total`；P4 补齐 master 侧 `license_state`、`license_expires_at_seconds` 与三个节点数量指标，见 8.3） | ~20 |
| `weed/command/master.go`、`weed/command/server.go`、`master_follower.go` | `-license.file`、`-master.license.file`、`-mdir` 临时目录告警（P2 已完成） | ~40 |
| `weed/shell/command_license.go`（新） | `license.show/import/validate`（P2 已完成） | ~140 |

合计：新包约 1,700 行（含测试约 2,700 行）；既有 Go 文件修改约 720 行，模板约 140 行，新增 Go 文件约 550 行。不改 raft 状态机、不改 webdav；v9 起 `filer.proto` 增加一个字段，volume/filer gRPC 在拒读态的返回按 1.2 G1/G2 变化；mount 只改 1.6 的读取错误传播。

### 7.3 签发工具（独立项目，另计；2026-10-14 修订）

不在 massdb-sql 的签发工具上加 OSS 产品档案（原计划的改法与第 9 节决策 7 原默认值作废）。massdb-sql 的 `tools/license-issuer/` 只作测试用：P1 用它和 OpenSSL 3 生成 `weed/license/testdata` 的互通向量（[gen_issuer_vectors.py](../weed/license/testdata/gen_issuer_vectors.py) 调用其内部 `_sign_claims`，绕过写死 SQL 产品的 `validate_claims`）；真实进程验证用一次性测试根。

生产签发工具放在另一个独立项目（项目待定），须满足：
- 协议 v1：compact JWS（Ed25519，`typ="massdb-license+jws"`）、严格 JSON 与文本码点规则、`export-trust` 信任清单格式，与 P0 契约第 1 节一致。
- OSS 产品档案：`product="MassDB OSS"`，limits 为 `max_master_nodes`、`max_volume_servers`、`max_filers`，功能 `DATA_READ`；`prepare-claims` 从 `GET /license/deployment` 或 `weed license.init` 的申请生成声明并接受三个 `--max-*`；`sign`、`verify`、`export-trust`。
- 发布前用它重新生成互通向量，并让 `weed/license` 的测试对它通过。

它产出生产 kid 与公钥（7.4），是发布的前置条件；P5 的功能验收仍用测试根，不被它阻塞。工作量随项目另计。

2026-10-21 修订：独立签发项目定为 MassDB Manager 的签发中心 `manager-license`（Manager 计划决策 D3），为 OSS 建自己的密钥；它的互通测试经 `weed/license` 的外部向量测试验证本仓库的验签，安装包由 `make massdb-package` 注入测试根，见 8.21。

### 7.4 开工前准备

- protoc v5.28.3、protoc-gen-go v1.34.2、protoc-gen-go-grpc v1.5.1（本机未安装，版本须与现有生成文件一致；v9 起 `master.proto` 与 `filer.proto` 都要重新生成，`filer.proto` 同步到 `other/java`）。
- 生产 kid 与公钥（发布前，由 7.3 的独立签发项目产出）；P1–P5 用测试根。

---

## 8. 实施顺序与验收

| 阶段 | 工作 | 完成证据 | 估时 |
| --- | --- | --- | --- |
| P0 契约 | 冻结 claims、挂点、原因码、第 9 节决策 | **已确认（2026-10-13）**：[P0 契约](license-p0-contract-20261013.md) | 0.5 天 |
| P1 核心包 | `weed/license` 除 `manager.go` 外全部 + 单测（manager 依赖 proto 与 raft，并入 P2） | **已完成（2026-10-13）**：`go test -race ./weed/license/` 通过；签发工具 + OpenSSL 3 签发的互通向量验证通过；`AllowRead` 0 分配，单核约 0.65 ns/op；7.1 测试清单中 voter 多数派、任期、对账写回三项随 manager 在 P2 完成 | 3 天 |
| P2 master 管理 | proto（先行）、manager、HTTP/gRPC、voter 多数派、任期与配置索引、对账写回、账本 `(term,revision)` 复制与对账、对账期间注册延后、加入两阶段、移除前重提交、`voters_acked`、cluster_id、令牌、水位、页面 | **已完成（2026-10-13）**，证据与差异见 8.1：`go test -race ./weed/license/` 通过（含 19 个 manager 用例）；`server/command/shell/cluster/topology/wdclient/pb` 测试通过；真实进程验证单 master、goraft 与 hashicorp 3 master、3→5 扩容与缩容、切主对账、页面导入、`license.init` 与 `-license.file`、日志无证书与令牌 | 6 天 |
| P3 下发与拦截 | 按流对账、volume/filer 守卫、M1、G1（含 `filer.proto`、合并规则、客户端改动）、G2、缓存、内部令牌传递链、mount 错误传播、S3 两处 | **已完成（2026-10-14）**，证据与差异见 8.2：S1–S4、S6、S15–S20 真实进程通过（mount 与 `-cipher` 在 Linux 虚拟机上补测；mount 硬链接与截断的上游缺陷已修复）；S5、S7、S8 属 master 侧，P2 已验证；新增 server/filer/mount/wdclient/pb/license 单测通过 | 6.5 天 |
| P4 节点额度 | 账本、宽限、预占、`NODE_REJECTED`（volume 与 filer）、三个 master 加入入口、无证书额度 0 与 master 启动检查 | **已完成（2026-10-14）**，证据与差异见 8.3：S9–S12 与 S21 的节点部分真实进程通过（单 master、hashicorp 3 master 强杀切主、`weed server`、基线版本升级）；10 分钟宽限实测 603 秒自动释放；新增 license 单测通过（master 加入入口与启动检查的 S21 部分 P2 已验证） | 2.5 天 |
| P5 验收与交付 | `go vet`、`go test ./weed/...`、Benchmark 命令、`CGO_ENABLED=0` 构建、`weed server` 一体化、docker-compose、运维手册 | 全部场景记录；S13。**进行中**：**已完成（2026-10-14）**：全量检查、S14、`weed server` 一体化、docker-compose 与[运维手册](license-operations-manual-20261014.md)见 8.5（其间发现并修复 goraft 全集群重启后对账卡死），外部审核修复见 8.6、8.8–8.14，S13 见 8.7 | 2 天 |

合计约 20.5 人天（单人，不含签发工具；签发工具为独立项目，另计，见 7.3）。v9 增量见 P0 契约 8.1。

### 8.1 P2 实施记录（2026-10-13）

**交付：** `weed/license/manager.go`、`master_state.go` 与单测；`master.proto`（4.1）与 `filer.proto` 的 `content_hidden = 13`（含 `other/java` 副本）按原版本工具链重新生成，`master_pb`、`filer_pb` 的既有部分逐字节不变（`mq_agent.pb.go` 与其 proto 在上游已不一致，本次不重新生成）；7.2 中标注 “P2 已完成” 的文件。

**验证：**

| 项 | 方式 | 结果 |
| --- | --- | --- |
| 单 master 导入/校验/幂等/错误码（400/409/413/405/404/503） | 真实进程 + curl | 通过；`weed shell` 的 `license.show/validate/import` 通过 |
| 未来生效证书 | 真实进程 | `newest=2, prev=1`，生效证书仍为 1，页面显示 “Next certificate” |
| 无证书 3 master 启动 | goraft 真实进程 | 以 `license: NODE_LIMIT_EXCEEDED master 3/0 ...` 退出；hashicorp 走同一 `CheckBootstrap`（单测与代码路径） |
| `license.init` + `-license.file` | goraft 与 hashicorp 各 3 master | 正常组建；冲突 id 拒绝；重复启动时已应用的证书跳过 |
| 切主对账 | goraft（强杀 leader）、hashicorp（杀 leader） | 新 leader 在 1 秒内完成对账；对账期间 KeepConnected 返回 `Unavailable license: RECONCILING`，客户端按既有循环重试 |
| 部分提交与追平 | goraft：1 台宕机时导入 | 200，`peers_failed` 列出宕机节点；重启后经 5 秒重推追平，`voters_acked` 由 2 台变为 3 台 |
| 加入两阶段 | hashicorp 3→5（OnPeerUpdate） | 先 nonvoter、同步后提升 voter，5 台都确认当前状态 |
| 额度 | hashicorp | 上限 4 时第 5 台加入被拒 `NODE_LIMIT_EXCEEDED master 4/4`；导入上限 3 被拒 `LICENSE_NODE_LIMIT_TOO_SMALL`；同序号不同字节 409、低序号 409 |
| 移除前重提交 | hashicorp `cluster.raft.remove` | 重提交到剩余 4 台后移除 |
| 页面 | 应用内浏览器 | 两份模板渲染；“Validate” 通过；在 follower 页导入提示 leader 地址；在 leader 页导入成功并刷新（浏览器面板自动取消 `confirm()`，导入经页面同一函数直接调用验证） |
| 日志脱敏 | 扫描全部进程日志 | 无证书片段、无 `internal_read_key`；`state.json` 权限 0600、目录 0700 |
| S7b/S7c/S7d/S7e/S7h/S7i、S8 | 单测（假 raft 集群） | 未达多数派 503 与幂等重试、nonvoter 不计数、5 voter 写回后 C/D/E 仍得 11、旧任期请求被拒、移除被不可达 voter 阻止、配置变化返回 `MEMBERSHIP_CHANGED`、账本跨任期排序、时钟回拨与三分支修复。真实进程无法改系统时钟，S8 只有单测 |

**与计划的差异：**

1. follower master 追平改为 leader 每 5 秒重推（3.3 第 6 条），不依赖 KeepConnected。
2. `SyncLicense` 的接收方若持有更高序号的状态，不报错而是返回自己的状态，leader 按 3.2 规则采纳并重推；只有返回状态与所推状态相同的 voter 才计入多数派。
3. 选主对账只采纳更高 `repair_seq` 的时钟状态（4.5）。
4. 对账的接管转换之后，把当前仍连在本 leader 上的 volume 与 filer 重新置为在线（`Connected` 回调），覆盖“同一 master 失去又重获 leader、流未断开”时连接不会重新注册的情况。（2026-10-25 修订，8.22 方案 6：改为在 `m.mu` 内按 manager 自己的 `conns` 置为在线，`Connected` 回调与 `licenseConnected()` 删除。）
5. 每个 master 在 raft 启动前把 `-license.file` 应用到自己的本地状态（不只是 leader）；leader 的对账取多数派中的最高状态并写回后才开放注册，满足 3.4 “开放注册之前提交” 的要求。
6. 推送前若账本有未发出的变更，先分配新版本，保证同一版本号只对应一份内容。
7. `master.follower` 的 manager 不落盘（它不参与 raft）。
8. 移除前重提交失败返回 `PARTIAL_COMMIT`（503）。
9. 导入与校验除 JSON 与 multipart 外也接受证书原文作为请求体（便于 `curl --data-binary @cert`）。
10. 原计划 P4 的 master 启动检查与 P3 的对账期间注册延后已在 P2 完成；volume/filer 的准入预占（`AdmitNode/DisconnectNode` 已实现并有单测）仍在 P4 接入心跳与 KeepConnected，首帧与按流对账在 P3。

**发现的上游行为（未改）：** 没有任何 volume server 时 KeepConnected 不发首帧，`weed shell` 一直等待连接；`-peers` 只允许奇数个 master，扩容验证因此按 3→5 进行。

### 8.2 P3 实施记录（2026-10-14）

**交付：** 7.2 中标注 “P3 已完成” 的文件。新增 `weed/server/license_node.go`（节点创建、HTTP/gRPC 读守卫、出站令牌、拒绝计数与限频日志）、`weed/server/filer_license.go`（G1 隐藏、M1 副本、写入合并规则、`KvGet` 白名单）、`weed/filer/internal_token.go`、`weed/mount/filehandle_license.go`，以及只供测试导入的 `weed/license/licensetest`（构造 VALID、NO_LICENSE、NOT_READY 状态的节点）和各自单测。

**验证（真实进程：1 master + 2 volume + filer1（`-s3 -webdav`、`-saveToFilerLimit=1024`）+ filer2（独立存储）+ 冷启动的 filer3；另起一组 `weed server -filer -s3 -webdav`）：**

| 场景 | 结果 |
| --- | --- |
| S1 无证书 | filer、S3 写入 201/200；filer HEAD、`?metadata=true`、目录列表与 S3 Head/List 200；volume GET、filer GET、Range 读、`?proxyChunkId=` 403 `NO_LICENSE`，body 与头符合契约 2.2；S3 GetObject 为 403 `AccessDenied` XML 并保留原因头；webdav 下载中断（curl 退出码 18） |
| S2 导入 | 5.3 秒内全部节点恢复（≤ 1 个心跳周期）；其间曾有一次 filer 已放行而 volume 尚未更新，filer 返回 500，属两层守卫的过渡 |
| S3 到期边界 | 60 秒有效期证书：到期秒起 volume、filer、S3 均 403 `EXPIRED`，写入与 HEAD 不受影响；续期后 5 秒恢复 |
| S4 未来生效 | `prev` 在 +75 秒到期、`newest` 在 +45 秒生效：+75 秒之后读取仍放行，节点按本地时钟切换，master 未下发 |
| S6 master 失联 | 停 master 30 秒，volume 直连与两个 filer 读取均 200；master 恢复后一致 |
| S15 CopyObject | 无证书返回 `InvalidCopySource`（本版本文案为 `InvalidArgument` “Copy Source must mention…”），目标不存在；续期后复制成功且内容正确 |
| S16 内部维护 | 10001 块（含 manifest）文件 gRPC `UpdateEntry` 成功，`?resolveManifest=true` 返回 10001 块；1001 次追加触发小块合并（合并后 2 块）；filer2 经对等订阅回放得到完整正文；从 0 订阅可回放持久化日志；filer.conf 修改后 filer2 热加载生效 |
| S17 内联正文 | HTTP 文件与目录 `?metadata=true`、目录 JSON，gRPC Lookup/List/Traverse/Subscribe/StreamRename 均隐藏、保留 `FileSize`、HTTP 带 `X-MassDB-Content-Hidden`；`/etc/iam/identity.json`、`/etc/seaweedfs/filer.conf` 不隐藏；带正确令牌可见，错值与错长度令牌不放行；证书有效时不隐藏 |
| S18 单进程 | `weed server -filer -s3 -webdav` 无证书：webdav 下载分块文件与内联文件均中断；S3 GetObject 403；manifest 文件 `UpdateEntry` 成功 |
| S19 volume gRPC | 7 个 RPC 在无令牌、错值、错长度时均 `PermissionDenied license: NO_LICENSE`，正确令牌放行，`.idx/.vif` 不受限；`volume.move`、`ec.encode`、`ec.balance` 成功；EC 卷经只持 1 个分片的节点 HTTP DELETE 成功（跨节点读分片带令牌）；`weed backup` 报 `license: NO_LICENSE` |
| S20 回写与复制 | S3 打标签、CopyObject REPLACE、gRPC 查后回写之后正文完整；带隐藏标志在不存在的路径创建返回 `CONTENT_HIDDEN`；`filer.backup` 停在隐藏事件，续期后从原偏移续传且目标端内容完整；`fs.meta.save`、`fs.cat`、`filer.cat`、`filer.meta.backup` 报 `CONTENT_HIDDEN`，`fs.meta.save` 不留下转储文件；冷启动的 filer3 引导后正文完整；S3 重启后 IAM 鉴权生效；`KvGet` 非偏移键被拒、偏移键可读 |
| 缓存 | 重启 volume 后 0.4 秒内按缓存证书放行（不等首帧）；缓存文件 0600，只含 `newest/prev/cluster_id` |
| 日志 | 全部进程日志无证书片段与令牌；拒绝日志每节点每分钟至多 1 条 |
| 单测 | 新增 server、filer、mount、wdclient、pb、license 用例；明文与 `-cipher` 两种块读取都验证了令牌只出现在三个内部读取函数上、用户读取不带令牌；`go test -race`（license、filer、mount、wdclient、pb）与 CI 标签构建通过 |

**Linux 补测（Parallels 虚拟机 Fedora 42 aarch64，FUSE 3；1 master + 2 volume + 1 filer（`-encryptVolumeData`、`-saveToFilerLimit=1024`）+ 1 mount（`-cacheCapacityMB=0`）；证书按虚拟机本地时钟签发）：**

| 场景 | 结果 |
| --- | --- |
| S1 mount 读 | 无证书时全新挂载：分块加密文件、含 manifest 的文件、隐藏正文的内联文件 `read` 均返回 `EIO`，没有返回全零；`ls` 显示真实大小；写入新文件成功 |
| S1b 原句柄恢复 | 保持三个句柄不关、每秒读一次；导入后分块文件 2.6 秒恢复，manifest 文件与内联文件 4.6 秒恢复（上限为心跳周期 + 5 秒）；失败期间每个句柄每 5 秒重试一次；之后重新 `open` 也可读 |
| S17、S20 mount | 证书无效时重新挂载，meta cache 里缓存的是隐藏条目：`chmod`、`setfattr`、`touch` 发出的请求带 `content_hidden`，续期后正文完整、权限与 xattr 生效；续期后新打开的文件首次读取即恢复 |
| S16 `-cipher` | 加密的 manifest 块在到期状态下经 mount `chmod` 触发的 `UpdateEntry` 成功；1001 次追加在加密集群上合并为 2 块；`filer.meta.tail -timeAgo=2h` 从头回放持久化日志无错误 |
| `volume.fsck` | 到期时因 manifest 无法解析而中止报错；续期后正常完成，manifest 引用的 10001 个块没有被误判为孤儿 |

同一环境用基线版本（`f2fcc241e`，不含证书代码）对照，确认 mount 的两个上游缺陷与证书无关，已在 P3 修复（见下方“P3 中修正的问题”第 5、6 条）：对内联文件做硬链接返回 `EIO`、留下无法 `stat` 的链接名并把原文件正文清成全零；对内联文件 `truncate -s 0` 不生效，filer 上正文不变。修复后在同一环境复测：证书有效时，经 HTTP 写入的内联与分块文件建硬链接（含第二个链接）成功，各名字 inode 相同、链接数正确、内容一致，删除一个名字后其余可读；截断到 0 得到空文件，截断到 5 字节得到前 5 字节。证书无效且 meta cache 中是隐藏条目时，建硬链接成功、续期后各名字正文完整；截断到 0 得到空文件；截断到非 0 长度在关闭时返回 `EIO`（字节未知，filer 返回 `CONTENT_HIDDEN`），正文不变。另外 mount 对每次失败的读取都按上游代码打一条 warning，失败态下应用反复读取会产生大量客户端日志（不发网络请求）。

**P3 中修正的问题：**

1. P2 的 `Manager.Update` 只给 filer 填 `internal_read_key`，volume 拿不到 key，一律拒绝令牌，manifest 解析、合并、日志回放在无证书时都会失败（与 1.4 第 1 条、契约第 3 节不符）。改为 volume 心跳流与 filer 的 KeepConnected 流都带 key，shell、broker 等其它客户端不带。
2. 上游 volume 在 `ReadMode=proxy` 下把 HEAD 改成 GET 转发，无证书时 HEAD 也会被目标 volume 拒绝；改为沿用原方法。
3. shell `volume.fsck` 在 shell 进程内解析 manifest（没有令牌），上游遇到解析失败只打印一行并继续，manifest 引用的数据块会被报告为孤儿，加 `-reallyDeleteFromVolume` 会误删在用数据。改为解析失败即中止并报错；契约 5.2 中“`volume.fsck` 只取 `.idx`、不受影响”对含 manifest 的文件不成立，已修订。
4. 上游 `weed filer.cat` 吞掉全部错误、退出码为 0；改为错误打印到 stderr 并以 1 退出，隐藏正文时不会留下看似完整的空输出。
5. 上游 mount 硬链接（`weedfs_link.go`）：经 filer HTTP 或 S3 写入的文件没有 inode 属性，`Link` 以 inode 0 回复内核，链接失败并返回 `EIO`；新名字的 `CreateEntry` 不带 `Content`，filer 按它重写共享的硬链接数据，内联正文被清空。改为 inode 为 0 时使用内核给出的旧 inode 并写回条目，新名字带上 `Content`、`ContentHidden`、`RemoteEntry`；filer 的写入合并规则在新名字不存在时以共享的硬链接数据作为已有条目，隐藏正文的文件也能建硬链接。
6. 上游 mount 截断（`weedfs_attr.go`）只裁剪分块与 `FileSize`，内联正文原样保留，filer 继续返回旧字节；改为同时裁剪内联正文，截断到 0 时清除隐藏标志。

**与计划的差异：**

1. KeepConnected 首帧：授权帧先于卷位置单独发送，所有客户端（含 shell）都能收到首帧，顺带消除了 8.1 记录的“无 volume 时 shell 一直等待”。volume 心跳中带授权的响应不填 `Leader`：leader 自己的响应在上游从不带 `Leader`，填了会让以别名地址连接的 volume 多一次重定向。
2. webdav 读入口对 `ContentHidden` 返回错误（2 行；计划原写“不改 webdav”），避免返回全零。上游 webdav 本身不读 `Entry.Content`，内联文件在证书有效时也返回全零，属上游缺陷，未改。
3. mount：来自 meta cache 的隐藏条目在新打开的句柄上首次读取时立即向 filer 重取一次，之后仍 ≥ 5 秒一次，避免续期后每个新打开的文件都先失败 5 秒；mount 写入会丢弃内联正文（上游行为），写入后清除隐藏标志。
4. 新 filer 引导遇到隐藏条目时每 5 秒重试，直到拿到令牌（2026-10-25 起最多 5 分钟，被拒即停，8.22 方案 9）；元数据聚合在事件进入聚合日志与回放之前检查，遇到隐藏条目断开并从该事件重新订阅（不只在 Replay 内拒收）。
5. 指标只实现了 `license_denied_reads_total{server,path}`（prometheus 名带仓库统一前缀，为 `SeaweedFS_license_denied_reads_total`），`license_state`、`license_expires_at_seconds` 与节点数量类指标随 P4/P5。
6. `filer.Entry.ContentHidden` 在 HTTP JSON 里的字段名是 `ContentHidden`（与该结构其它字段一样为大驼峰）。

**未覆盖：** `volume.check.disk` 只在副本间有差异时才读 needle，本次集群无副本，未触发（`ReadNeedleBlob` 守卫已由探测客户端验证）。`CacheRemoteObjectToLocalCluster` 未接远端存储，只有代码路径。S14（新旧混部）未测（P5 已测，见 8.5）。被拒准入（`NODE_REJECTED`）的节点侧处理已实现，master 侧在 P4 接入（已接入，见 8.3）。

### 8.3 P4 实施记录（2026-10-14）

**交付：**

- `weed/server/master_grpc_server.go`：`SendHeartbeat` 在 `GetOrCreateDataNode` 之前、`KeepConnected` 在 `addClient` 之前（仅 `ClientType == "filer"`）调用 `License.AdmitNode`，流结束时 `DisconnectNode`；超额时 volume 先收到 `HeartbeatResponse{license_reject_reason:"NODE_LIMIT_EXCEEDED"}` 再收到 `PermissionDenied`，filer 直接收到 `PermissionDenied`。
- `weed/license/manager.go`、`ledger.go`：按地址记录本 leader 上存活的注册流数，最后一条流结束才开始宽限；已离线条目再次断开不重置 `last_seen`；不再是 leader 的 master 只减计数、不改本地账本副本；拒绝日志每分钟至多 1 条并附合并次数。
- `weed/license/node.go`、`weed/server/volume_grpc_client_to_master.go`、`weed/server/filer_server.go`：`Reject` 返回是否新进入拒绝态，节点只在首次被拒时打 error 日志；volume 收到拒绝帧后跳过该帧的卷参数比较。
- `weed/stats/metrics.go`、`weed/server/master_license.go`：master 每 5 秒按 `GET /license` 的视图刷新 5 个指标（契约第 3 节 P4 修订）。
- `weed/license/manager_test.go`：`TestManagerNodeStreams`。

**验证（真实进程；证书由测试根签发）：**

| 场景 | 环境 | 结果 |
| --- | --- | --- |
| S21 无证书准入 | 单 master | 第 1 个 volume 与第 1 个 filer 被拒，消息 `license: NODE_LIMIT_EXCEEDED volume_server 0/0 no license imported`（filer 同）；volume GET 403 `NODE_REJECTED`；filer 卡在等待 master、HTTP 端口不监听；导入后 3.2 秒内两者在下一次重试时注册 |
| S9 第 4 个 volume | 单 master，`max_volume_servers=3` | 第 4 个被拒 `3/3` 并每 5 秒重试，GET 403 `NODE_REJECTED`；已注册的 v1 停止后在宽限内重启，0.6 秒注册，未被拒 |
| S9 切主 | hashicorp 3 master + 4 volume | 第 4 个在旧 leader 被拒；kill -9 leader 后 1 秒选出新 leader，原 3 个 9 秒内全部重新注册，第 4 个仍被新 leader 以 `3/3` 拒绝 |
| S10 释放 | 单 master | 宽限内第 4 个持续被拒；`DELETE /license/nodes/{addr}` 对在线节点 409 `NODE_ONLINE`、对未知地址 404 `NODE_NOT_FOUND`；释放宽限节点后第 4 个 4.6 秒内注册；空出 1 个名额后同时启动 2 个 volume，只有 1 个注册；不调用 `DELETE` 时 filer 停止 603 秒后名额自动释放（宽限 600 秒加重试间隔与 10 秒轮询粒度），被拒的 filer 随即注册，HTTP 与 S3 读恢复 |
| S10 切主不延长宽限 | hashicorp 3 master | volume 离线 66 秒后强杀 leader：新 leader 与两个 follower 账本中它的 `last_seen` 都不变，仍占名额，第 4 个继续被拒；它在宽限内重启后正常注册 |
| S11 降额 | 单 master | 账本 3 条（含 1 条宽限）时导入上限 2 返回 409 `LICENSE_NODE_LIMIT_TOO_SMALL`；释放后导入成功 |
| S12a | 单 master，`max_filers=1` | 第 2 个 filer 冷启动卡在等待 master、端口不监听，日志 `license: master refused this filer: NODE_LIMIT_EXCEEDED filer 1/1` |
| S12b | 同上 | 对运行中的 f8 `SIGSTOP`，27 秒后 master 经 keepalive 断开其流并进入宽限；释放后名额由冷启动等待中的 f9 占用；`SIGCONT` 后 f8 重连被拒：卷位置缓存仍在，HTTP GET 403 `NODE_REJECTED`，S3 GetObject 403 `AccessDenied`；此前 f8 在宽限内重启（加 `-s3`）未被拒 |
| S21 `weed server` | 单进程 `-filer -s3` | 无证书时内置 volume GET 403 `NODE_REJECTED`，filer 与 S3 端口不监听；导入后 1.6 秒内全部注册，S3 建桶、写入、读取成功 |
| S21 存量升级 | 基线版本（不含证书代码）goraft 3 master + 7 volume + 2 filer，写入数据后停机 | `license.init`（1 台生成、另 2 台 `-clusterId`）→ 签发 → 新版本以 `-license.file` 启动：7 个 volume、2 个 filer 全部注册，日志无准入拒绝，两个 filer 读原有数据 200 |
| 指标 | 单 master | 5 个指标按契约第 3 节 P4 修订导出，数值与 `GET /license` 一致 |
| 日志 | 全部进程 | master 拒绝日志每分钟 1 条并附合并次数；节点只在首次被拒时打 error；无证书原文 |
| 单测 | — | `TestManagerNodeStreams`：同一地址两条流只在最后一条结束时进入宽限、重复断开不移动宽限、非 leader 不改账本副本、超额消息格式；`go test ./...`、license 包与 wdclient 授权用例的 `-race`、CI 标签构建通过；wdclient 的上游用例 `TestConcurrentGetLocations` 在 `-race` 下间歇报 `resetVidMap` 的数据竞争，基线版本同样复现，与本改动无关 |

**与计划的差异：**

1. 断开以“本 leader 上该地址的存活注册流数归零”为准，不跟随上游的 `DataNode.Counter` 与 `ClusterNode.counter`（前者在 `RegisterUuids` 失败的路径上增减不对称）；非 leader 只减计数（契约 2.4 P4 修订）。
2. 拒绝帧不填 `VolumeSizeLimit`、`Preallocate`，volume 跳过该帧的参数比较（契约 2.4 P4 修订）。
3. 指标只在 master 导出（契约第 3 节 P4 修订）。
4. 3.4 表 volume 行的“先 `rack.FindDataNode(ip, port)` 与账本查询”改为只查账本：拓扑里在线的 volume 都已在本 leader 准入（账本内且在线），选主后由对账的 `Connected` 回调置为在线，不需要另查拓扑。

**观察（未改）：** 重启后重新加入的 follower 在下一个账本批次之前持有旧副本：批次只要求 voter 多数派确认，不向落后者补发。切主时对账按多数派取最高版本，S10 切主验证中该 follower 也在对账写回后追平，不影响结论。

**未覆盖：** S9 中“让某个 follower 的 KeepConnected 落后数分钟再强杀 leader”未单独构造：follower master 不从 KeepConnected 应用授权或账本（8.1 差异 1），账本只经 `SyncLedger` 复制、切主按多数派对账，该结论由结构保证，并有 P2 单测。goraft 多 master 的切主准入未测（hashicorp 已测，准入代码不区分 raft 实现；P5 docker-compose 已测，见 8.5）。S13 的 200 个 volume 集中重连属 P5。被拒 filer 的 mount 读取边界见 1.3。

### 8.4 P4 后加固（2026-10-14）

**起因：** 复核 gRPC mTLS 是否必须开启时确认：mTLS 不改变证书对被授权方的约束力，改为可选（1.3）；默认明文部署下最省事的两条绕过路径不需要 master 主机权限，一条是对任意 master 发一次 gRPC 调用取得令牌，一条是伪造 master 下发的时间，本节把两者堵上，均不依赖 TLS。

**交付：**

- `weed/server/master_license_peers.go`（新）、`master_grpc_server_license.go`：`GetLicense(include_state=true)`、`GetLedger`、`SyncLicense`、`SyncLedger` 先检查调用方来源 IP，须属于当前 raft 成员（voter 与 nonvoter）、`-peers` 与本 master 的地址，或 `master.toml` 的 `[master.license] peer_ips`（IP 或 CIDR，逗号分隔，非法值启动时退出）；主机名解析结果缓存 10 秒，解析失败沿用上次结果。否则返回 `PermissionDenied`，拒绝日志每分钟至多 1 条并列出 master 主机。不带 `include_state` 的 `GetLicense`（shell 的 `license.show`）不受影响。
- `weed/license/node.go`：节点时钟取 `max(master 锚点时间, 本地墙钟 − 5 分钟)`（4.3）。
- `command/scaffold/master.toml`：`[master.license] peer_ips`。
- 单测：`TestNodeClockLocalFloor`、`TestLicensePeerGuard`、`TestLicenseCallerIP`。

**验证（真实进程：hashicorp 3 master 绑定 `192.168.0.109` + 1 volume；证书由测试根签发）：**

| 场景 | 结果 |
| --- | --- |
| 组建与对账 | leader 对账写回后 `voters_acked` 为 3 台，日志无误拒 |
| 导入 | 导入序号 2：`peers_failed` 为空，两台 follower 应用 |
| 账本复制 | volume 注册后两台 follower 的账本副本都记为在线 |
| 强杀 leader | 新 leader 在 term 4 完成对账（向存活 peer 的 `GetLicense`/`GetLedger` 放行），volume 重新注册；旧 leader 重启后追平，3 台确认 |
| 非 master 地址 | 经 master 的 localhost 监听调用（来源 127.0.0.1）：4 个 RPC 均 `PermissionDenied license: caller 127.0.0.1 is not a master of this cluster`，状态查询正常；经 `192.168.0.109` 调用放行（与 master 同机，属 1.3 所述 master 主机途径） |
| `peer_ips` | 配置 `127.0.0.1` 的 master 放行回环调用，未配置的 master 仍拒绝；`peer_ips = "not-an-ip"` 启动即报错退出 |
| 测试 | `go test -race ./weed/license/`；`CGO_ENABLED=0 go test` server、command、wdclient、shell；CI 标签构建 |

**2026-10-25 修订（8.22 方案 4）：** 主机名解析失败时把旧地址连同当前时间写回缓存，TTL（10 秒）内不再重试，从未解析成功的名字在 TTL 内直接拒绝；`-peers` 与 raft 成员重复列出的名字只解析一次；单次解析超时由 2 秒降到 1 秒，几个名字连续失败仍在 master 间 RPC 的 3 秒截止内。

**未覆盖：** 节点时钟下限只有单测（真实进程需要伪造 master）。主机名形式的 `-peers` 只以 `localhost` 做了单测（P5 docker-compose 中 3 个 master 经容器主机名互连已验证，见 8.5），NAT、多网卡与 Kubernetes 非 headless 服务地址未测，这类部署须在 `peer_ips` 中补地址，否则选主对账拿不到多数派、注册一直延后，升级说明须写明。goraft 多 master 未测（校验不区分 raft 实现；P5 docker-compose 已验证，见 8.5）。

**环境记录：** 本机 macOS 上 `weed/server` 等包的测试二进制在依赖 `github.com/shoenig/go-m1cpu` 的 cgo 初始化时崩溃，与改动无关，以 `CGO_ENABLED=0` 运行。

### 8.5 P5 实施记录（进行中）

**全量检查（2026-10-14，HEAD `816a90c0b`；基线 `fdbe524f3`，不含证书代码）：**

| 项 | 方式 | 结果 |
| --- | --- | --- |
| `go test`（Linux，CI 命令） | Parallels 虚拟机（Fedora 42 aarch64）装 Go 1.26.4，源码为 HEAD 加 `go mod vendor`；`go test -count=1 -p 4 -tags "elastic gocdk sqlite ydb tikv rclone" ./...`，cgo 开启（gcc 15），`-mod=vendor`、`GOPROXY=off`，在只有回环的独立网络命名空间（`unshare -n`）内以 `nice -n 19` 运行，避免测试中写死的 `localhost:9333` 等连到虚拟机上在线的 massdb-oss | 47 个包通过，90 个包无测试文件，无失败，退出码 0；虚拟机上在线服务的端口与进程不受影响 |
| `go test`（本机 macOS） | 同上标签，`CGO_ENABLED=0`（8.4 环境记录） | 46 个包通过，无失败；`weed`、`command`、`filer/tikv` 编译失败：`tikv` 标签引入的 `gosigar` 在 darwin 上需要 cgo。`command` 去掉 `tikv` 标签后通过，另两个包无测试文件 |
| `go vet` | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet -tags <CI 标签> ./...`，HEAD 与基线各跑一次 | 两边各 31 条，去掉行号后完全一致（上游既有：unkeyed fields、unreachable code、copylocks 等），证书改动无新增 |
| 构建 | `CGO_ENABLED=0 go build`（本机）；`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags <CI 标签>` | 均通过 |
| Benchmark | `go test -run '^$' -bench . -benchmem ./weed/license/` | `BenchmarkGuardAllowRead`：单核 0.59 ns/op（`-cpu 1`），18 核并行 0.053 ns/op，均 0 分配，与 P1 一致 |

**S14 新旧版本混部（2026-10-14，单 master；旧版为基线 `fdbe524f3` 构建）：**

| 组合 | 结果 |
| --- | --- |
| 旧 master + 新 volume、新 filer，无缓存 | volume POST 201，GET 403 `NOT_READY`；filer POST 201，HEAD 与 `?metadata=true` 200，GET 403 `NOT_READY`；节点不写缓存文件 |
| 新 master（5 分钟证书）+ 新 volume、新 filer + 旧 volume、旧 filer | 导入前旧 volume 收到 `PermissionDenied license: NODE_LIMIT_EXCEEDED` 并按既有循环重试；导入后 4 个节点都注册，旧节点同样占名额（volume 2/3、filer 2/2）；旧 `weed shell` 的 `volume.list`、`fs.ls` 正常 |
| 同上，证书到期后 | master `EXPIRED`；新 volume、新 filer GET 403 `EXPIRED`（新 filer 读旧 volume 上的文件也 403，由 F1 拦）；**旧 volume GET 200，旧 filer 读旧 volume 上的文件 200**，不拦截；旧 filer 读新 volume 上的块返回 500（`read chunk ... 403 Forbidden`，旧 filer 没有内部令牌，manifest 解析等维护读取同样失败）；新旧 filer 写入均 201 |
| 新节点已有缓存，master 回退到旧版 | 旧 master 不下发授权帧，新 volume 与新 filer 按缓存证书继续放行（GET 200），写入 201；换回新 master 后节点重新注册，状态恢复 |

**结论：** 与计划一致，必须同批升级：旧 volume、旧 filer 照常提供读取，而旧 filer 在证书无效时还会读不到新 volume 上的块（含内部维护读取），混部比全旧或全新都差。master 回退不会撤销已缓存的授权，放行持续到缓存证书到期（4.4）；无缓存的新节点为 `NOT_READY`。运维手册的升级步骤按“master 先升级，再同批升级全部 volume 与 filer”写。

**未覆盖：** 新旧 master 混在同一 raft 组未测（部署要求 master 先整体升级）；S14 只在单 master 上做。

**docker-compose（2026-10-14，本机 Docker Desktop，镜像为 HEAD 的 linux/arm64 静态二进制加测试根，基于 `alpine:3.22`）：** 新增 [local-license-compose.yml](../docker/compose/local-license-compose.yml)：3 个 goraft master（`-license.file` 只读挂载）、3 个 volume、filer、s3，全部状态在命名卷上；第 4 个 volume 在 `extra` profile。文件头写了首次部署步骤（`license.init` → 签发 → `up`）。

| 场景 | 结果 |
| --- | --- |
| 缺证书文件 | master 以 `license: -license.file: open /license/cert.jws: no such file or directory` 退出（255） |
| 首次部署 | `docker compose run --rm masterN license.init` 三台得到同一 `deployment_id`；签发后 `up`：3 台 `voters_acked`，3 个 volume、1 个 filer 注册，follower 账本一致；master 之间按容器主机名调用，来源校验无误拒（主机名解析路径首次在真实环境验证） |
| 读写 | S3 建桶、Put、Get，filer POST/GET，volume 直读均正常 |
| 来源校验 | 在 volume1 容器内以探针调用 master 之间的 4 个 RPC：均 `PermissionDenied license: caller 172.21.0.6 is not a master of this cluster` |
| 额度 | volume4 被拒 `NODE_LIMIT_EXCEEDED`，本地 GET 403 `NODE_REJECTED` |
| goraft 强杀 leader | 约 15 秒选出新 leader 并对账（goraft 选举超时较长），读取不中断；新 leader 以 `3/3` 拒绝 volume4（原 3 个 volume 的宽限名额保留），原 3 个 volume 约 25 秒内重新注册；旧 leader 重启后 3 台确认。补上 8.3 未覆盖的 goraft 切主准入 |
| 全集群重启（`down` 后 `up`，保留卷） | **发现缺陷**：新 leader 在 term 0 当选，写回的账本版本 `(0, 1)` 低于 follower 磁盘上的 `(1, 4)` 被拒，对账一直重试，所有 volume 与 filer 注册被延后，集群不可用。修复见 3.4“raft 任期回退”与单测 `TestManagerLedgerSurvivesTermReset`（去掉修复即以 `RECONCILING` 失败）。修复后镜像在卡死状态上直接恢复，再做一次全集群重启：term 0 对账完成，约 30 秒内全部节点注册，S3 与 filer 可读。以最终代码从全新部署复验：强杀 leader 后 29 秒、连续两次全集群重启后各 23 秒，恢复到 3 个 volume 与 filer 全部注册且 S3 可读（含 goraft 选主时间）；`state.json` 与缓存文件权限 0600、目录 0700 |
| 到期与续期 | 到期后 S3 GetObject 403 `AccessDenied`（原因头 `EXPIRED`），filer、volume GET 403，S3 Put、filer POST 与 HEAD 不受影响；在 leader 导入序号 2：`peers_failed` 为空，2 秒内读取恢复 |

部署注意：上游独立 `weed s3` 默认只绑定 localhost，容器里须加 `-ip.bind=0.0.0.0`（上游 `seaweedfs-compose.yml` 同样如此）。

**运维手册（2026-10-14）：** [license-operations-manual-20261014.md](license-operations-manual-20261014.md)，覆盖第 10 节所列流程，命令、接口、错误码与指标按代码核对。编写中发现一处可用性缺口：`GET /license` 只给出三类节点的数量，不列出宽限中的地址，`DELETE /license/nodes/{addr}` 需要运维自己找地址；master 之间的 `GetLedger` 自 8.4 起只对 master 开放。已补：`GET /license`（及 `license.show`）新增 `grace_nodes`，每项为 `type`、`address`、`last_seen`、`release_at`（`last_seen` + 10 分钟），master 页面 License 区块增加 “In grace” 行；各 master 按本地账本副本给出。单测扩展 `TestManagerAdmissionAndRelease`；真实进程中 volume 停止后 HTTP、shell 与页面都列出 `127.0.0.1:8680`，按该地址 `DELETE` 后列表清空。

**`weed server` 一体化（2026-10-14，本机，`server -filer -s3 -webdav`，非默认端口）：**

| 场景 | 结果 |
| --- | --- |
| 无证书启动 | 单 master 正常引导（goraft 选主约 18 秒，上游行为）后 `NO_LICENSE`；内置 volume GET 403 `NODE_REJECTED`，filer、S3、webdav 端口不监听 |
| 导入（4 分钟证书） | 2 秒内 filer、S3、webdav 上线；S3 读写、filer 12 MB 分块文件（`maxMB=4`）与 webdav 下载的 MD5 一致 |
| 到期 | S3 GetObject 403 `AccessDenied`（`EXPIRED`），filer GET 403，webdav 下载中断（200 后 0 字节，curl 退出码 18）；S3 与 filer 写入正常 |
| 续期 | 导入序号 2，4 秒内 S3 与 webdav 恢复 |
| 进程重启 | 从 `state.json` 恢复 `VALID`（序号 2），约 20 秒（主要是 goraft 选主）后 S3、filer、webdav 可读，数据一致 |

### 8.6 外部审核修复（2026-10-14）

审核基于 `2ec303f03`，提出 4 项，均先复现后修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | filer gRPC 的系统目录豁免按原始 `req.Directory` 判断，查询却经 `util.JoinPath` 解析了 `..`：`LookupDirectoryEntry` 以 `/etc/../data` 读 `/data/a` 时被当作 `/etc` 放行，证书无效时返回完整正文 | `licenseContentExempt` 先 `path.Clean` 目录，所有 gRPC 出口一并生效（契约 5.3） | 单测 `TestLicenseContentExempt`、`TestLicenseContentFilter` 增加 `..` 用例（去掉修复即失败）；真实进程中证书到期后，修复前 `/etc/../data`、`/etc/../../data` 返回 `secret`，修复后均隐藏 |
| 2（P1） | 写 `state.json` 失败后内存已是新状态，重试相同证书或同步被判为“未变化”而跳过写盘并确认；三个 voter 全部确认序号 2，两个 follower 重启后恢复为序号 1 | 记录上次写盘是否失败；导入、`SyncLicense`、`SyncLedger` 应答与 leader 自身计票前先重写成功，否则不确认（3.3 第 4 条） | 单测 `TestManagerAcksOnlyDurableState`：注入 follower 与 leader 写盘失败，重试期间不计入确认、导入失败，恢复后确认且重启后为新序号；在原逻辑上（只加注入钩子）第二次导入即把未落盘的 follower 计入确认 |
| 3（P1） | `newest` 相同、`prev` 不同的状态不收敛：比较与确认只看 `newest`，落后 master 以 `-license.file` 加载同一张未来证书后持有过期的 `prev`，过渡期为 `EXPIRED` 而 leader 为 `VALID`，切主后仍不一致 | `prev` 取序号更高的一张（3.2），`Certs.Compare` 据此替换，确认时比较 `prev`，对账合并与采纳 follower 状态同样处理，节点按同一规则采纳 | 单测 `TestManagerPrevConverges`（落后 follower 收到推送后采用更好的 `prev` 并确认，过渡期 `VALID`；持较差 `prev` 的 leader 对账后采用更好的一张；去掉修复即超时失败）、`TestNodeAdoptsBetterPrev` |
| 4（P2） | 续期后 mount 重取隐藏正文条目时整体替换句柄里的条目，覆盖了证书无效期间在打开的句柄上做、尚未 flush 的 `chmod`/`chown` | 句柄有未提交修改时只取 filer 的正文并清除隐藏标志，其余字段保留（1.6） | 单测 `TestTakeRefetchedEntry`；Linux 虚拟机真实 FUSE：到期后挂载、打开内联文件（读 `EIO`）、`fchmod 0600` 与 `fchown 2000`、续期后同一句柄读到 `secret`；修复前关闭时句柄回到 `0660/uid 0` 并写回 filer，修复后 filer 存为 `0600/uid 2000/gid 2000` |

第 2、3 项只在假 raft 集群单测中复现与验证（真实进程需要按 master 注入磁盘写失败、构造落后 master）。修复后 license（含 `-race`）、server、filer、mount、wdclient、shell、command 测试与 CI 标签构建通过。审核同时指出 master 新地址准入扫描全账本、快照复制与排序持锁，集中重连、拒绝态大目录与元数据回放仍是 S13 的重点。

### 8.7 S13 性能（2026-10-14）

**环境：** 单机 Apple M5 Pro（6 性能核 + 12 能效核）、48 GB、macOS；压测程序与全部服务进程同机。A 为基线 `fdbe524f3`（不含证书代码），B 为 `7b7bca195` 加测试根，均 `CGO_ENABLED=0` 构建，命令行与数据目录相同。测试期间关闭其它应用，Parallels 虚拟机保持运行（空闲，约 0.2 核），`caffeinate` 防休眠、允许熄屏。

**方法与计划的差异：**

1. 每个负载 A、B 各 3 个窗口，按 A B B A A B 排列以抵消数据增长；每窗重启全部进程，预热 10 秒、计时 60 秒（计划为 30 + 120 秒）。试跑测得同版本吞吐波动 ≤ 1.2%、p99 ≤ 3%，60 秒足够。
2. 噪声带取 A 三窗与 B 三窗各自的（最大 − 最小）/ 均值中较大者；A/B 差异为两组均值之差，落在噪声带内即通过。
3. 采集吞吐、p50/p99/p999、全部服务进程 CPU 合计（`ps`）、分配速率与 GC（各进程 `/metrics` 的 `go_memstats_alloc_bytes_total`、`go_gc_duration_seconds_count`）、RSS；集群：1 master + 1 volume + filer（`-saveToFilerLimit=1024 -maxMB=4`）+ S3；`-cipher` 组的 filer 加 `-encryptVolumeData`。
4. 元数据回放为从 0 订阅 `/replay` 下 100 万条创建事件、计到最后一条的用时（读持久化日志，经内部令牌），不是重启 filer 追平。
5. 拒读态在主集群数据的副本上导入不含 `DATA_READ` 的证书（`FEATURE_MISSING`），与 `EXPIRED` 走同一拒读路径；只跑 B，与 B 有效态对比。
6. “200 个 volume 集中重连”用心跳模拟器：200 条 `SendHeartbeat` 流同时首次注册，保持 15 秒后同时断开、再同时重连，记录到首个响应的延迟；“一个 voter 不可达、另一个 `SyncLedger` 延迟 2 秒”在真实进程中注入需改代码，改为 license 包单测 `TestManagerAdmissionNotBlockedBySlowPeer`（假 raft 集群，200 个并发准入）。
7. “filer 重连”由 3 master 集群强杀 leader 覆盖（filer 与 volume 都要向新 leader 重新注册）；续期为 B 窗口内 S3 读负载进行到一半时导入新证书。

**结果（有效态，A/B）：**

| 负载 | A 吞吐 | B 吞吐 | 吞吐差异（噪声） | p99 差异（噪声） | CPU 差异 | 分配差异 |
| --- | --- | --- | --- | --- | --- | --- |
| volume 直读 4 KB | 75,950 次/秒 | 76,711 | +1.0%（3.4%） | p99 -5.9%（18.1%） | +0.5% | -0.5% |
| filer 读 4 KB | 37,777 次/秒 | 37,833 | +0.1%（0.7%） | p99 -1.6%（4.9%） | -0.0% | -0.4% |
| filer 读 32 MB（8 块） | 114 次/秒 | 117 | +2.6%（6.2%） | p99 -0.7%（2.0%） | -2.3% | +1.5% |
| S3 GetObject 4 KB | 24,609 次/秒 | 24,566 | -0.2%（0.7%） | p99 -0.4%（2.9%） | +0.1% | -0.4% |
| 内联文件 `?metadata=true` | 95,704 次/秒 | 95,302 | -0.4%（0.6%） | p99 +0.6%（1.0%） | +0.6% | +0.1% |
| 大目录 HTML 分页（1000 条） | 358 次/秒 | 360 | +0.4%（1.2%） | p99 -0.3%（2.0%） | -0.0% | +1.1% |
| 大目录 JSON 分页（1000 条） | 5,435 次/秒 | 5,397 | -0.7%（0.7%） | p99 +0.6%（2.4%） | -0.0% | +1.4% |
| manifest 文件 `UpdateEntry` | 1,503 次/秒 | 1,528 | +1.6%（1.2%） | p99 -0.9%（0.9%） | -0.9% | +1.6% |
| 追加触发小块合并 | 5,124 次/秒 | 4,932 | -3.7%（10.7%） | p99 -0.2%（18.1%） | -1.9% | -0.3% |
| filer PUT 1 KB | 29,882 次/秒 | 29,238 | -2.2%（2.4%） | p99 +6.7%（8.7%） | -1.2% | +3.4% |
| S3 PUT 1 KB | 18,157 次/秒 | 18,307 | +0.8%（1.6%） | p99 -0.6%（2.5%） | +2.1% | -15.5% |
| S3 DELETE | 13,048 次/秒 | 13,002 | -0.4%（0.6%） | p99 +0.6%（0.4%） | +0.3% | -1.2% |
| 100 万条元数据回放 | 776,729 事件/秒 | 772,694 | -0.5%（2.2%） | 用时 +0.5%（2.2%） | +0.6% | -4.7% |
| manifest 文件 `UpdateEntry`，`-cipher` | 1,448 次/秒 | 1,427 | -1.4%（2.9%） | p99 +3.2%（7.4%） | +4.0% | +3.5% |
| 追加触发小块合并，`-cipher` | 2,253 次/秒 | 2,253 | +0.0%（0.2%） | p99 +2.3%（11.8%） | +0.1% | +0.0% |

84 个比较项中 9 个超出噪声带，均为噪声带极窄（0.4%–3%）时的小幅差异：manifest 更新吞吐 +1.6%、S3 PUT 分配 −15.5%、回放分配 −4.7% 为 B 更好；其余为 CPU 或分配 +0.6%–+3.4%，对应负载的吞吐与 p99 均在噪声内。大目录 JSON 每次请求多分配约 2%（吞吐与 CPU 无差别），已核对：有效态下隐藏检查对每个条目只做一次原子读、不分配，返回的 JSON 字节数相同，差异来自 P3 为 `filer.Entry` 增加 `ContentHidden` 字段后条目对象略大。**结论：未发现证书功能带来的退化。**

**续期：** 3 次在 S3 读负载中途导入续期证书，0 错误，窗口吞吐 23,300 次/秒、p99 2.02 ms（该窗口位于写入负载之后，与窗口开头的 S3 读不同条件，只作无错误的证据）。

**拒读态（B，3 窗）：** volume 拒读 91,560 次/秒、filer 96,216、S3 45,590（拒绝不读数据，均快于有效态读取）；内联元数据 96,006 次/秒，与有效态相同；大目录 JSON 需逐条隐藏正文，吞吐 5,467 次/秒不降，每次请求分配多约 16%、CPU 多约 8%。拒读日志：volume 每 6 分钟 2 条、filer 4 条、S3 0 条（S3 的拒绝在 filer 记录），满足每节点每分钟至多 1 条。

**3 master（hashicorp）：**

| 项 | A | B |
| --- | --- | --- |
| 强杀 leader 后 volume 与 filer 重新注册 | 8.1、8.0、8.2 秒 | 10.1、8.2、5.1 秒 |
| 切主期间 filer 读取 | 0 错误，p99 1.13 ms | 0 错误，p99 1.12 ms |
| 200 节点首次注册 p99 | 13.0、10.0、13.5 ms | 13.4、11.3、11.4 ms |
| 200 节点集中重连 p99 | 19.5、18.4、17.1 ms | 15.7、19.9、26.1 ms |
| 每轮 `state.json` 写盘 | — | 3 次 |
| leader CPU / 分配 | 0.03 核 / 1.0 MB/s | 0.03 核 / 1.0–1.1 MB/s |

慢 voter 单测：一个 voter 不可达、另一个 `SyncLedger` 延迟 2 秒、一个账本批次正在等待时，200 个并发准入 p99 5.9 ms（开 `-race`），准入不等待复制。

**事故与上游观察：** 追加负载使磁盘写满。每次 `?op=append` 的元数据事件都带整个文件新旧两份块列表；文件有上千块、每秒约 5000 次追加时，元数据日志每分钟增长数十 GB，主集群 6 窗累计约 166 GB（A、B 相同，上游行为），之后 volume 低于默认 1% 保留空间转为只读，首轮 `-cipher` 组的追加全部失败、结果作废，系统盘一度只剩 0.5 GB。已删除测试数据卷恢复空间，`-cipher` 组改为 8 个写入者、每窗先删除追加文件后重跑（每窗开始前检查磁盘余量 ≥ 30 GB），重跑 0 错误，仍累计约 93 GB（加密块带 32 字节密钥，事件更大）。主集群在空间耗尽前跑完，拒读与 3 master 两组只读，结果有效。对运维的含义：对多块文件的频繁小追加会让元数据日志快速膨胀，与证书无关。

**未覆盖：** Linux 物理机与独立压测机；计划的 30 + 120 秒窗口；真实 200 个 volume 进程；真实进程中的慢 voter 注入。原始结果与脚本在会话临时目录，未入库。

### 8.8 外部复审修复（2026-10-15）

复审基于 `9cab640dc`，提出 5 项（2 项 P1、3 项 P2），均先复现后修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | 8.6 第 1 项只规范化了目录：`LookupDirectoryEntry` 以目录 `/etc`、名字 `../data/a` 查询时实际读取 `/data/a`，仍按 `/etc` 豁免，证书无效时返回正文 | 豁免按“目录 + 名字”的完整路径规范化后判断；`LookupDirectoryEntry` 与 `CacheRemoteObjectToLocalCluster` 改用实际查询路径的父目录（契约 5.3） | 单测增加名字含 `..` 的用例；真实进程证书到期后，修复前目录 `/etc`、名字 `../data/a` 返回 `secret`，修复后隐藏 |
| 2（P1） | 8.6 第 2 项只记录已发生的写盘失败：第一次同步更新内存、写盘尚未完成时到达的相同请求被判为“未变化”直接确认，`SyncLedger` 同样 | 改为记录磁盘上状态对应的授权修订号、账本版本与时钟修复序号，确认前要求磁盘追上内存，否则等正在进行的写盘后再写一次（3.3 第 4 条） | 单测 `TestManagerDuplicateSyncWaitsForDisk`：follower 第一次写盘挂起期间，重复的 `SyncLicense` 与 `SyncLedger` 不返回，写完后才返回，重启后为新状态；在修复前代码上重复请求立即返回成功 |
| 3（P2） | `adoptAhead` 采纳其他 master 的较新证书时没有合并对方的时钟修复，leader 仍为 `CLOCK_SUSPECT` 并下发拒读；重复导入该证书被判为“未变化”，也无法修复 | 采纳时按三分支规则合并时钟；重复导入同一证书时若未应用其修复则应用（4.5） | 单测 `TestManagerAdoptsClockRepair` 两个子用例，修复前均为 `CLOCK_SUSPECT` |
| 4（P2） | mount 续期恢复时，打开的句柄上未提交的截断（5 字节截到 3 字节）保留了大小却取回完整正文 | 取回的正文按句柄上的文件大小截断（1.6） | 单测；Linux 虚拟机真实 FUSE：修复前关闭后 filer 存为大小 3、正文 `hello`，修复后正文 `hel` |
| 5（P2） | mount 续期恢复且句柄无本地修改时，直接装入 filer 返回的 uid/gid，绕过 `-map.uid/-map.gid` 映射 | 装入句柄前映射为本地 id，meta cache 仍存 filer 的 id（1.6） | 单测 `TestLocalEntryFromFiler`；虚拟机真实 FUSE（映射 2000:1000）：修复前续期后句柄属主变为 1000，修复后保持 2000 |

修复后 license（`-race`）、server、filer、mount、wdclient、shell、command、s3api、pb 测试与 CI 标签构建通过。复审对性能的意见（S13 为同机短窗口、部分场景用模拟器与假集群、拒读态大目录 JSON 分配约 +16%、CPU 约 +8%）与 8.7 的“未覆盖”一致：S13 不等同于长期、Linux 生产环境验收。

### 8.9 外部第三轮审核修复（2026-10-15）

第三轮审核基于 `77027e59a`。前两轮 5 项的原复现用例均已通过；本轮新提出 3 项（2 项 P1、1 项 P2），先复现、再修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | 移除 master（`RemoveMaster`）时 leader 直接把自己计为已确认，漏了 8.8 第 2 项的磁盘检查。leader 写盘失败、账本只由 B、C 确认后，仍能依次移除 B、C；leader 重启后账本回退，节点的宽限记录丢失，新地址可以占用它的额度 | leader 计入剩余 voter 多数派之前，先确认 `state.json` 已持有当前状态，失败则计入失败列表（3.3 第 8 条） | 单测 `TestManagerRemoveMasterWaitsForLeaderDisk`：leader 写盘失败时，移除 B 返回 `PARTIAL_COMMIT`，失败列表含 leader；恢复后依次移除 B、C，leader 重启后账本仍含该节点。修复前的代码上移除直接成功 |
| 2（P1） | mount `copy_file_range` 清空了内联正文，但没有像写入那样清除隐藏标志和对应读错误。拒读期间，目标句柄读到 `EIO`；续期后重取又把旧正文装回句柄，而读取优先返回内联正文；关闭后 filer 上旧正文与新块并存，读出的是旧正文 | 与写入相同，清除隐藏标志与对应读错误（1.6） | 单测 `TestCopyFileRangeClearsHiddenContent`。Linux 虚拟机真实 FUSE：把新文件中的 `world` 复制到隐藏的 `hello` 上，修复前拒读期间读到 `EIO`，关闭后 filer 返回 `hello`（复制的数据丢失）；修复后读到 `world`，filer 存为 `world` |
| 3（P2） | 句柄上先截短再扩长（5→3→5）：8.8 第 4 项只按最终大小裁剪，续期后取回 `hello` 而不是 `hel\0\0`。若在续期前关闭，filer 见大小未变，按纯元数据更新补回完整正文，关闭却返回成功 | 句柄记录隐藏期间截到过的最小非 0 长度。续期重取时从该长度起补零；仍为隐藏且该长度小于当前大小时，flush 返回 `EIO`（`CONTENT_HIDDEN`），不发送（1.6，契约 5.3） | 单测 `TestTruncateHiddenContent`（经 `SetAttr` 与 `doFlush`）。Linux 虚拟机真实 FUSE，修复前：续期后读到 `hello`，filer 存 `hello`；续期前关闭返回成功，filer 仍为 `hello`。修复后：读到 `hel\0\0`，filer 存 `hel\0\0`；续期前关闭返回 `EIO`，filer 不变 |

三个新单测在修复前的代码上都失败。修复后通过的检查：license 测试（含 `-race`）、server、filer、command、mount（含 `page_writer`；新单测另跑 `-race`）、shell、s3api、wdclient、pb 测试，以及 CI 标签的 Linux 构建。`go vet ./license/ ./mount/` 只报上游已有的 `inode_to_path.go` 一条。

复审还指出 S13（8.7）测的是 `7b7bca195`，不能直接证明当前提交无性能回归。此后的代码改动都不在读守卫路径上：
- master 只在确认路径上多比较三个版本号；
- filer 的 `LookupDirectoryEntry` 与 `CacheRemoteObjectToLocalCluster` 每次多一次路径拼接，本机基准约 60 ns、48 B、1 次分配，相对一次元数据查询可以忽略；
- mount 在句柄装入条目与截断隐藏正文时多一次原子操作，flush 时多一次条目读锁与字段判断。

因此未重跑 S13。拒读态大目录 JSON 的分配与 CPU 增量见 8.7，仍属已知代价。

### 8.10 外部第四轮审核修复（2026-10-15）

第四轮审核基于 `77027e59a` 加 8.9 的修复（当时尚未提交）。8.9 的 3 项原复现用例均已通过；本轮新提出 2 项（1 项 P1、1 项 P2），先复现、再修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | mount 重取隐藏正文时在锁外等待 filer 应答，期间 `SetAttr` 可以截断到 0、清除隐藏状态。8.9 的修复只覆盖串行情况，应答回来后仍照常处理：取回 `hello` 时，截断到 0 再扩到 5 字节的文件又变回 `hello`；应答仍为隐藏时，又设回已被截断清除的读错误，之后完整写入 `world` 成功，读取却返回 `EIO` | 重取以发起时的读失败状态作为句柄状态的标记：截断到 0（`clearHiddenContent`）与装入新条目（`SetEntry`）都会替换它。应答回来时，在条目锁内核对标记未变且条目仍为隐藏，才装入正文；结果用比较并交换写回，标记已变就丢弃。重解析 manifest 的重试同样处理（1.6） | 单测 `TestRefetchDroppedAfterTruncate`：替换 filer 查询，在应答前截断到 0 再扩到 5，分别返回正文与隐藏；读到 5 个零字节、无读错误，写入 `world` 后读到 `world`。两种应答在修复前的逻辑上分别读到 `hello` 和 `EIO`。`TestTakeRefetchedEntry` 增加标记已变的用例。Linux 虚拟机重跑 8.9 的真实 FUSE 场景，结果不变 |
| 2（P2） | `writeFileAtomic` 忽略打开父目录与目录 `Sync` 的错误。master 的 `state.json` 也用它，目录同步失败时上层仍推进磁盘版本并计入确认，掉电后 rename 可能丢失 | 打开或同步目录失败时返回错误，上层按写盘失败处理（3.3 第 4 条）；Windows 不同步目录，`EINVAL`、`ENOTSUP` 视为文件系统不支持而跳过。volume/filer 的 `license-cache.json` 用同一函数，失败只记 warning（缓存语义不变） | 单测 `TestManagerImportNeedsDirectorySync`：状态目录设为 0300（可创建、可 rename、不可打开），导入返回 `sync directory` 错误，恢复权限后同一证书导入成功；修复前导入成功。root 运行时跳过（root 能打开该目录），本机与 Linux 虚拟机（以 nobody 运行）均通过。linux/darwin/windows/freebsd 编译通过 |

修复后通过的检查：
- 本机：license 测试（含 `-race`）、server、filer、command、mount（含 `page_writer`；mount 新单测另跑 3 次 `-race`）、shell、s3api、wdclient、pb 测试，以及 CI 标签的 Linux 构建。`go vet ./license/ ./mount/` 只报上游已有的一条。
- Linux 虚拟机：license、mount（含 `page_writer`）、server、filer 测试通过，测试临时目录放在磁盘上。`-race` 在虚拟机上链接失败（race 运行时缺 `clock_gettime` 符号），属工具链环境问题，未在虚拟机上跑。

性能：正常读取路径未改动；重取结果多一次比较并交换。master 每次写 `state.json` 多一次目录同步（`fsync`），只在导入、同步、账本批次等确认路径上，不在读路径。未重跑 S13，也未做断电验收。

### 8.11 外部第五轮审核修复（2026-10-15）

第五轮审核基于 `3e1273c66`。8.10 的 2 项原复现均已通过；本轮新提出 1 项 P1，先复现、再修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | mount 的重试方式由最近一次错误决定。重取隐藏正文时查询 filer 失败（如 `Unavailable`），`ErrContentHidden` 被该错误替换，下一次重试就改走 manifest 重解析；内联文件没有 chunks，解析直接成功并清除读错误，正文却仍隐藏，读取与 `copy_file_range` 都成功返回全零。同一原因还使查询失败后截断到 0 再写入时，读错误没有被清除，暂时返回 `EIO` | 读失败状态单独记录“正文隐藏”：重试方式与写入、截断到 0 时是否清除，都按它判断；查询失败只替换错误，保留该标记（1.6） | 单测 `TestRefetchKeepsHiddenAfterLookupError`：第一次查询返回错误；5 秒间隔后重试再次查询 filer，filer 已恢复正文时读到 `hello`，仍隐藏时读失败，不再返回全零；查询失败后截断到 0 并写入 `world`，立即读到 `world`。修复前的逻辑上，第二次读取不查询 filer（共 1 次查询） |

修复后，mount（含 `page_writer`；mount 相关单测另跑 3 次 `-race`）、license、server、command 测试与 CI 标签 Linux 构建通过，`go vet ./mount/` 只报上游已有的一条。新单测经 `readDataByFileHandle` 走完整重试路径，查询 filer 的函数在测试中被替换。Linux 虚拟机真实 FUSE（证书到期后挂载，隐藏的内联文件 `hello`；在重取进行中停掉 filer，再重启，最后续期），对比 `3e1273c66` 与修复版：

| 步骤 | 修复前 | 修复后 |
| --- | --- | --- |
| filer 停止期间读取（查询失败，约 26 秒后返回） | `EIO` | `EIO` |
| filer 停止期间截断到 0、写入 `world` 后读取 | `EIO` | `world` |
| filer 恢复、证书仍过期，5 秒后读取 | 成功返回 5 个零字节 | `EIO` |
| 续期后同一句柄读取 | 仍为 5 个零字节 | `hello` |

两个版本里 filer 停止期间写入的 `world` 关闭时都返回 `EIO`、未写入 filer：mount 写入后即后台上传，向 filer 申请 volume 的重试在 filer 恢复前约 0.15 秒放弃。这是上游 mount 在 filer 不可用时写入的行为，与本项无关。正常读取路径未改动，未重跑 S13。

### 8.12 外部第六轮审核修复（2026-10-15）

第六轮审核基于 `39a9ff360`。8.11 的原复现均已通过；本轮新提出 1 项 P1，先复现、再修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | 句柄有未提交修改（如 chmod）时，重取只取 filer 的内联字节，分块与大小沿用句柄上隐藏时的旧值，分块组也按旧的空分块重建。其间别的客户端把文件改写为分块存储时，读取成功但返回全零；flush 又以“大小 5、无分块、无正文、未隐藏”覆盖 filer，丢掉别人写入的分块引用。同一原因下，别的客户端写入更长的内联内容（如 6 字节）时，会被裁到旧大小 | 句柄记录隐藏时的大小。句柄没有改过大小时，内联字节、分块、大小与 MD5 一起从 filer 取回，分块组按 filer 的分块重建，chmod 等其余本地修改保留；改过大小时只能裁剪内联字节，filer 已改为分块存储则不合并，保持隐藏与读失败，flush 由 filer 以 `CONTENT_HIDDEN` 拒绝（1.6） | 单测 `TestTakeRefetchedEntry` 增加分块改写、更长内联改写、本地截断后遇到分块三种情况；`TestRefetchTakesChunksWithLocalChmod` 经 `SetAttr` 与 `maybeRetryRead` 走完整路径，句柄取得 filer 的分块与大小，分块组含该分块。两个测试在修复前的逻辑上都失败，句柄保持“大小 5、无分块” |

真实 FUSE 的触发条件是一个时间窗。上游 mount 收到元数据订阅事件后，会用新条目替换已打开句柄的条目，丢弃其未提交修改。只有在 filer 已被改写、事件尚未到达时发生重取，才会进入本项路径。经 HTTP 写入硬链接的另一个名字会断开链接，也无法借此稳定触发。

Linux 虚拟机上用真实 FUSE 跑了以下场景：
- 常见路径（证书过期时挂载隐藏的 `hello`，句柄上 chmod，另一客户端经 HTTP 写入 2000 字节分块内容，续期后读取，关闭）：`39a9ff360` 与修复版结果相同。读到 2000 字节，filer 保留 1 个分块与全部内容；订阅事件按上游行为替换了句柄条目，句柄上的 chmod 未生效。
- 8.9 与 8.11 的场景，用修复版重跑，结果与此前修复版一致。

修复后 mount（含 `page_writer`；mount 相关单测另跑 3 次 `-race`）、license、server、command 测试与 CI 标签 Linux 构建通过，`go vet ./mount/` 只报上游已有的一条。正常读取路径未改动，未重跑 S13。

### 8.13 外部第七轮审核修复（2026-10-15）

第七轮审核基于 `3d9b55b1d`。8.12 的分块场景已通过；本轮新提出 2 项 P1，都出在同一个时间窗：正文被隐藏的内联文件后来变为仅远端存储的对象（`RemoteEntry`，无正文、无分块），而客户端仍持有旧条目。先复现、再修复：

| # | 问题 | 修复 | 验证 |
| --- | --- | --- | --- |
| 1（P1） | mount 重取时，句柄有未提交修改（如 chmod）则只取内联字节、分块、大小与 MD5，漏了远端对象描述。远端对象的正文与分块都为空，读取因此不触发远端取数（`IsInRemoteOnly` 为假），直接成功返回全零；之后 flush 提交的条目也不带远端引用。句柄改过大小时同样如此 | 远端对象描述与内联字节、分块、大小、MD5 一起取回；句柄改过大小而 filer 已是远端对象时，与分块一样不合并，保持隐藏（1.6） | 单测 `TestTakeRefetchedEntry` 增加两种情况：带 chmod 的远端对象改写（取回后为仅远端存储，本地 chmod 保留），以及本地截断后遇到远端对象（保持隐藏） |
| 2（P1） | filer 的写入合并只比较大小与分块。客户端带着旧的隐藏条目做纯元数据更新时，若已有条目是仅远端存储（大小相同、双方都无分块），就判为可合并，只复制已有的（空）正文，落盘条目丢掉远端引用。这条不依赖 mount 的读取路径 | 判定纯元数据更新时同时要求远端对象描述与已有条目相同；不同则走已有的拒绝分支，返回 `CONTENT_HIDDEN`（契约 5.3 写入合并规则第三行） | 单测 `TestMergeHiddenContent` 增加远端对象的两种情况；`TestLicenseMergeHiddenRemote` 用 LevelDB 与真实 filer：存入仅远端存储的条目，不带远端描述的隐藏更新被拒，带相同描述的更新落盘后保留远端引用与新的权限位 |

三个新单测在修复前的逻辑上都失败。修复后通过的检查：server、filer（含各存储子包）、mount（含 `page_writer`；mount 相关单测另跑 3 次 `-race`）、license、s3api、command、shell、wdclient、pb 测试，以及 CI 标签的 Linux 构建。`go vet ./mount/` 只报上游已有的一条。

关于第 2 项的影响面：
- S3 打标签、mount chmod 等回写的是读到的条目，远端描述与已有条目相同，照常合并。
- `filer.remote.sync` 上传后回写远端描述，用的是事件里的条目；它遇到隐藏事件本就停下（契约 8.2 S20），不受影响。
- `remote.mount` 与 `remote.meta.sync` 新建条目时不带隐藏标志，不受影响。
- 例外：远端版本有更新时，这两个命令会把经 gRPC 读到的已有条目改为新的远端描述、清空正文后写回（`doSaveRemoteEntry`）。已有条目是被隐藏的内联正文时，写回带着隐藏标志：
  - 修复前：大小不同则被拒；大小相同则被当作纯元数据更新，旧内联正文与新远端描述并存，本该清空的旧正文被保留。
  - 修复后：一律返回 `CONTENT_HIDDEN`，该条目留待证书恢复后再同步。

本轮未做真实 FUSE 验证：触发需要远端存储，且只发生在时间窗内（见 8.12）。正常读取路径未改动，未重跑 S13。

### 8.14 外部第八、九轮审核修复（2026-10-15）

第八轮审核基于 `9b9da1949`，第九轮审核基于 `9b9da1949` 加第八轮未提交的修复。8.13 的两项原复现均已通过。两轮共提出 4 项（3 项 P1、1 项 P2），都在上游 mount 处理仅远端存储文件（`RemoteEntry`，本地无正文、无分块）的路径上，并非证书改动引入；但证书恢复后，重取得到的远端对象会走这条路径，因此一并修复。

| # | 轮次 | 问题 |
| --- | --- | --- |
| 1（P1） | 八 | 缓存远端对象（`CacheRemoteObjectToLocalCluster`）成功后，只替换了句柄条目，没有替换读取实际使用的分块组，读取成功却返回全零。条目被整体替换，句柄上未提交的 chmod 被覆盖并随 flush 提交。filer 返回空条目时，空条目被装入句柄 |
| 2（P1） | 九 | 句柄先写入、再首次读取时，缓存分块带着下载时的时间戳，比此前写入的数据新；flush 合并重叠分块时淘汰了写入，filer 上只剩旧正文 |
| 3（P1） | 九 | filer 缓存时会发出一条更新事件，上游失效处理（`invalidateFunc`）收到后整体替换句柄条目，并丢弃句柄上尚未写出的数据。第八轮只在缓存时已有未提交修改才识别这条自身事件，先读后写或 chmod 的修改仍被还原。两个并发的首次读取会发出两次缓存请求，第二条事件同样覆盖本地修改 |
| 4（P2） | 九 | 截短再扩长（5→3→5）恢复了截掉的字节；截断到 0 再扩长（5→0→5）恢复了原正文：截断后文件仍是仅远端存储，下次读取又从远端取回 |

修复（第九轮把第八轮“缓存后与本地修改合并”的做法改为“修改前先缓存”）：
- **修改前先缓存**：写入、`copy_file_range` 的目标、截断或扩长到非 0 之前，先把仅远端存储的文件缓存到集群（`cacheRemoteBeforeChange`；没有打开句柄的截断用 `cacheRemoteEntry` 缓存到要保存的条目里）；缓存失败返回 `EIO`。此后修改与普通分块文件相同：写入的数据比缓存分块新，截断裁剪缓存分块。写入的时间戳改在缓存之后取。
- **截断到 0 去掉远端描述**：文件成为本地空文件，不再按仅远端存储取回；`filer.remote.sync` 若在运行，会把空文件上传。
- **读取时缓存**：缓存请求在读锁外进行，同一句柄一次只运行一个，已缓存则不再请求。结果在写锁内与新的分块组一起装入（`takeCachedEntry`）：无未提交修改时整体替换；有未提交修改时（此时只可能是 chmod 等属性修改），只取正文、分块、远端描述、大小与 MD5，其余保留本地。filer 返回空条目时报错，不装入；处于读失败状态的句柄不触发缓存。
- **自身事件**：每次装入都记下这次缓存的分块与远端描述。失效处理先从 meta cache 取新条目（更新事件传入的是旧条目），与记录一致即认作句柄自己的缓存事件，只认一次，不替换句柄、不丢弃待写数据；其他事件照旧按上游处理。读写在缓存期间持有句柄锁，事件总在装入之后处理。**无句柄的缓存（2026-10-25 修订，8.22 方案 11）：** `cacheRemoteEntry` 在向 filer 发请求前按路径记下远端描述，meta cache 应用更新事件前先问 mount（`ownMetaEvent`）：路径无打开句柄、事件是"无分块 → 有分块且远端描述相同（忽略 `last_local_sync_ts_ns`）"即认作这次下载的事件，整条忽略并消费标记；否则照旧。原因：无句柄的截断在缓存后立即 `saveEntry`，其自身事件按签名被跳过，而 filer 的下载事件带回全尺寸条目，meta cache 会停在旧大小且没有后续事件纠正。标记一分钟过期（filer 已缓存过的文件不发事件）；有句柄的文件仍由 `invalidateHandle` 处理，保持与下载装入的顺序。

单测 `TestDownloadRemoteEntry`：替换缓存请求，预置分块缓存，覆盖以下情况：
- 并发两次首次读取只请求一次，读到 `world`；
- 先读后写入 `newer` 并 chmod，自身事件只认一次，之后仍读到 `newer`、权限 0600；
- 先写入再读：写入前已缓存，缓存分块早于写入，读到 `newer`；
- 先 chmod 再读：读到 `world`、权限 0600；
- 5→3→5 读到 `wor\0\0`；5→0→5 不请求，读到 5 个零字节，远端描述已去掉；
- 无句柄时的缓存，以及 filer 返回空条目时读取报错。

逐一去掉串行下载、修改前缓存、截断到 0 去掉远端描述、对未修改句柄的事件记录这四处，测试分别失败。

Linux 虚拟机真实 FUSE 验证：另起本测试集群的 S3 网关作为远端存储，用 `remote.configure` 与 `remote.mount` 挂载桶，6 个仅远端存储的文件内容都是 `world`。

| 文件与操作 | `9b9da1949` | 修复后 |
| --- | --- | --- |
| a：句柄上 chmod 0600 后读取；关闭后 filer | 5 个零字节，权限 0644；filer 权限 644 | `world`，0600；filer 权限 600 |
| b：直接读取 | 5 个零字节 | `world` |
| c：不先读，写入 `newer`，关闭后 filer | `newer` | `newer` |
| d：先读，写入 `newer`、chmod 0600，3 秒后读取；关闭后 filer | 首读 5 个零字节，后读 `newer`、0600；filer 为 `world`（写入丢失）、600 | 首读 `world`，后读 `newer`、0600；filer 为 `newer`、600 |
| e：5→3→5 后读取；关闭后 filer | 5 个零字节；filer 为 `world`（截断丢失） | `wor\0\0`；filer 元数据为大小 5、一个 3 字节分块，经 filer 读取返回空正文（见下） |
| f：5→0→5 后读取；关闭后 filer | 5 个零字节；filer 为 `world` | 5 个零字节；filer 为 5 个零字节 |

e 的 filer 读取是上游另一个问题：普通分块文件经 mount 截断（分块大小被就地缩小）后再扩长，filer 读取返回空正文。不含证书代码的基线 `fdbe524f3` 对普通文件同样如此，与远端存储和本节修改无关，单独处理。8.12 的分块改写场景用修复版重跑，结果不变。

修复后 mount（含 `page_writer`；mount 相关单测另跑 3 次 `-race`）、server、filer、license、s3api、command、shell 测试与 CI 标签 Linux 构建通过，`go vet ./mount/` 只报上游已有的一条。正常读取路径的“是否仅远端存储”判断原本就在读锁内，写入多一次同样的判断，未重跑 S13。

### 8.15 外部第十轮审核修复（2026-10-15）

第十轮审核基于 `1698cc663`。8.14 三项的常规复现通过；本轮提出 3 项（2 项 P1、1 项既有 P2），前两项仍在仅远端存储文件的路径上，第三项是上游读取器的并发问题。

| # | 问题 |
| --- | --- |
| 1（P1） | 写入的时间戳在缓存之后取 mount 本机时间，而缓存分块的时间戳由 filer 的时钟打上：filer 时钟领先时（复现用 5 秒）写入仍比缓存分块旧，flush 合并重叠分块时丢掉写入，filer 上只剩旧正文。flush 前的读取不受影响：脏页覆盖读取结果时不看时间戳 |
| 2（P1） | `SetAttr` 只持条目锁，没有取 Read、Write 与失效回调共用的句柄锁。chmod 后截短时，filer 的缓存事件若先于缓存 RPC 的应答到达，失效回调整体替换句柄，chmod 丢失，缓存记录也没能建立；首次读取的下载进行中截断到 0，截断先于装入完成，装入时条目已非仅远端存储而不记录，随后自身事件又把文件恢复为 5 字节 |
| 3（P2，既有） | `ChunkReadAt.lastChunkFid` 被同一文件的并发读取不同步地读写，`-race` 报告数据竞争；license 之前即如此，未见内容损坏 |

修复：
- **写入时间戳单调且晚于条目分块**（`FileHandle.writeTsNs`）：`Write` 与 `copy_file_range` 的时间戳取本机时间、句柄上一次写入时间加 1、条目全部分块（取条目分块与分块组两处，分块组内 manifest 已解析）的最大 `ModifiedTsNs` 加 1 三者的最大值，并记为句柄的上一次写入时间。覆盖顺序因此不依赖 mount 与 filer 的时钟一致：缓存的远端对象、续期重取的条目、其他客户端写入的分块（上游本就按时间戳合并，同样受时钟偏差影响）都被之后的写入覆盖。被推到“未来”的时间戳只用于合并顺序，mtime 不受影响。每次写入多一次分块遍历（2 MB 分块的 1 GB 文件约 500 次比较）。
- **`SetAttr` 取句柄排他锁**：有句柄时与 Write 相同，在缓存、修改条目之前取得句柄排他锁并重新取条目。读取的下载在共享锁内完成后截断才进行，裁剪的是缓存分块；截断自己发起的缓存，其事件在 `SetAttr` 返回后处理，按记录识别为自身事件。失效回调抽成 `WFS.invalidateHandle`，测试可直接调用。
- **`lastChunkFid` 加锁**：读取与更新在互斥锁内交换，语义不变。

单测：
- `TestWriteNewerThanFilerChunks`：filer 桩（分配文件 id、记录 `CreateEntry`）与上传桩，缓存分块的时间戳领先 5 秒。写入 `newer` 后 flush，filer 收到的条目只有写入的分块、时间戳等于句柄记录的写入时间，之后读到 `newer`；第二次写入与 `copy_file_range` 的时间戳依次递增。
- `TestSetAttrOrderedWithDownload`：chmod 后截短，缓存桩在应答前从另一协程投递自身事件：事件等到截断完成，结果 `wor`、0600、大小 3、记录已消费。读取的下载进行中截断到 0：截断在下载完成前不返回，读到 `world`，截断后大小 0、无远端描述与分块，自身事件不改变句柄。
- `TestReaderAtConcurrent`：8 个协程并发读取同一个 `ChunkReadAt`。

去掉对应修复，三个测试分别失败（第三个在 `-race` 下报告竞争）。审核方的独立复现中，时钟超前与 32 协程并发读取两个用例，以及第九轮的三个用例，在修复后通过；其 `SetAttr` 的两个用例在缓存桩内同步投递事件、或在下载期间等待截断返回，与串行化后的行为互斥（会死锁），由上述单测以异步方式覆盖。

修复后 mount（含 `page_writer`）与 filer 全包 `-race` 通过，不再报告数据竞争；server、license、s3api、command、shell、util、pb 测试（`CGO_ENABLED=0`）与 Linux 交叉编译通过，`go vet ./mount/ ./filer/` 只报上游已有的一条。本轮未重做真实 FUSE 验收与 S13。

### 8.16 外部第十一轮审核修复（2026-10-15）

第十一轮审核基于 `7319db63d`，对照 license 前的基线只看后续修复引入的问题，确认 2 项（都是 P2，都影响普通 mount 写入），上轮时钟偏差与 `SetAttr` 交错的独立复现已通过。

| # | 问题 |
| --- | --- |
| 1（P2） | 8.15 的 `writeTsNs` 遍历 `Entry.Chunks`，`cacheRemoteBeforeChange` 的 `IsInRemoteOnly` 读 `Chunks`，都只持句柄锁；后台上传完成时 `AppendChunks` 在 `LockedEntry` 自己的锁内追加分块，句柄锁管不到它。普通文件连续写入即触发 `-race` 报告，未见内容损坏 |
| 2（P2） | `writeTsNs` 每次写入都遍历条目分块与分块组，持句柄排他锁，开销随文件分块数线性增长：审核方 4 KB 写入处理函数微基准，500 分块 1.08 µs、5000 分块 8.32 µs、50000 分块 94.27 µs（原实现 0.36 µs） |

修复：
- **维护最大时间戳，写入不看分块**：句柄新增原子的 `chunkTsNs`，分块进入条目的四处把它抬到分块的最大 `ModifiedTsNs`（`noteChunks`，只升不降）：`SetEntry`（打开、失效替换，分块组内 manifest 已解析）、缓存远端对象装入（`takeCachedEntry`）、续期重取装入（`takeRefetchedEntry`）、上传完成追加（`AddChunks`）。截断与 flush 的合并只会减少或保留分块，不需要更新；多出来的旧上限只让写入时间戳略晚，无害。`writeTsNs` 只比较本机时间、上一次写入与 `chunkTsNs`，不读 `Entry.Chunks`。
- **远端判断在 `LockedEntry` 锁内**：`LockedEntry` 增加 `IsInRemoteOnly`，遮蔽内嵌 `Entry` 的同名方法，读取、下载、装入与修改前缓存的判断都在与 `AppendChunks` 相同的锁内。

单测 `TestWriteAfterUploadedChunks`：一个协程追加 1000 个时间戳领先 5 秒的分块（`AddChunks` 与分块组），主协程在句柄排他锁内反复取写入时间戳并做修改前缓存判断，之后一次写入的时间戳晚于全部追加的分块；去掉修复后 `-race` 报告竞争。基准 `BenchmarkWriteTsNs`：500 与 50000 个分块都约 25 ns、零分配。

修复后 mount（含 `page_writer`）与 filer 全包 `-race` 通过；server、license、s3api、command、shell 测试（`CGO_ENABLED=0`）与 Linux 交叉编译通过，`go vet ./mount/ ./filer/` 只报上游已有的一条。本轮未重做真实 FUSE 验收与 S13。

### 8.17 外部第十二轮审核（性能复测）修复（2026-10-15）

第十二轮审核基于 `c00783ec7`，复测 8.16：4 KB 写入（`WFS.Write` 写入内存缓冲，不含网络与落盘，五轮中位数）500、5000、50000 分块分别从 1.24、9.30、97.60 µs 降到 0.40、0.40、0.41 µs；时间戳计算约 25 ns、零分配；50000 分块的条目初始化多约 0.09 ms（1.91 → 2.00 ms），不再由每次写入承担。8.16 的两项并发复现连续三轮通过。

另确认 1 项条件性回归（P2）：manifest 首次解析失败（无证书）、重试成功时，`resolveChunksAgain` 装入新分块组没有抬高 `chunkTsNs`，而 `SetEntry` 时分块组里还没有 manifest 的内部分块（manifest 块本身的时间戳为 0）。内部分块的时间戳若领先本机（复现用 30 秒），之后的写入时间戳更旧，上传后读取仍是旧内容。8.16 把“同一批分块重建分块组”当作不引入新时间戳，漏掉了这种情况。

修复：`resolveChunksAgain` 装入新分块组时同样调用 `noteChunks`。单测 `TestWriteAfterManifestResolved`：manifest 经 filer 代理从 httptest 取，首次 403、恢复后内部分块时间戳领先 30 秒，重试读到 `world` 后写入的时间戳晚于该分块；去掉修复则失败。mount 全包 `-race` 通过。

### 8.18 外部第十三轮审核修复（2026-10-15）

第十三轮审核基于 `64539d29c`，排除上游原有问题后确认 1 项（P2）：后台上传完成的回调（`saveChunkedFileIntervalToStorage`）追加分块后直接读 `entryChunkGroup` 指针并 `AddChunk`，不持 `entryLock`；`resolveChunksAgain`（P3 引入）在 `entryLock` 写锁内取当前分块、替换分块组。上传若在取当前分块之后、替换之前完成，分块会加进即将丢弃的旧分块组，当前句柄漏读这段数据（flush 的元数据仍含它）。`-race` 稳定报告，未复现持久化丢失。续期重取与缓存远端对象的装入（`takeRefetchedEntry`、`takeCachedEntry`）同样在写锁内替换分块组，也受影响。**2026-10-25 补充（8.22 方案 7）：** 替换点还有 `invalidateHandle`（元数据事件）与 `AcquireFileHandle`（同一 inode 再次打开）里的 `SetEntry`，此前只持句柄锁或句柄表锁，现同样在 `entryLock` 写锁内调用。

锁序：`doFlush` 等待上传完成时不持 `entryLock`；`entryLock` 的写锁持有者（重试解析、重取装入、缓存装入、截断、xattr）都不等待上传。因此上传回调可以在 `entryLock` 读锁内完成“追加分块 + 加入分块组”（`addUploadedChunk`）：要么在替换前完成，分块在替换时取到的当前分块里，被加进新分块组；要么在替换后完成，加进新分块组。

单测 `TestUploadDuringManifestRetry`：manifest 取回延迟 50 ms，期间一个上传完成；重试读到 `world` 后，读取上传的那段得到 `newer`；去掉读锁则 `-race` 报告竞争。mount 全包 `-race` 通过。

### 8.19 外部第十四轮审核修复（2026-10-15）

第十四轮审核基于 `21dfab3ac`：8.18 的上传竞态修复后 60 次并发场景通过，追加/覆盖写、三种完成顺序、`Fsync`、重新打开读取 18 个场景通过；`64539d29c → 21dfab3ac` 的性能对比（各 6 样本中位数）：4 KiB 内存写（5 万分块）359.35 → 360.50 ns，上传完成后的本地处理 291.0 → 290.9 ns，4 KiB 回环上传 89.13 → 89.85 µs，1 MiB 回环上传 362.86 → 361.68 µs，分配次数不变。

另确认 1 项（P2）：`Lseek`（`SEEK_DATA`/`SEEK_HOLE`）只持句柄共享锁，直接读 `entryChunkGroup` 指针搜索分块；`Read` 也只持共享锁，续期恢复时经 `resolveChunksAgain` 在 `entryLock` 写锁内替换分块组，两者不互斥，`-race` 稳定报告。续期重取与缓存远端对象的装入同样受影响。修复：`Lseek` 取文件大小与搜索分块都在 `entryLock` 读锁内，锁序与 `Read` 相同（句柄共享锁 → `entryLock` 读锁）。

单测 `TestLseekDuringManifestRetry`：manifest 取回延迟 50 ms，期间 20 次 `SEEK_DATA`，重试读到 `world` 后分块组能搜到数据（未解析的分块组没有 section）；去掉读锁则 `-race` 报告竞争。mount 全包 `-race` 通过。

### 8.20 对照 MassDB SQL 2026-10-15/16 修订（2026-10-21）

MassDB SQL（`2.0.5-license` 分支）在 OSS 契约冻结（2026-10-13）之后有两轮许可修订：
- 2026-10-15 `c9b69a9a6b0`：取代式续期。序号更高的证书完全取代旧证书，不再要求不缩短期限、不降额度、不减功能、不留空档，只检查实际用量；validate 与 import 的响应新增 `changes`。
- 2026-10-16 `4f53a3e9585`：降级兼容、内置公钥、取消导入前的 FE 包检查。

协议 v1 文档自 2026-09-25 起未改。签发工具只改了续期预检，`weed/license/testdata/gen_issuer_vectors.py` 用到的签名与导出函数没变，互通向量仍然有效。

| MassDB SQL 的变化 | OSS 现状 | 处理 |
| --- | --- | --- |
| 取代式续期，只查实际用量 | 已经如此：`CheckImport` 只查序号、到期与账本用量；未来生效时等待期取新旧较小额度，被顶掉的待生效证书丢弃 | 不改规则，契约 1.5 第 8 条写明 |
| validate、import 返回 `changes` | 没有，缩短与降额静默接受 | **本次补上**（见下） |
| 拒绝未来生效的降额证书（D1） | 接受 | **不跟进**：D1 的理由是 Doris 旧版本回放兼容，OSS 没有带许可的旧版本；契约 1.5 第 9 条写明差异 |
| 内置公钥、外部清单可选 | 本来就只用编译进二进制的清单，没有外部文件 | 不适用 |
| 取消导入前的 FE 包检查 | 从未有过 | 不适用 |
| 许可持久化格式冻结（D2） | 未发布 | 发布后同样遵守：`state.json`、`license-cache.json` 与 master 间同步的格式要让旧版本能读 |
| 换钥纪律（D4） | 同样适用；OSS 换钥只能发版 | 运维手册写明 |

正式信任根（`productionTrustManifest`）的来源：2026-10-21 决定由独立签发项目为 OSS 生成自己的密钥，不沿用 MassDB SQL 内置的 `self-use-license-20260929`（契约 1.7）。理由：
- MassDB SQL 的签发工具只能签 `MassDB SQL` 的证书，OSS 无论用哪把密钥都需要独立签发项目，共用密钥省不掉这部分工作；
- OSS 的公钥编译在二进制里，换钥只能发版，共用密钥会让两个产品的换钥与回退约束绑在一起；
- 一把私钥泄露只影响一个产品。`product` 字段能防止证书在两个产品间串用，防不住泄露后的任意签发。

信任清单在签发项目产出公钥前保持为空，发布前填入。

**本次改动：**
- `Certs.CheckImport` 计算 `changes`（`state.go`），validate、import 与启动时的 `-license.file` 共用。比较对象是导入时生效的证书。代码与 MassDB SQL 相同，额度按 master、volume、filer 分开。
- `ImportOutcome`、`ValidateOutcome` 加 `changes`，HTTP 与 gRPC（shell 用）同一份 JSON，`PARTIAL_COMMIT` 的错误体也带上。
- 导入日志与 `-license.file` 应用日志附上变更代码。shell 的 `license.validate/import` 在 JSON 后加一行 `changes: ...`。
- master 页面的 Import 先做 validate，在确认框里列出变化；validate 失败直接显示错误，不弹确认框。validate 与 import 的结果上方也列出变化。

**验证：**
- 单测 `TestCheckImportChanges`：无变化、首个证书、幂等重试为空数组；立即生效的缩短、降额与去掉 `DATA_READ`；未来证书顶掉待生效证书并留空档、降低 volume 额度；待生效证书已到点时按生效证书比较。
- 单测 `TestManagerReportsChanges`：同一证书 validate 与 import 的 `changes` 序列化后逐字节相同；重试为 `"changes":[]`。
- license（含 `-race`）、server、shell、command 测试与 CI 标签 Linux 构建通过。
- 本机真实进程（单 master，测试信任根）：

| 步骤 | 结果 |
| --- | --- |
| HTTP 与 shell validate、shell import c2（期限 30 天→10 天，volume 3→1） | `EXPIRY_SHORTENED`、`VOLUME_SERVER_LIMIT_REDUCED`；shell 另起一行 `changes: ...`；master 日志 `imported oss-c2 sequence 2; changes: ...` |
| 导入 c3（1 小时后生效，额度不降） | `changes` 为空 |
| validate 与 import c4（c2 到期 21 天后才生效） | 两者相同：`COVERAGE_GAP`（1814400 秒）、`PENDING_DISCARDED`（c3） |
| 导入 c5（2 小时后生效，filer 2→1） | 接受：`FILER_LIMIT_REDUCED`、`PENDING_DISCARDED`（c4）；重试 `unchanged=true`、`changes=[]`；等待期 filer 额度为 1 |
| 页面导入 c6（立即生效，期限缩短，filer 2→1，顶掉 c5） | validate 结果、导入确认框与导入结果都列出三项变化，导入成功 |
| 页面导入已被取代的 c1 | 显示 409 `LICENSE_SEQUENCE_NOT_INCREASING`，不弹确认框 |

### 8.21 安装包与签发中心互通入口（2026-10-21）

MassDB Manager 的 OSS 计划 O1（部署中心用的安装包）与 O2（签发中心出 OSS 证书）对本仓库授权部分的改动，不改授权行为：

- `TestExternalIssuerVectors`（`weed/license/external_vectors_test.go`）：`SEAWEEDFS_LICENSE_VECTORS_DIR` 指向外部签发方产出的目录时运行，未设时跳过。目录含 `trust.json`（导出的信任清单）、可选的 `trust.b64`（同一清单的 base64url，必须逐字节解码回 `trust.json`）与 `vectors.json`（`{"vectors":[{"file","expect","kid","claims"}]}`，`expect` 为 `VALID` 或 `ParseCertificate` 的原因码）。证书文件先去首尾空白，与 `-license.file` 和导入接口一致；两份清单分别作为生产根与测试根各验一遍。签发中心的 `MassDbOssInteropTest` 调用它。已用 `testdata`（kid `interop-2026`）与签发中心测试根（kid `massdb-oss-test-20261021`）的向量验证通过，并确认期望不符时失败。
- `license.ProductionTrustManifest()`：返回编译进去的生产清单原文，供出包工具写进 `package.json` 并核对二进制。
- `make massdb-package`（`package/`，规范见 `package/README.md`）：测试根经 `-ldflags -X …/weed/license.testTrustManifest=` 注入，注入前用 `NewTrustStore` 按同样规则校验（与生产清单 kid 重复也会报错）；`package.json` 记录 `trust` 与 `trustEncoding`（测试根 `ldflags-base64url`，生产根 `embedded-json`），出包时确认该串原样存在于 `bin/weed`。生产清单为空且没给测试根时拒绝出包；生产清单非空时暂不支持叠加测试根。
- 实测：签发中心测试根打出的 arm64 包在容器里起 master，`GET /license` 为 `trust_root: "test"`，签发中心的样例证书（占位部署标识）`POST /license/validate` 返回 `LICENSE_DEPLOYMENT_MISMATCH`，即 kid 受信任、验签通过。未签发本集群的证书，带证书的 volume / filer 注册与读放开留待 Manager 计划 O9。

验收场景（真实进程：3 master（另测 2 voter + 1 nonvoter 与 5 voter 两组）+ 3 volume + 2 filer + 1 s3 + 1 mount + 1 `weed server -filer -webdav`；一组开启 `-cipher`）：

| # | 场景 | 期望 |
| --- | --- | --- |
| S1 | 无证书 | volume/filer POST 200；filer HEAD/`?metadata=true`/目录列表 200；S3 Put/List/Head 正常；volume GET、filer GET 403 `NO_LICENSE`；S3 GetObject 标准 `AccessDenied`；mount 对普通分块文件读 `EIO`，写正常；S3 重启后 IAM 鉴权仍生效（未授权的 Put/Delete 被拒） |
| S1b | 独立 mount、无缓存、无证书、首次打开 manifest 文件 | `read` 返回 `EIO`，不得返回全零正文；保持原句柄不关闭，导入证书并等目标 volume 的新证书生效后（最长一个心跳周期），下一次满足 5 秒重试间隔的读取触发重解析，成功后同一句柄可读；旧句柄未关闭时再次 `open` 也可读；失败状态下重试频率 ≤ 1 次/5 秒。端到端恢复上限 = 心跳周期 + 5 秒 + manifest 解析耗时，记录实测值 |
| S2 | 导入有效证书 | ≤ 1 个心跳周期内全部节点读恢复；`GET /license` VALID；三个 master 的 `LicenseState`（cluster_id、newest、prev、internal_read_key、repair_seq）一致，水位允许不同 |
| S3 | 到期边界 | 2 分钟有效期证书：到期秒起（加 4.3 粒度）GET 403 `EXPIRED`，写与元数据不受影响，续期后恢复 |
| S4 | 未来生效证书 | 导入 `not_before` 在未来的 12：`newest=12, prev=当前`；到点各节点按本地时钟切换，master 无需下发；期间额度取两者较小；再导入更高序号且立即生效的 13：所有 master 与节点整体替换为 `(13, nil)`，切主后 12 不复活 |
| S5 | 篡改/错集群/错产品/sequence 回退/同序号不同内容 | 400/409，保旧 |
| S6 | 运行中 master 全部失联 | 已启动节点继续按快照读写；到期按单调时钟生效；master 恢复后一致。不测试 master 离线时重启节点 |
| S7a | 部分提交达到多数派 | 3 voter 中 1 个 follower 推送失败：导入 200，`peers_failed` 列出；它重连后经首帧追平 |
| S7b | 部分提交未达多数派 | 3 voter 中 2 个失败：503 `PARTIAL_COMMIT`；重试同一证书幂等成功 |
| S7c | nonvoter 不计入 | 2 voter + 1 nonvoter，仅 nonvoter 成功：503 |
| S7d | 切主对账写回 | 5 voter：A 仅本地落盘 11 后 B 当选，B 对账选中 11，在写回到至少 2 台其他 voter 前保持 `RECONCILING`；随后 A/B 不可用，C/D/E 选主仍得到 11 |
| S7e | 迟到请求 | 旧 leader 的 `SyncLicense` 因任期不等于当前任期被拒 |
| S7f | 导入后扩容 master | A/B/C 导入 11 后（C 故意落后）加入 D/E：先为 nonvoter，追平并确认 11 后才提升 voter；随后 A/B 不可用，C/D/E 选主得到 11。另测导入进行中加入：提升被锁排斥到导入完成后；扩容的同步与确认延迟记录 |
| S7h | 缩容 master | 11 在 A/B 提交、C 仍为 10：`RaftRemoveServer B` 先把 11 重提交到 {A, C} 才移除；若 C 不可达则移除被拒。goraft 组：`GET /license` 的 `voters_acked` 正确列出未确认的 C |
| S7i | 账本跨任期版本 | 旧 leader 留下未确认的高修订，新 leader 确认了低修订后旧 leader 恢复：对账取新任期版本，旧版本不覆盖 |
| S7g | 对账期间注册延后 | 新 leader 持旧高额度证书、最新证书额度更低：对账完成前所有注册（含旧地址）返回可重试错误，volume/filer 数秒后自动重连成功；对账完成后新地址按新额度；对账期间节点的授权层读写不变（不下发拒读原因，不清缓存） |
| S8 | leader 时钟回拨 > 5 s | `CLOCK_SUSPECT`；导入更高序号且 `issued_at <= now` 的证书后恢复；切主到曾持未来水位的 master 不再进入 `CLOCK_SUSPECT`；同任期迟到的低 `repair_seq` 请求不抬高水位 |
| S9 | `max_volume_servers=3` 时启动第 4 个 | 被拒并持续重试，本地 GET 403 `NODE_REJECTED`；已注册的 3 个在宽限内重启/断连重连不被拒；**旧 leader 强杀后切主，原 3 个在线节点重连不被第 4 个挤掉**；让某个 follower 的 KeepConnected 落后数分钟再强杀 leader 并由它当选，结论不变（账本经 `SyncLedger` 确认而非 KeepConnected） |
| S10 | 宽限释放 | 停 1 个超过宽限后第 4 个加入；宽限内被拒；`DELETE /license/nodes/{addr}` 后立即加入；并发启动第 4、5 个只有一个成功；节点离线 1 分钟后切主，它的宽限截止时间不变 |
| S11 | 降额续期 | 账本 3 条时导入上限 2 被 409；下线并释放后成功 |
| S12a | filer 冷启动被拒 | 第 N+1 个 filer 卡在等待 master，HTTP 端口不监听，日志出现 `license:` 原因 |
| S12b | filer 运行中被拒 | 已运行的 filer 断连超过宽限后被占额，重连被拒：其 HTTP/S3 读返回 403 `NODE_REJECTED`（vid 缓存仍在也拒绝）；已注册 filer 宽限内重连不被拒 |
| S13 | 性能 | 3 轮 A/A + 3 轮 A/B，同硬件/数据/并发/连接。负载组：volume 直连 GET、filer GET 单 chunk、filer GET 多 chunk 大文件、S3 GetObject、filer/S3 PUT、DELETE、manifest 文件 `UpdateEntry`（含 `-cipher`）、小块 merge 写入（含 `-cipher`）、大目录（10 万条）HTML 与 JSON 列表、内联小文件 metadata、元数据日志回放（重启 filer 追平 100 万条）；每窗预热 30 s 计时 120 s；另跑续期、切主、filer 重连、拒绝态高并发 GET、拒绝态大目录 JSON、200 个 volume 集中重连（账本批次复制）事件窗，以及“一个 voter 不可达、另一个 voter 的 `SyncLedger` 人为延迟 2 秒时 200 个 volume 集中重连”事件窗（注册延迟 p99 不得受该延迟影响）。采集吞吐、p50/p99/p999、整进程 CPU、分配速率、RSS、GC，以及 master 的 CPU、分配、`state.json` 写盘次数与注册延迟。通过标准：各组吞吐与 p99 的 A/B 差异落在 A/A 噪声带内；整进程 CPU 与分配速率差异在噪声带内；拒绝态日志 ≤ 1/分钟/节点 |
| S14 | 旧版本混部 | 新 master + 旧 volume 不拦截（必须同批升级）；新 volume + 旧 master 无缓存时 `NOT_READY` |
| S15 | S3 CopyObject 无证书 | `InvalidCopySource`，目标不存在或不变 |
| S16 | 内部维护无证书 | ≥ 10000 块文件 `UpdateEntry` 成功（明文与 `-cipher` 各一次）；删除后 `volume.fsck` 无孤儿；filer 重启后 `SubscribeMetadata` 回放追平；小块合并写入成功；按 chunks 存储的 `filer.conf` 启动加载与变更重载成功；`NOT_READY` 的 volume 对带头请求仍拒绝 |
| S17 | 内联正文（v9 改） | 见 P0 契约 8.2：HTTP 三处出口与 gRPC 六处出口均隐藏且 `FileSize` 不变；系统豁免路径不隐藏；mount 读 `EIO`，续期后原句柄恢复 |
| S18 | `weed server -filer -webdav` 同进程 | 无证书与过期时 webdav 下载失败，S3 GetObject 403；manifest 文件 `UpdateEntry` 仍成功 |
| S19 | volume gRPC 原始读（v9） | 见 P0 契约 8.2 |
| S20 | 隐藏正文的回写与复制（v9） | 见 P0 契约 8.2 |
| S21 | 无证书准入与升级（v9） | 见 P0 契约 8.2 |

### 8.22 第十五轮审核（2026-10-22，方案已定、代码未改）

第十五轮审核基于 `41301715a`，在本仓库会话内完成，范围是分支相对上游 `5e4696065` 的全部非测试 Go 改动：`weed/license` 核心包逐行、master 集成（raft 适配、对端 RPC 地址校验、准入、HTTP/gRPC 管理接口、shell、`license.init`）、volume/filer 读守卫与内部令牌、mount 读失败与重取、Cassandra 哈希键存储。`CGO_ENABLED=0 go build ./...`、`go vet ./license/`、`go test -race ./license/` 通过。未发现丢数据或泄露正文的问题；确认 12 项，均未修改代码，方案与取舍记录如下。级别：P2 影响功能或可用性，P3 窄窗口、信息项或与上游一致的瑕疵。

| # | 问题 |
| --- | --- |
| 1（P2） | mount：`weedfs_attr.go:117-126` 截断到 0 只经 `clearHiddenContent` 清隐藏类读失败；`SetEntry`（`filehandle.go:98-110`）记下的 manifest 解析失败（`readFailure.hidden=false`）原样保留，而 `readFromChunks`（`filehandle_read.go:63`）先查 `readError()` 再读脏页。无有效证书打开带 manifest 的大文件 → `O_TRUNC` 或 `truncate(0)` → 写入 → 读取，5 秒内返回 `EIO`，尽管条目里只剩刚上传的普通分块、脏页也有数据；读取晚于上次失败 5 秒才由 `maybeRetryRead → resolveChunksAgain` 清掉。部分截断同理：`SetChunks` 重新解析剩余 manifest 的结果（`filechunk_group.go:77`）被忽略。与"每条写路径都清隐藏标志"的规则不对称：导致失败的分块已经没了，标志却还在 |
| 2（P2） | filer.backup：`filer_license.go:27-31` 的 `KvGet` 白名单只有 `Meta`、`sync.`、`remote.sync.`，`filer.backup` 的检查点键前缀 `backup.`（`filer_backup.go:90`）不在其中。拒读态 `getOffset` 得到 `PermissionDenied`，`filer_backup.go:119` 当作"无检查点"从 1970 开始；目录与删除事件此时仍成功，`AddOffsetFunc` 3 秒后经 `setOffset`（`KvPut` 不受限）用远古时间戳覆盖检查点；随后遇正文读 403 停流，`runFilerBackup` 每 1.7 秒重试并重复。续期后从该偏移续传，整段历史重新复制到 sink（localsink 重写每个文件，S3 sink 重传）。契约 5.3 的 `KvGet` 行与 S20 都要求 filer.backup"续期后从原偏移续传"，白名单在契约与代码里都漏了 `backup.` |
| 3（P2） | master：hashicorp/raft v1.7.2 的 `GetConfiguration()`（库源码 `api.go:897-903`）只填 `latest` 不填 `latestIndex`，`Index()` 恒为 0，`master_license.go:89` 取到的 `Membership.Index` 始终为 0。`RemoveMaster` 的 `after.Index != mem.Index` 永远为假，只剩授权修订号比较；`LeaderInfo.ConfigIndex` 在线上也始终为 0（接收方忽略）。契约 138、198、412 行与计划 3.3 第 8 条"配置索引已变化则 `MEMBERSHIP_CHANGED`"在生产上不生效；`manager_test.go` 通过是因为假 raft 提供了真实索引。进程内 `opMu` 串行化成员变更、非 leader 的 `RemoveServer` 会被 raft 拒绝，实际窗口小，但设计依赖的不变量没有被执行 |
| 4（P2） | master：`master_license_peers.go:152-170` 解析主机名失败时返回旧 IP 但不刷新 `at`，下一次调用再解析，每个名字付 2 秒超时；`checkLicensePeer`（88-98）的 hosts 把 `-peers` 与 raft 成员各列一遍不去重。用主机名互连时 DNS 不可用（k8s CoreDNS、docker 内置 DNS 故障）：每个 master 间 RPC 至少付两次 2 秒，超过 `peerRPCTimeout` 3 秒，导入 `PARTIAL_COMMIT`、账本每 5 秒"未确认"、此时选主的新 leader 卡在 `RECONCILING` 拒绝所有注册；raft 本身在既有连接上照常运行 |
| 5（P2） | master 时钟：`ApplySync` 对相同 `repair_seq` 经 `ClockState.Merge`（`state.go:252-261`）取较大水位，而对账写回与 `pushPending` 推送的 `stateLocked()` 带的是 leader 当前墙钟。4.5 只在对账路径避免"一台时钟超前的 master 让新 leader 进入 `CLOCK_SUSPECT`"，`SyncLicense` 路径又把它带回来。leader 快 N 秒（N > 5）时每次推送后 follower 的 `refreshClock` 打 `CLOCK_SUSPECT` 错误日志、指标置位；若在 N−5 秒窗口内切主到它，新 leader 对全集群拒读到窗口结束。需 master 间时钟不同步才触发 |
| 6（P3） | master：`master_grpc_server.go:73-101` 的 defer 先 `DisconnectNode` 再 `UnRegisterDataNode`；对账用的 `licenseConnected()`（`master_license.go:268`）读 Topo，且 `reconcileLocked` 在 `m.mu` 外取快照后再换账本。流恰在对账期间结束时账本项在线但无 `conns`，不进宽限，`DELETE /license/nodes/{addr}` 报 `NODE_ONLINE`，直到该节点重连再断开或下次切主。窗口很窄 |
| 7（P3） | mount：`weedfs.go:168`（`invalidateHandle`）与 `filehandle_map.go:52`（`AcquireFileHandle`）调用 `SetEntry` 只持句柄锁或句柄表锁，不持 `entryLock`；`SetEntry` 写 `entryChunkGroup` 与 `entry`；`UploadPipeline.Shutdown()` 不等在飞上传，`addUploadedChunk` 只持 `entryLock` 读锁。8.18 列的三个替换点漏了这两处，`-race` 可见；上游本有同类竞争，后果限于分块加进即将丢弃的旧条目 |
| 8（P3） | master：`master_server.go:380-389` `OnPeerUpdate` 把 nonvoter 也当"已在配置里"，`JoinMaster` 10 秒内未确认留下的 nonvoter 不再自动提升，需 `cluster.raft.add`。计划 3.3 第 7 条"任一步失败保持 nonvoter 并告警"已知，但比上游少了重连自愈 |
| 9（P3） | filer：`filer.go:97-114` 引导只在对端隐藏正文时无限重试（每 5 秒全量 BFS），被配额拒绝的 filer 永远拿不到令牌；`setOrLoadFilerStoreSignature`（`filer.go:162-172`）在引导前已写 `filer.store.id`，中途杀掉后下次启动 `isFresh=false`，半份命名空间被当作已初始化 |
| 10（P3） | filer：`filer_server_handlers.go:67` 对 `/?proxyChunkId=` 的 license 403 在 `maybeCheckJwtAuthorization`（82）之前；上游该分支本就不走 filer JWT（靠 volume 侧 JWT），配置 `jwt.filer_signing` 时未鉴权调用者能看到 403 里的 reason、cluster_id、expires_at |
| 11（P3） | mount：`filehandle_license.go:252-256` 隐藏期间改过大小再取回正文时只裁剪或补零内联正文，`Attributes.Md5` 沿用旧值，flush 后 ETag 与内容不符（上游分块截断与 `SetAttr` 内联裁剪同样不改 md5）。另：无句柄截断仅远端文件（`weedfs_attr.go:68 → cacheRemoteEntry`）时，filer 的缓存事件与 `saveEntry` 的 meta cache 更新没有排序，`stat` 可能停在旧大小——中等置信度，待复现 |
| 12（P3） | 契约与代码不一致：契约 5.2 元数据白名单含 `.cpx`，`volume_grpc_copy.go:425` 的 `isCopyFileMetadataExt` 没有；树内无调用方，无实际影响 |

方案（待实施；每项独立提交，带单测，计划与契约按下列条目同步修订）：

1. **截断后清除非隐藏类读失败。** `weedfs_attr.go` 截断分支：size 为 0 时无条件清掉 `readFailure`（新增不区分隐藏类的 `clearReadFailure`），旧内容一点不剩；`hiddenCut`、`ContentHidden`、`RemoteEntry` 的处理不变。部分截断时用 `entryChunkGroup.SetChunks` 的返回值更新非隐藏类失败：解析成功或剩余分块无 manifest 则清除，失败则记新错误与时间；隐藏类失败不受影响。**解析成功时必须同时调用 `noteChunks(chunks, entryChunkGroup)`（2026-10-24 复核修订）**：`weedfs_attr.go:109` 的 `SetChunks` 今天就不更新 `chunkTsNs`，首次打开解析失败、截断时才解析成功的 manifest，其内部分块的时间戳若领先本机，之后写入的时间戳比它们旧，flush 合并时写入被旧数据覆盖，与 8.17 的 `resolveChunksAgain` 同一类问题；清除读失败会让这条路径成为常态，所以一并修。单测加：内部分块时间戳领先 30 秒，部分截断后写入的时间戳晚于全部分块，按提交的元数据重新读取得到新内容。重试丢弃语义不变：`maybeRetryRead` 的 CAS 看到失败已被替换（含置 nil）即丢弃结果。单测：无证书打开带 manifest 的条目 → `SetAttr(0)` → 写 → 立即读到数据；部分截断使 manifest 全部消失后读成功；仍有 manifest 时继续 `EIO`。文档：1.6 补"截断掉导致失败的分块即清除失败状态"。
2. **`backup.` 进白名单，`getOffset` 出错不再从 0 开始。** A：`filer_license.go` 的 `licenseKvOffsetPrefixes` 加 `"backup."` 字面量（与 `"sync."` 同样写法，server 包不引用 command 包）；值为 8 字节 int64，满足 `licenseKvMaxOffsetBytes`。B（防御层，改变上游行为）：`filer_backup.go` 的 `getOffset` 返回错误时直接返回，交给 `runFilerBackup` 的 1.7 秒重试循环；"未找到"本来就是无错误且时间戳为 0，不受影响。这同时修掉上游"任何瞬时错误都从 1970 重放"的隐患。单测：`filer_license_test.go` 拒读态 `backup.` 键可读、超 8 字节仍拒。文档：契约 5.3 `KvGet` 行与 S20 加 `backup.`（记 2026-10-22 修订），本节记录。
3. **成员变更比较配置内容而非索引。** `manager.go` 给 `Membership` 加比较排序后 `Voters`、`Nonvoters` 的方法，`RemoveMaster` 以它代替 `after.Index != mem.Index`；`Index` 与 `LeaderInfo.ConfigIndex` 接收方本来就忽略，删掉以免误导（proto 字段保留、置 0）。goraft 与 hashicorp 同样适用。不采用从 `HashicorpRaft.Stats()["latest_configuration_index"]` 解析：`pushPending` 每秒调 `Membership()`，`Stats()` 代价高且是字符串解析。单测：现有 `MEMBERSHIP_CHANGED` 用例改为推送期间改变成员列表；假 raft 的 `Index` 固定 0 模拟生产。文档：契约三处"配置索引"改为"配置内容"，计划 3.3 第 8 条与 v5 说明同步。
4. **DNS 负缓存与去重。** `master_license_peers.go` 的 `resolve` 失败时写回 `resolvedHost{ips: 旧值, at: now}`，TTL 内不再重试；`checkLicensePeer` 的 hosts 用 map 去重；可选把 `licensePeerResolveTimeout` 从 2 秒降到 1 秒。单测：注入可控 resolver，验证失败后 10 秒内只解析一次、仍按旧 IP 放行。文档：8.4 补一句。
5. **采纳对端水位前钳位到本地 now。** `state.go` 的 `Merge` 增加 `now` 参数，`in.Watermark` 先取 `min(in.Watermark, now)`；`ApplySync`、`adoptAhead`、对账采纳 `best.clock` 三处都经钳位，`repair_seq` 三分支逻辑不变。理由：`CLOCK_SUSPECT` 针对本机时钟相对自己历史的回退，对端超前只是偏差。代价：新加入且时钟被回拨的 master 不再继承集群水位，该情形仍由节点侧"本地墙钟减 5 分钟"下限（4.3）兜底。不采用放宽 5 秒容差：会削弱回拨检测。单测：leader 快 30 秒推送后 follower 不进入 `CLOCK_SUSPECT`；修复降水位仍生效；S8 现有用例不变。文档：4.5 三分支规则加钳位修订。
6. **对账以 `conns` 为准，defer 顺序调整。** A：`master_grpc_server.go` 把 `DisconnectNode` 放进更早注册的独立 defer（LIFO 使其在 `UnRegisterDataNode` 之后执行）。B（根治）：`reconcileLocked` 在 `m.mu` 内用 `m.conns` 的键标在线，取代锁外的 `connected()` 快照——`conns` 正是本 master 仍有活跃流的节点，与账本替换在同一把锁下完成；`ManagerOption.Connected` 与 `licenseConnected()` 随之删除。单测：对账期间 `DisconnectNode` 的节点在新账本中进入宽限。
7. **两处 `SetEntry` 持 `entryLock` 写锁。** `invalidateHandle` 与 `AcquireFileHandle` 在 `fh.entryLock.Lock()` 下调用 `SetEntry`（锁序：句柄锁或句柄表锁 → `entryLock`，与现有一致）；`SetEntry` 内部不加锁，因三个替换点已持写锁。单测：`-race` 下并发 `addUploadedChunk` 与 `invalidateHandle`。文档：8.18 的替换点清单补这两处。
8. **只重试"待提升"的 nonvoter（2026-10-24 复核修订）。** 不能把配置里所有 nonvoter 都提升：`cluster.raft.add -voter=false` 与 `RaftAddServer{Voter:false}` 是用户明确配置的角色，上游 `OnPeerUpdate` 把它们视为"已在配置里"而不动，照原方案提升会把 3 voter 变成 4 voter、多数派从 2 变 3。改为：manager 记录 `pendingVoters`（`JoinMaster(voter=true)` 已加为 nonvoter 但推送或提升未完成的 id），`OnPeerUpdate` 与 `tick`（每 `retryInterval`）只对这个集合重试提升；成功、`RemoveMaster`、或失去 leader 身份时清除（新 leader 没有这份记忆，留下的 nonvoter 仍按 3.3 第 7 条由运维 `cluster.raft.add` 处理）。`-voter=false` 加入的 id 永不进入集合。单测：用户 nonvoter 在重连后仍是 nonvoter；待提升 id 在重连后被提升。文档：3.3 第 7 条改为"任一步失败保持 nonvoter 并告警，同一 leader 任期内在该 master 重连或每 5 秒重试提升"。
9. **引导有上限，`filer.store.id` 后写，引导切点持久化（2026-10-24 复核修订）。** `MaybeBootstrapFromOnePeer` 重试加上限（如 5 分钟）后返回错误，`filer_server.go` 的 `Fatalf` 带出原因；`OnLicenseReject` 已触发时立即失败。`SetStore` 只生成签名并返回 `isFresh`，不落库；引导成功（或非 fresh）后再 `KvPut`，半份命名空间不会被当作已初始化。只延后写标记还不够：失败的引导已经插入了部分条目，重启后的第二次 BFS 只插不删，源端在两次之间删除的文件在本地残留，而事件订阅从新的 `startFromTime`（`filer_server.go:213`，每次进程重算）开始，漏掉中间的删除事件。因此第一次引导前先把切点 `startFromTime` 写入存储（如键 `filer.bootstrap.since`），之后无论进程内重试还是 fresh 状态下重启，BFS 的 `snapshotTime` 与 `AggregateFromPeers` 的起点都用这个切点：切点之前的全量来自 BFS，之后的全部增删来自事件回放，两段拼接无缝；引导成功写入 `filer.store.id` 后删除该键。对端持久化日志若已不再覆盖切点（保留期过），引导明确失败并提示清空存储重来，不默默从新时间开始。签名值照常在内存生成，`AggregateFromPeers` 不受影响。单测：引导失败后重启仍为 fresh；第一次引导后源端删除的文件，第二次引导成功后本地不存在。
10. **代理路径最小化 403。** 推荐：`/?proxyChunkId=` 的拒绝只返回状态码与 `X-MassDB-License-Reason`，不带 cluster_id 与 expires_at，改动最小。不推荐把分支挪到 JWT 之后：会让代理读新受 filer 读 JWT 约束，是行为变化。
11. **重算 Md5；meta cache 先复现。** `takeContent` 第三分支裁剪或补零后以 `md5.Sum` 重算 `Attributes.Md5`（内联正文很小）。meta cache 旧大小一项先写复现用例，确认后再决定是否让无句柄的 `cacheRemoteEntry` 路径按路径做 `ownCacheEvent` 式标记。
12. **契约删 `.cpx`。** 代码无调用方，改契约 5.2 与代码一致。

建议顺序：1、2、3 各自独立先做；4、5 同属 master 时钟与对端层面，一起做；6–12 视情况。验证以 `CGO_ENABLED=0 go test -race` 跑 `license`、`server`、`mount`、`command` 包。契约只改 `KvGet` 白名单、"配置索引"措辞与 `.cpx`，其余记在本节。

**方案 1 实施（2026-10-25）：** `weedfs_attr.go` 截断分支：size 为 0 时经 `clearReadFailure` 无条件清除 `readFailure`；部分截断经 `setTruncatedChunks` 用 `SetChunks` 的返回值更新非隐藏类失败（成功清除并 `noteChunks`，失败记新错误与时间，隐藏类失败不动），仍在 `entryLock` 写锁内，`maybeRetryRead` 的 CAS 看到失败被替换即丢弃结果。单测 `TestTruncateClearsManifestFailure`（无证书打开带 manifest 的条目 → `SetAttr(0)` → 写 → 立即读到数据；截掉 manifest 块后读到剩余分块；仍有 manifest 时继续 `EIO` 且失败时间刷新；隐藏类失败不受部分截断影响）与 `TestWriteAfterTruncateResolvesManifest`（内部分块时间戳领先 30 秒，截断到 3 时解析成功，写入时间戳晚于全部分块，flush 后按提交的元数据重新读取得到新内容）；去掉修复或去掉 `noteChunks` 分别失败。mount 全包 `-race` 通过。1.6 已同步。

**方案 2 实施（2026-10-25）：** A：`filer_license.go` 的 `licenseKvOffsetPrefixes` 加 `"backup."`。B：`filer_backup.go` 的 `getOffset` 返回错误时 `doFilerBackup` 直接返回错误，由 `runFilerBackup` 的 1.7 秒循环重试，不再从 1970 开始；"未找到"仍是无错误、时间戳 0。单测 `TestLicenseKvGetCheck` 加 `backup.abc`；新增 `TestLicenseKvGetBackupOffset`：拒读态经真实 leveldb 存储，8 字节的 `backup.` 检查点可读、不存在的检查点返回空值、同前缀 16 字节的值与硬链接键返回 `PermissionDenied`。契约 5.3 `KvGet` 行与 S20、本计划 1.5 的旁路说明已加 `backup.`。B 的取舍按 2026-10-22 方案执行（用户 2026-10-25 确认）。

**方案 3 实施（2026-10-25）：** `Membership` 去掉 `Index`，新增 `sameServers`（复制排序后比较 `Voters`、`Nonvoters`）；`RemoveMaster` 以 `!after.sameServers(mem)` 代替 `after.Index != mem.Index`；`LeaderInfo` 去掉 `ConfigIndex`，`masterRaft.Membership` 不再读 `f.Index()`，`SyncLicense` 收发两端不再填写或读取 `config_index`（proto 字段保留、置 0）。单测 `TestManagerRemoveMasterRecommits` 改为推送期间加入 nonvoter `x` 得 `MEMBERSHIP_CHANGED`、同一集合换序后移除成功；假 raft 不再有索引。去掉比较则该用例失败。契约 138、198、412 行与本计划 3.3 第 8、10 条、v5/v7 说明已同步。

**方案 4 实施（2026-10-25）：** `master_license_peers.go` 的 `resolve` 失败时写回 `resolvedHost{ips: 旧值, at: now}`，TTL 内不再解析；去重放在 `allows` 收集名字处（同一入口，覆盖 `-peers` 与 raft 成员重复），不在 `checkLicensePeer`；`licensePeerResolveTimeout` 降到 1 秒；解析经可替换的 `licensePeerLookupIP`。单测 `TestLicensePeerGuardResolveFailure`：重复名字只解析一次，DNS 失败后 TTL 内零次解析、仍按旧 IP 放行，TTL 过后每个名字只失败一次且再过 TTL 前不重试，从未解析成功的名字被拒。8.4 已补说明。

**方案 5 实施（2026-10-25）：** `state.go` 的 `Merge` 增加 `now` 参数，`in.Watermark` 先取 `min(in.Watermark, now)` 再走三分支；`ApplySync`、`adoptAhead`、对账采纳 `best.clock` 三处都经钳位（后两处仍只在对端 `repair_seq` 更高时采纳）。单测：`TestClockState` 加三条钳位用例（同序号与更高序号的超前水位都只到 now，低于对端 now 的本地水位不被拉低）；新增 `TestManagerLeaderClockAheadDoesNotSuspectFollower`：follower 时钟落后 leader 30 秒，leader 导入后墙钟前进再推送，follower 保持 `VALID`、水位不超过本地 now，随后当选 leader 仍 `VALID`，再由 leader 导入更高序号证书时水位降到 `issued_at`；去掉钳位则该用例在第一步报 `CLOCK_SUSPECT`。`TestManagerClockSuspectAndRepair`（S8）与 `TestManagerAdoptsClockRepair` 不变。4.5 已同步。license 包 `-race` 通过。

**方案 6 实施（2026-10-25）：** A：`SendHeartbeat` 的 `DisconnectNode` 放进更早注册的独立 defer，在 `UnRegisterDataNode` 之后执行。B：`reconcileLocked` 在 `m.mu` 内用 `m.conns` 的键把仍持有流的节点置为在线，与账本替换同锁完成；`ManagerOption.Connected`、`m.connected` 与 `licenseConnected()` 删除。单测：`TestManagerLedgerReplicationAndTakeOver` 改为直接给 c 的 `conns` 放入 v2 模拟存活的流；新增 `TestManagerReconcileDisconnectDuringReconcile`：leader 重新当选、对账收集期间（`onCollect` 钩子）v1 的最后一条流断开，新账本中 v1 离线、`last_seen` 为对账时间并可 `ReleaseNode`，v2 仍在线且 `ReleaseNode` 报 `NODE_ONLINE`，`conns` 只剩 v2。license、server `-race` 通过。8.1 第 4 条已加修订说明。

**方案 7 实施（2026-10-25）：** `weedfs.go` 的 `invalidateHandle` 与 `filehandle_map.go` 的 `AcquireFileHandle` 在 `fh.entryLock.Lock()` 下调用 `SetEntry`（锁序：句柄锁或句柄表锁 → `entryLock`，与现有一致；`SetEntry` 内部不加锁）。单测 `TestSetEntryDuringUploads`：一个 goroutine 持续 `addUploadedChunk`，主 goroutine 交替触发 200 次元数据事件与重新打开；去掉两处锁则 `-race` 报告近 400 条竞争。mount 全包 `-race` 通过。8.18 的替换点清单已补这两处。

**方案 12 实施（2026-10-25）：** 契约 5.2 `CopyFile` 行的元数据白名单删去 `.cpx`，与 `isCopyFileMetadataExt` 一致；代码不变，既有单测 `TestIsCopyFileMetadataExt` 已断言 `.cpx` 受守卫。

**方案 10 实施（2026-10-25）：** `Guard.WriteDeniedStatus` 只写 403 与 `X-MassDB-License-Reason`；filer 代理分支改用 `licenseHttpProxyReadAllowed`（不接受内部令牌，与此前相同），不再带 `cluster_id`、`expires_at` 与 JSON body。未把分支挪到 JWT 之后。单测 `TestWriteDeniedStatus`、`TestLicenseHttpProxyReadAllowed`。契约 5.2 的 403 响应表拆出 F2 一行，计划 1.2 F2 行同步。

**方案 11 实施（2026-10-25）：** （a）`takeContent` 第三分支裁剪或补零后以 `md5.Sum` 重算 `Attributes.Md5`，`TestTruncateHiddenContent` 断言取回后的 md5 为 `hel\0\0` 的 md5。（b）meta cache 旧大小一项已复现并修复：把订阅的事件处理抽成 `MetaCache.ApplyEvent` 供单测直接调用，`NewMetaCache` 增加 `ownEventFn`；mount 在 `cacheRemoteEntry` 请求前按路径记标记（`markRemoteDownload`，RPC 失败即清除，一分钟过期），`ownMetaEvent` 对无句柄路径、形如"无分块 → 有分块且远端描述相同"的更新事件返回 true 并消费标记，meta cache 整条忽略；有句柄的路径一律交给 `invalidateHandle`。单测 `TestTruncateWithoutHandleKeepsMetaCacheSize`：经 gRPC 存根 filer 无句柄截断远端文件到 3，下载事件在 RPC 之后或之前到达两种顺序下 meta cache 都保持 3，标记只消费一次（之后别的客户端的 chmod 事件、未标记的下载事件都照常应用）；去掉钩子则 meta cache 回到 5。mount 全包 `-race` 通过。1.6 之后的远端缓存一节已补说明。

**方案 8 实施（2026-10-25）：** `Manager` 加 `pendingVoters`（id → 地址）：`JoinMaster(voter=true)` 在推送前记入、`AddVoter` 成功后删除；`RetryPromotion(ctx, id)` 只对集合内的 id 重跑 `JoinMaster`，`OnPeerUpdate` 对已在配置里的 master 调它（经 `licenseRetryPromotion`）；`tick` 在 leader 且对账完成时每 `retryInterval` 单飞一轮重试；`RemoveMaster`、非 leader 的 `tick`、新任期对账完成都清空集合。单测 `TestManagerPromotesPendingVoters`：`-voter=false` 加入的 n 重连后仍是 nonvoter；d、e 因不可达留为 nonvoter 并待提升，d 重连经 `RetryPromotion` 提升，e 由 `tick` 提升；移除 e 与 a 失去 leader 后集合清空。license、server `-race` 通过。3.3 第 7 条已同步。

**方案 9 实施（2026-10-25）：** `MaybeBootstrapFromOnePeer(ctx, …, since)`：对端隐藏正文时每 `BootstrapRetryInterval`（5 秒）重试，总计不超过 `BootstrapRetryLimit`（5 分钟）后带原因返回；`filer_server.go` 用 `context.WithCancelCause` 建引导上下文，`OnLicenseReject` 触发即取消，错误带 "master refused this filer"，`Fatalf` 带出。`SetStore` 对无签名的存储只在内存生成签名并返回 `isFresh`，不再写 `filer.store.id`；新增 `PersistStoreSignature`、`BootstrapSince(startFrom)`（读持久化切点 `filer.bootstrap.since`，没有则写入 `startFrom`）、`FinishBootstrap`（先删切点再写 `filer.store.id`，中途崩溃则重启仍为 fresh）。`filer_server.go` 在 fresh 时以切点作为 BFS 与 `AggregateFromPeers` 的起点：切点之前的全量来自 BFS（BFS 本就是对端当前状态，`olderThanTsNs` 只用于日志），之后的增删来自事件回放，两段拼接无缝。覆盖检查 `checkBootstrapLogCoverage`：引导前列对端 `/topics/.system/log` 最早的（UTC）天目录，晚于切点所在天则失败并提示清空本 filer 存储重来；对端无日志目录视为无事可回放。与方案原文的差别：对端日志"保留期"在 filer 里不存在（日志只会被运维删除），所以覆盖检查按天目录判定而非保留期；`filer.store.id` 未写入期间对端 `GetFilerConfiguration` 仍返回内存里的签名。单测（`filer_test` 包，gRPC 存根 filer）`TestBootstrapFreshStoreUntilFinished`：新存储不写签名；隐藏正文重试后超限返回 "still hides content"；被拒的上下文立即返回带 `NODE_LIMIT_EXCEEDED` 的原因；重启仍为 fresh 且切点不变；对端日志最早天晚于切点时拒绝并提示；覆盖后复制成功，`FinishBootstrap` 后切点删除、再启动不再 fresh 且签名一致。`TestBootstrapReplaysFromCut`：第一次引导复制的 `/d/x` 在源端删除后，第二次引导完成时本地仍在，`AggregateFromPeers` 从持久化切点订阅（存根记录 `SinceNs` 等于切点）回放删除事件后本地不存在，新建的 `/d/y` 在。filer、server、command `-race` 通过。1.5、8.2 已同步，运维手册第 12 节补排查项。

取舍（2026-10-22 提出，用户 2026-10-25 确认按原方案执行）：方案 2 的 B 改变上游行为（`getOffset` 出错由"从 0 开始"改为报错重试）；方案 5 调整 4.5 的三分支规则（相同 `repair_seq` 的水位钳位到本地 now）。

**方案复核（2026-10-24）：** 外部对本节方案做了对照源码的隔离复现，确认原方案 3 处缺陷，已改入上文：方案 1 的部分截断分支漏了 `noteChunks`，新写入会被时间戳领先的旧分块覆盖（P1，复现 `Write/Flush` 成功但重读得旧内容，补 `noteChunks` 后通过）；方案 9 只延后写标记不能保证重启后一致，第二次引导不删除源端已删的残留条目且订阅起点漏事件（P1，复现已删除文件保留），改为持久化引导切点；方案 8 会把用户以 `-voter=false` 配置的 nonvoter 一并提升（P2，复现 3 voter 变 4 voter），改为只重试待提升集合。复核同时确认正常读热路径无性能问题（守卫微基准约 0.66 ns/op、零分配），未做集群长期压测。

本轮核对无误的部分：七个 volume 数据 RPC 全部守卫，其余 35 个 RPC 不返回 needle 字节；内部令牌只出现在 `fetchWholeChunk`、`ChunkStreamReader`、`StreamContent` 与 filer/volume 之间，用户路径无泄漏；filer 六处隐藏点与 `KvGet` 一致，豁免按清理后的全路径判定；S3 CopyObject 的 403 不会变成对象内容；`Guard.AllowRead` 两次原子读无分配，心跳与 KeepConnected 只在 `state_version` 变化时下发；manager 不跨 RPC 持 `mu`，`persistMu → mu` 无反向获取；mount 锁序一致，写入时间戳单调；Cassandra 的 dirhash 布局、前缀上界与 session 清理正确。HTTP `/license/*` 只有白名单、gRPC `ImportLicense` 无鉴权、`/etc/` 整棵树豁免、master 主机上任意进程可读 `internal_read_key`，计划 1.3 与契约 5.3 均已接受，不计为问题。

---

## 9. 取舍（2026-10-13 用户已确认，确认结果见 P0 契约第 6 节）

| # | 问题 | 默认 | 备选 |
| --- | --- | --- | --- |
| 1 | 节点计数口径 | 成员账本（在线 + 10 分钟宽限），全量快照复制 | 永久登记簿，只能显式释放 |
| 2 | 从未导入证书时是否限制注册 | 不限制 | 引导额度 1+1+1；**最终选定：对齐 MassDB SQL，无证书不能加入节点（仅单 master 新集群可引导）** |
| 3 | `filer.sync`、`filer.backup`、`filer.remote.sync` 独立进程 | 证书无效时拦截 | 命令行参数携带内部令牌 |
| 4 | volume gRPC 原始读 | 不封堵 | **封堵（已选；改为 handler 入口守卫，不用拦截器）** |
| 5 | `HEAD` 与元数据接口 | 放行 | 全拦 |
| 6 | 管理接口访问控制 | 依赖厂商签名；白名单不视为防护 | 导入口令 |
| 7 | 签发工具 | massdb-sql 加产品档案 | 独立项目（**2026-10-14 改选**；massdb-sql 的签发工具只作测试用，见 7.3） |
| 8 | `-license.file` | 做 | 不做 |
| 9 | `Entry.Content` | 两个只读 HTTP 接口隐藏并打标记；gRPC 面与 mount 列为边界 | filer.proto 加 `content_hidden` 标志，`UpdateEntry` 在该标志下忽略 `Content`，gRPC Lookup/List 也隐藏；需改 filer.proto 与 mount（**已选**，规则按 1.5 v9 细化） |
| 10 | 内部令牌轮换接口 | 不做 | `POST /license/rotate-internal-key` |
| 11 | 成员账本复制批次 | ≤ 5 秒合并一次、voter 多数派确认；切主最多丢一个未确认批次 | 每次变更立即同步推送（注册延迟增加） |

---

## 10. 用户未提及但纳入的事项

- **限定四个读取函数的内部读取通道**（1.4），含配置文件读取与加密/重试保留；同进程 webdav 验收（S18）。
- **内联正文在 HTTP 与 gRPC 出口隐藏，配置目录豁免，回写按合并规则保留正文，复制/备份停在隐藏事件处**（1.5，v9），避免 IAM 回归、回写丢数据与目标端空文件。
- **volume gRPC 数据 RPC 守卫与 volume 之间的令牌**（1.2 G2，v9）；**无证书不能加入节点、master 启动前证书检查与先取证后部署/升级路径**（3.4，v9）。
- **两证书状态与时间推导生效**（3.2），取消激活转换。
- **voter 多数派、当前任期校验、选主对账写回、幂等重试**（3.3）。
- **成员账本经确认 RPC 复制、选主对账、原子预占与对账期间准入冻结**（3.3/3.4），切主后原在线节点有宽限保护，已离线节点不被延长。
- **voter 加入两阶段、移除前重提交并与导入共用临界区、goraft 模式的 `voters_acked` 运维检查，提交与对账绑定 raft 配置索引**（3.3；2026-10-25 改为配置内容）。
- **`RECONCILING` 只延后注册与管理操作，不向节点下发拒读**（3.3）。
- **账本版本 `(term, revision)` 跨任期可排序，在途批次至多一个，复制 RPC 在锁外进行**（3.4）。
- **独立 mount 对 manifest 文件的错误传播与限频恢复**（1.6），避免返回伪造的全零正文，续期后旧句柄自动恢复。
- **被拒准入的 volume 与 filer 本地读都拒绝**（3.4），冷启动与运行中分开验收。
- **三分支时钟修复同步**（4.5）。
- **按流对账下发**（4.2）、本地缓存（4.4）。
- **S3 CopyObject 状态校验与 AccessDenied 映射**（5.2）。
- **日志脱敏、拒绝限速、指标**（`license_state`、`license_expires_at_seconds`、`license_denied_reads_total`、`license_nodes_online/grace/limit{type}`）。
- **部署要求**：master 先升级；`<mdir>/license/` 随 mdir 备份；volume 第一个 `-dir` 与 filer `-defaultStoreDir` 持久化；`-saveToFilerLimit=0` 为建议而非保证。
- **性能新增工作的完整清单**（4.6）与整进程指标、补充负载（S13）。
- **运维手册**（已完成，见 [license-operations-manual-20261014.md](license-operations-manual-20261014.md)）：导出申请 → 签发 → 导入 → 续期 → 降额与宽限释放 → 存量集群升级（`license.init` → `-license.file`）→ 状态目录恢复 →（可选）开启 gRPC mTLS 与 HTTPS 保护集群内传输 → 故障排查（含 master 经 NAT 或其它网卡互连时配置 `[master.license] peer_ips`，见 8.4）。
