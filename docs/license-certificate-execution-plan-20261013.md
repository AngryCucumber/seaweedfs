# MassDB OSS（SeaweedFS 3.84）授权证书执行计划

日期：2026-10-13，v9（v8 合并八轮外部审核修订；v9 为 P0 契约冻结，冻结细节见 [P0 契约](license-p0-contract-20261013.md)）。源码基线：`claude/authorization-certificate-system-cb1fc3` / `dd3b853cc`（3.84，已含 MassDB OSS 品牌页面）。参考方案：MassDB SQL 的 `docs/license-certificate-execution-plan-20260922.md`、`docs/license-protocol-v1-20260922.md` 及 `tools/license-issuer/`。

**一句话方案：** master 是证书唯一管理点（导入/校验/持久化/多 master 同步/节点额度），证书原文沿既有心跳和 KeepConnected 流下发，volume server 与 filer 各自用内置公钥验签后缓存；只在 volume 和 filer 的 HTTP GET 文件内容处加守卫；filer 自身的维护性读取（manifest 解析、元数据日志回放、小块合并、系统配置文件）通过限定在四个读取函数上的内部令牌在 volume 放行，保证入库、更新、元数据、内部维护不受证书影响。不新增 HTTPS、不改 raft 状态机、不改 gRPC 服务语义。

**v4 相对 v3 的变化（来自第四轮审核）：** 撤销 gRPC `Lookup/List` 的 `Content` 剥离（会导致 S3 IAM 配置读空、标签回写清空内联正文、mount 读到补零数据），改为只在 HTTP `?metadata=true` 与目录 JSON 两个只读接口隐藏并打 `X-MassDB-Content-Hidden` 标记，gRPC 面的内联正文列为边界；授权状态改为 `(newest, prev)` 两证书、生效证书由时间推导，不再有 pending 激活转换，排序只看 `newest.sequence`；选主对账选中的状态必须先写回 voter 多数派才退出 `RECONCILING`；任期校验改为与当前 raft 任期相等；时钟修复同步改为三分支；宽限账本改为“在线 + 宽限”的全量成员快照复制；内部通道补入 `StreamContent`（filer.conf、IAM 配置按 chunks 存储时的读取）并保留加密与重试；补 master/filer/volume 准入的原子预占；S12 拆冷启动与运行中；S13 补大目录、内联文件、加密 manifest/merge、日志回放负载。

**v5 相对 v4 的变化（来自第五轮审核）：** 独立 mount 首次打开 manifest 文件时解析失败只打警告、随后按稀疏文件补零返回，必须让打开失败而不是返回伪造正文，因此 mount 需要约 5 行改动；voter 增删与授权状态转移定义顺序，提交与对账绑定 raft 配置索引；`RECONCILING` 期间冻结新地址准入；成员账本改为带确认的 RPC 复制并在选主时对账，撤销“最多丢 5 秒”的硬保证；切主只给原在线节点新宽限，已离线条目保留原截止时间；`StreamContent` 的令牌经私有中间函数传递，用户入口传空；补账本复制的后台成本与测量。

**v6 相对 v5 的变化（来自第六轮审核）：** master 成员变更改为完整的“加入两阶段（nonvoter 追平 → 同步授权与账本 → 提升 voter）”与“移除前先把已确认状态重提交到剩余 voter 多数派”，goraft 静态配置模式只能靠运维检查 `voters_acked`；账本版本改为 `(term, revision)` 跨任期可排序，定义同版本不同内容与迟到批次处理，在途批次至多一个；`RECONCILING` 期间所有注册（含旧地址重连）一律延后，对账与接管转换完成后再开放，避免恢复覆盖刚重连的节点；mount 的 manifest 错误改为在失败状态下限频重解析、成功即清错，并补“保留旧句柄续期恢复”验收。

**v7 相对 v6 的变化（来自第七轮审核）：** 移除 voter 的整个流程（重提交确认到实际移除）与导入、其他成员变更共用 manager 临界区，并在执行前重新核对授权版本与配置索引；统一 `RECONCILING` 语义为“只延后注册与管理操作，节点保留现有授权快照”，撤销向节点下发 `RECONCILING` 拒读原因；S1b 的恢复时间改为“目标 volume 新证书生效后，下一次满足重试间隔的读取触发重解析”；补充 manager 锁内 RPC 截止时间与 mount 重解析并发协调的实现要点。

**v8 相对 v7 的变化（来自第八轮审核）：** 普通账本复制改为“锁内取快照、锁外发 RPC、回来带版本校验”，不在注册共用的 manager 锁内等待网络；只有导入、成员变更与选主对账保留锁内同步 RPC；S13 补“账本 RPC 慢响应或不可达时 200 节点集中重连”的注册延迟验收；1.6 的“5 秒内恢复”统一为 S1b 口径。

**v9 相对 v8 的变化（P0 契约冻结，用户确认第 9 节）：** 决策 2 改为对齐 MassDB SQL 的“无证书不能加入节点”（volume、filer 额度 0，master 只允许单 master 新集群引导，启动前做本地证书检查），补 `weed license.init` 与 `-license.file` 的先取证后部署/升级路径（3.4）；决策 4 改为封堵 volume gRPC 中返回数据正文的 7 个 RPC，volume↔volume 经 gRPC 元数据携带内部令牌（1.2 G2）；决策 9 改为封堵 gRPC 面内联正文，`filer.proto` 加 `content_hidden`，写入按合并规则保留正文，系统目录豁免，filer 之间带令牌，订阅/复制/备份遇隐藏正文停在事件处，mount 返回 `EIO` 并限频重取（1.5）。核对 v8 全部源码引用后修订：M1 补目录条目 `?metadata=true` 分支；F1 前移到 `checkPreconditions` 之前、限 GET、免清理对象头；F2 限 GET；`internal_read_key` 只发给 volume 与 filer；任期改用 `CurrentTerm()`；`-mdir` 默认临时目录告警。合计估时约 20.5 人天。

**P2 完成（2026-10-13）：** master 侧 manager、proto（含 `filer.proto` 字段）、gRPC/HTTP 管理接口、选主对账与写回、成员变更两阶段与移除前重提交、账本复制、`license.init`、`-license.file`、master 启动检查、页面与 shell 命令已实现并经单 master、3 master（goraft 与 hashicorp）、3→5 master 扩容与缩容的真实进程验证；实现与本文的差异及未覆盖项见 8.1。

**P3 完成（2026-10-14）：** 按流下发、volume/filer 守卫（V1、F1、F2、M1）、filer gRPC 内联正文隐藏与写入合并规则（G1）、volume gRPC 数据 RPC 守卫与 volume 之间的令牌（G2）、节点缓存、内部令牌传递链、订阅/备份/读取命令的隐藏正文处理、mount 错误传播、S3 两处已实现，并在 1 master + 2 volume + 3 filer（含 `-s3 -webdav` 同进程）、`weed server -filer -s3 -webdav` 与 Linux 虚拟机（mount、`-cipher`）上做了真实进程验证；P3 中修正的 P2 缺陷与上游问题、与本文的差异及未覆盖项见 8.2。

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
| F2. filer `/?proxyChunkId=` | [filerHandler](../weed/server/filer_server_handlers.go:66) 调 `proxyToVolumeServer` 前；仅 `r.Method == GET`（该代理按原方法转发，写入与 HEAD 不拦） | 明确 403 |
| M1. 三处只读 HTTP 元数据出口隐藏内联正文 | 目录条目 `?metadata=true`（[filer_server_handlers_read.go:124](../weed/server/filer_server_handlers_read.go:124)，v9 补：S3 尾斜杠目录标记对象的 `Content` 从这里返回）、文件条目 `?metadata=true`（[filer_server_handlers_read.go:147](../weed/server/filer_server_handlers_read.go:147)）、目录 JSON（[filer_server_handlers_read_dir.go:63](../weed/server/filer_server_handlers_read_dir.go:63)）：当 `!AllowRead()` 且 `len(Content) > 0` 且不在系统目录豁免内（1.5）时，序列化的副本里 `Content` 置空、`ContentHidden=true`，响应头加 `X-MassDB-Content-Hidden: true`，`FileSize` 保留 | 浏览器、curl 直接查 metadata |
| G1. filer gRPC 返回的内联正文（v9，决策 9） | `LookupDirectoryEntry`、`ListEntries`、`TraverseBfsMetadata`、`SubscribeMetadata/SubscribeLocalMetadata`、`StreamRenameEntry`、`CacheRemoteObjectToLocalCluster` 的出站 Entry，及 `KvGet`；规则见 1.5，挂点见 P0 契约 5.3 | 任意 gRPC 客户端、mount、S3 列表、订阅类命令 |
| G2. volume gRPC 数据 RPC（v9，决策 4） | `CopyFile`（非元数据扩展名）、`ReadNeedleBlob`、`ReadAllNeedles`、`VolumeTailSender`、`VolumeIncrementalCopy`、`VolumeEcShardRead`、`Query` 的 handler 入口；拒读态校验 gRPC 元数据 `x-massdb-internal-read`；volume↔volume 的 3 个出站调用点附加令牌（P0 契约 5.2）。不加拦截器 | `weed shell volume.check.disk`、`weed backup`、直连 gRPC 读取 |

两层都拦的原因：filer 若已开始流式返回 200，再被 volume 拒绝只会得到截断响应；filer 先判断能给出正确状态码。volume 层保证绕过 filer 直连也无法读。F1/F2/M1 不识别内部令牌，只看证书状态；G1 只认 filer 之间调用携带的令牌，G2 只认 volume 之间调用携带的令牌。

拒绝响应：HTTP 403，`Content-Type: application/json`，body `{"error":"license_denied","reason":"<REASON>","expires_at":<unix>,"cluster_id":"..."}`，响应头 `X-MassDB-License-Reason: <REASON>`。mount 对分块文件读到 `EIO`（[weedfs_file_read.go:52](../weed/mount/weedfs_file_read.go:52)）；含 manifest 的文件需要 1.6 的 mount 改动才能保证 `EIO`。拒绝日志限速（每节点每分钟 1 条）。

### 1.3 明确不封堵、作为已知边界

- 内部令牌（1.4）是集群内共享的静态密钥，在 gRPC 明文内网中可被截获后用于直读 volume；`ClientType` 不鉴权，能连 master 且有空闲 filer 名额的客户端可自称 filer 经 `KeepConnected` 拿到令牌；master 之间对账用的 gRPC `GetLicense(include_state=true)` 同样不鉴权，能连 master 的任何客户端都可直接取得 `internal_read_key`（P3 补记）。开启 `security.toml` 的 gRPC mTLS 后这些都不成立，部署手册要求开启。不引入每请求 HMAC。
- G2 只在建流时检查，证书有效期间已开始的 gRPC 数据流在到期后不中断。
- 同进程的 webdav（`weed server -webdav`）解析 manifest 时会经内部通道读到 manifest 块（只含 chunk id 列表）；数据块经 `fetchChunkRange` 读取，不带令牌，仍被拦。
- **内联正文的剩余边界（v9）：** 系统豁免路径（`/etc/` 全部、`/topics/` 下的 `topic.conf` 与 `*.offset`）的正文经 gRPC 照常可读，能经 filer HTTP 写 `/etc/` 的用户可借此存取小文件；`other/java` 客户端（不在构建内）读到空正文，回写依赖 protobuf-java 保留未知字段；证书到期前已发出的订阅事件不追回。
- filer 按 `notification.toml` 向外部队列（Kafka、SQS 等）发送的元数据事件在服务端生成，内联正文不隐藏；该出口由运维配置，与内部维护同级（P3 补记）。
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
- **filer 之间不隐藏：** 元数据聚合订阅（含自身）与新 filer 引导携带内部令牌；Replay 与引导遇到隐藏条目报错重试，不入库。
- **`KvGet` 旁路：** 拒读态只放行 `Meta`、`sync.`、`remote.sync.` 前缀的同步偏移键。

### 1.6 独立 mount 对 manifest 文件的错误传播（v5 新增）

独立 `weed mount` 首次打开含 manifest 的文件时，要经 volume GET 解析 manifest，它没有内部令牌，证书无效时被拒。现有代码在 [filehandle.go:78](../weed/mount/filehandle.go:78) 对解析失败只打 `Warningf`，分块表为空；随后 [filechunk_group.go:59](../weed/filer/filechunk_group.go:59) 把缺失区间当作稀疏文件补零并返回成功，应用会拿到伪造的全零正文而不是错误。

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
| 签发 | 复用 `massdb-sql/tools/license-issuer/`，见 7.3 |
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
- 有效额度：`newest` 未生效期间取 `min(prev.limit, newest.limit)`。
- 导入校验：`new.sequence > newest.sequence`；`new.not_before <= now` 则状态为 `(new, nil)`，否则 `(new, effective(now))`。
- `watermark/repair_seq` 独立于证书部分，同步规则见 4.5。

### 3.3 多 master 同步

不改 raft 状态机。采用“只在 leader 导入 + voter 多数派同步推送 + 选主对账并写回多数派 + 按流对账”：

1. **只有 leader 接受导入。** 导入 handler 自检 `ms.Topo.IsLeader()`，否则 503；不依赖 `proxyToLeader`（找不到 leader 时它会直接调本地 handler，[master_server.go:222](../weed/server/master_server.go:222)）。
2. **voter 多数派。** voter 集合取自当前 raft 配置：hashicorp 用 `GetConfiguration().Configuration().Servers` 中 `Suffrage == Voter` 的成员（现有 [RaftServer.Peers()](../weed/server/raft_server.go:173) 不区分，manager 内另取；`RaftAddServer` 支持 nonvoter，[master_grpc_server_raft.go:51](../weed/server/master_grpc_server_raft.go:51)）；goraft 全部成员是 voter。多数派 = `floor(voters/2)+1`，leader 自身计入。nonvoter 也推送，不计入多数派。
3. **`SyncLicense` 携带 `leader_address` 与 `leader_term`。** follower 只接受 `leader_address == Topo.Leader()` **且 `leader_term == 本地 raft 当前任期`**（hashicorp `CurrentTerm()`，goraft `Term()`）的请求；不用“最高已收到的 Sync 任期”，避免同一地址 term1 当选、落选、term3 再当选时 term1 的迟到请求被接受。
4. leader 先写本地 `state.json`，再同步推送；voter 成功数（含自身）达到多数派返回 200；否则 503 `PARTIAL_COMMIT` 列出失败 peer，leader 本地保留已落盘状态。**相同字节的证书重试导入视为幂等**，只重新推送；同 `sequence` 不同内容 409。
5. **选主对账并写回：** 成为 leader 后向所有 peer 调 `GetLicense`，需收到 voter 多数派（含自身）的响应；按 3.2 规则选出最高状态；**然后把选中的状态同步推送到 voter 多数派并得到确认，才退出 `RECONCILING`**。对账中 `GET /license` 为 `RECONCILING`，导入与成员变更返回 503；**不向节点下发任何拒读原因**，节点保留现有授权快照并按本地时钟评估，授权层的读写行为不变（可用性仍受 master 不可用期间既有行为影响，例如 filer 拿不到新的 volume 位置）；未达多数派每 5 秒重试，不在请求路径。审核中的反例（A 的 `PARTIAL_COMMIT` 少数派状态 11 被新 leader B 选中并下发，A/B 随后不可用，C/D/E 只能恢复到 10）在 v4 里 B 必须先把 11 写到 C/D/E 中至少两台才退出对账，之后任何多数派都能看到 11。
6. follower 启动或重连 leader 时经 KeepConnected 首帧和按流对账拿到当前状态（4.2），按 3.2 替换。**P2 实现：** 改为 leader 每 5 秒向尚未确认当前授权修订的成员重推 `SyncLicense`（带任期校验），follower master 不从 KeepConnected 应用授权状态；KeepConnected 的 `LicenseUpdate` 只给 volume/filer（P3）。
7. **加入 voter 的两阶段流程（hashicorp）。** `SyncLicense` 要求接收方本地 raft 已认识当前 leader 且任期相同（第 3 条），全新节点在进入配置前拿不到这些状态，所以不能“先同步再加入”。顺序改为：(a) 三个加入入口先 `CheckMasterJoin()` 预占额度，然后以 **nonvoter** 加入（`AddNonvoter`）；(b) 等它的 raft 追平（`LastIndex` 接近 leader）并通过 KeepConnected 建流；(c) leader 在 manager 锁内向它 `SyncLicense` + `SyncLedger` 并等待确认，锁内同时排斥导入，因此“预同步完成到提升之间”不会漏掉新证书；(d) 确认后 `AddVoter` 提升。任一步失败保持 nonvoter 并告警，不计入 voter 多数派。`OnPeerUpdate` 与 `updatePeers` 的 `AddVoter` 改为走同一流程。
8. **移除 voter 前先重提交。** `RaftRemoveServer`（[master_grpc_server_raft.go:87](../weed/server/master_grpc_server_raft.go:87)）、`updatePeers` 的 `RemoveServer`（[raft_hashicorp.go:107](../weed/server/raft_hashicorp.go:107)）、`OnPeerUpdate` 的远程移除（[master_server.go:383](../weed/server/master_server.go:383)）都先调 `License.BeforeRemoveVoter(target)`：计算移除后的 voter 集合，把当前已确认的 `LicenseState` 与账本重新推送到该集合并要求其多数派确认，成功才执行移除，否则拒绝并告警。审核反例（11 在 A/B 提交，C 仍为 10，先移 B 再移 A）在 v6 里移 B 时要先让 {A, C} 多数派持有 11，C 因此追平。
   **临界区：** “重提交确认 → 实际移除完成”整个区间持有 manager 互斥锁，与导入、加入两阶段、其他移除串行（现有 `RaftRemoveServer` 只拿 `RaftServerAccessLock` 读锁，[master_grpc_server_raft.go:64](../weed/server/master_grpc_server_raft.go:64)，不能提供这项互斥）。执行移除前再次核对“授权状态版本与配置索引都等于准备时记录的值”，不等则放弃本次移除并返回 409 让调用方重试。审核交错（两个移除分别完成确认后、执行前提交了 11，再依次移除 B、A 只剩持 10 的 C）因此不会发生：第二个移除的准备阶段要等第一个移除执行完，而导入被锁排斥到移除之后。锁内每个 peer 的 RPC 截止 3 秒、整个流程截止 10 秒，超时视为失败。
9. **goraft（静态 `-peers`）模式的边界。** 成员只能靠重启改 `-peers`（[raft_server.go:188](../weed/server/raft_server.go:188) 的 `DoJoinCommand` 在启动时执行），没有可挂钩的加入/移除入口。该模式下：新 master 启动后经 KeepConnected 首帧拿到状态；manager 对每个 voter 记录已确认的状态版本 `voters_acked`，`GET /license` 与 `weed shell license.show` 展示；运维缩容前必须确认保留的 voter 都已确认当前状态，否则可能回退。这是运维约束，不是代码保证。
10. **绑定配置索引。** 导入提交与对账都记录开始时的 raft 配置索引（hashicorp `GetConfiguration().Index()`）；完成前配置变化则按新配置重新计算多数派并补推送，仍不足则 `PARTIAL_COMMIT`/继续 `RECONCILING`。
11. **`RECONCILING` 期间一律延后注册**（3.4）：授权状态与账本对账、接管转换完成前，`SendHeartbeat` 与 `KeepConnected` 对所有地址（含账本内旧地址）返回可重试错误，volume 按既有 `heartbeat()` 循环、filer 按既有重连循环在数秒后重试；对账完成后再开放。节点保留现有授权快照，授权层读写不变（证书在本地缓存并按本地时钟评估）；注册被延后的节点收不到首帧，也不需要收到任何拒读原因。这样恢复流程不会覆盖刚重连写入的 `online=true`，也避免落后的新 leader 按旧高额度接纳新地址。

### 3.4 计数口径与超额行为

SeaweedFS 没有 `ALTER SYSTEM ADD/DROP`，volume server 通过心跳自动注册、断开即注销；filer 通过 KeepConnected 注册。v3 的“只记录断连节点”在旧 leader 突然宕机时无法保护原在线节点（它们没有断连记录，新 leader 重建拓扑期间可能被新节点占额），v4 改为**成员账本**：

- 账本 `map[addr]{type, last_seen, online}` 记录所有已注册节点（在线与宽限中）。注册时写入；断连时 `online=false, last_seen=now`；宽限 10 分钟后删除。
- 额度已用数 = 账本条目数（在线 + 宽限）。已在账本内的地址重新注册不占新额度；账本外的地址才检查额度。
- **版本：** 快照版本是 `(term, revision)`，`term` 为生成该快照的 leader 当时的 raft 任期，`revision` 在任期内单调递增；比较按字典序。审核反例（旧 leader A 留下未确认的 v100，新 leader B 确认了 v91）在 v6 里是 `(t1,100)` 与 `(t2,91)`，`t2 > t1`，A 恢复后对账不会用旧任期的高修订覆盖。同版本不同内容：拒绝并告警（实现错误）；迟到重发的较低版本：忽略。
- **复制：** leader 在账本有变更时按批次（≤ 5 秒合并一次）把**全量快照**写入本地 `state.json`，并经 gRPC `SyncLedger` 推送到所有 master，要求 voter 多数派确认。**锁规则：** 后台复制 goroutine 在 manager 锁内只做“拷贝当前快照并分配 `(term, revision)`”，然后**释放锁再发 RPC**；收到确认后重新加锁，核对 leader 任期未变且该 `revision` 仍是最新已发版本才标记已确认，否则丢弃结果。注册路径与复制共用的锁因此只有微秒级持有时间，慢 voter 或不可达 voter 不会让集中重连的节点排队。只有导入、加入两阶段、移除前重提交、选主对账这四类低频操作在锁内同步等待 RPC（带 3.3 的截止时间）。**在途批次至多一个**：上一批未确认前，新变更只合并进内存账本，不生成新版本；确认或超时（5 秒）后再生成下一版本（全量，自然包含合并的变更）并发送。超时未确认的旧版本不再重发，它的内容已包含在新版本里。不经 KeepConnected 流复制（KeepConnected 可能落后数分钟而 raft 仍正常）。
- **选主时账本对账：** 新 leader 与授权状态对账同步进行，向所有 peer 调 `GetLedger`，收到 voter 多数派响应后按 `(term, revision)` 取最高（含自己），再以新任期生成 `(new_term, 1)` 写回 voter 多数派，才退出 `RECONCILING`。切主最多丢最近一个未经多数派确认的批次，不承诺固定秒数。
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

每个 `license.Node` 实例一个 500 ms ticker 维护原子时钟：有 master 锚点时取 `master_now_unix_ms + 自锚定以来的单调流逝`（按毫秒累计后取整秒），无锚点退回本地墙钟；不与本地墙钟取较大值。同一 ticker 按 3.2 推导生效证书并在变化时替换快照。

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
- 修复是带序号的显式操作：导入 `sequence` 更高且 `issued_at <= now` 的新证书时，leader 把 `watermark = issued_at`、`repair_seq = sequence` 持久化并经 `SyncLicense` 同步。**follower 三分支：** `repair_seq > 本地` 则无条件采用（允许降低水位）；`repair_seq == 本地` 则 `watermark = max`；`repair_seq < 本地` 忽略该请求的水位（v3 的“否则取 max”会让同任期迟到的低序号请求重新抬高水位）。重复导入现有证书不重置水位。
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
| `POST /license/validate` | `{"certificate":"<jws>"}` 或 multipart `file` | 每进程 10 次/分钟令牌桶 |
| `POST /license/import` | 落盘、同步、下发；返回 `{"accepted":true,"effective_now":bool,"state_version":n,"voters_ok":[...],"peers_failed":[...]}`；非 leader 或对账中 503 | 导入串行；相同字节幂等 |
| `DELETE /license/nodes/{addr}` | 删除宽限中的账本条目 | 不踢在线节点 |
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
| `weed/server/master_grpc_server.go` | `SendHeartbeat`：账本判新/预占/对账冻结 + 首帧与按流对账 + 响应填容量字段；`KeepConnected`：`addClient` 前预占 + 首帧 + ticker 对账；日志脱敏（P2 已完成对账冻结与日志脱敏；P3 已完成首帧与按流对账；预占 P4） | ~60 |
| `weed/server/master_grpc_server_license.go`、`master_license.go`（新） | `GetLicense/ImportLicense/SyncLicense/GetLedger/SyncLedger`；raft 适配、peer RPC、类型转换（P2 已完成） | ~380 |
| `weed/server/master_server_handlers_license.go`（新） | 5 个 HTTP handler（P2 已完成） | ~170 |
| `weed/cluster/cluster.go` | `ListAllClusterNodes`（对账时保留在线 filer）（P2 已完成） | ~20 |
| `weed/server/master_server_handlers_ui.go` | args 加 `License`（P2 已完成） | ~4 |
| `weed/server/master_ui/license.html`（新）、`master.html`、`masterNewRaft.html`、`templates.go` | 两份模板共用一个 License 区块（P2 已完成） | ~130 |
| `weed/wdclient/masterclient.go` | `OnLicenseUpdate` 回调；首帧、循环、`PermissionDenied license:` 三处调用（P3 已完成） | ~20 |
| `weed/server/volume_server.go` | `license *license.Node`；启动读缓存（P3 已完成） | ~6 |
| `weed/server/volume_grpc_client_to_master.go` | 处理 `in.License`、`in.LicenseRejectReason`（P3 已完成） | ~15 |
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
| `weed/stats/metrics.go` | 指标（P3 只做 `license_denied_reads_total`，其余随 P4/P5） | ~20 |
| `weed/command/master.go`、`weed/command/server.go`、`master_follower.go` | `-license.file`、`-master.license.file`、`-mdir` 临时目录告警（P2 已完成） | ~40 |
| `weed/shell/command_license.go`（新） | `license.show/import/validate`（P2 已完成） | ~140 |

合计：新包约 1,700 行（含测试约 2,700 行）；既有 Go 文件修改约 720 行，模板约 140 行，新增 Go 文件约 550 行。不改 raft 状态机、不改 webdav；v9 起 `filer.proto` 增加一个字段，volume/filer gRPC 在拒读态的返回按 1.2 G1/G2 变化；mount 只改 1.6 的读取错误传播。

### 7.3 签发工具（massdb-sql 仓库，另计）

`validate_claims` 把 `product` 与 limits 字段写死（[license_issuer.py:167](/Users/angrycucumbers/project/license/massdb-sql/tools/license-issuer/license_issuer.py:167)），`sign/verify` 都调用它。加产品档案 `--product massdb-oss`，limits 为三个 OSS 字段，`prepare-claims` 加 `--max-*`，签发测试加 OSS 用例。约 1 人天，P5 依赖。

### 7.4 开工前准备

- protoc v5.28.3、protoc-gen-go v1.34.2、protoc-gen-go-grpc v1.5.1（本机未安装，版本须与现有生成文件一致；v9 起 `master.proto` 与 `filer.proto` 都要重新生成，`filer.proto` 同步到 `other/java`）。
- 生产 kid 与公钥（发布前）；P1 用测试根。

---

## 8. 实施顺序与验收

| 阶段 | 工作 | 完成证据 | 估时 |
| --- | --- | --- | --- |
| P0 契约 | 冻结 claims、挂点、原因码、第 9 节决策 | **已确认（2026-10-13）**：[P0 契约](license-p0-contract-20261013.md) | 0.5 天 |
| P1 核心包 | `weed/license` 除 `manager.go` 外全部 + 单测（manager 依赖 proto 与 raft，并入 P2） | **已完成（2026-10-13）**：`go test -race ./weed/license/` 通过；签发工具 + OpenSSL 3 签发的互通向量验证通过；`AllowRead` 0 分配，单核约 0.65 ns/op；7.1 测试清单中 voter 多数派、任期、对账写回三项随 manager 在 P2 完成 | 3 天 |
| P2 master 管理 | proto（先行）、manager、HTTP/gRPC、voter 多数派、任期与配置索引、对账写回、账本 `(term,revision)` 复制与对账、对账期间注册延后、加入两阶段、移除前重提交、`voters_acked`、cluster_id、令牌、水位、页面 | **已完成（2026-10-13）**，证据与差异见 8.1：`go test -race ./weed/license/` 通过（含 19 个 manager 用例）；`server/command/shell/cluster/topology/wdclient/pb` 测试通过；真实进程验证单 master、goraft 与 hashicorp 3 master、3→5 扩容与缩容、切主对账、页面导入、`license.init` 与 `-license.file`、日志无证书与令牌 | 6 天 |
| P3 下发与拦截 | 按流对账、volume/filer 守卫、M1、G1（含 `filer.proto`、合并规则、客户端改动）、G2、缓存、内部令牌传递链、mount 错误传播、S3 两处 | **已完成（2026-10-14）**，证据与差异见 8.2：S1–S4、S6、S15–S20 真实进程通过（mount 与 `-cipher` 在 Linux 虚拟机上补测；mount 硬链接与截断的上游缺陷已修复）；S5、S7、S8 属 master 侧，P2 已验证；新增 server/filer/mount/wdclient/pb/license 单测通过 | 6.5 天 |
| P4 节点额度 | 账本、宽限、预占、`NODE_REJECTED`（volume 与 filer）、三个 master 加入入口、无证书额度 0 与 master 启动检查 | S9–S12、S21 | 2.5 天 |
| P5 验收与交付 | `go vet`、`go test ./weed/...`、Benchmark 命令、`CGO_ENABLED=0` 构建、`weed server` 一体化、docker-compose、运维手册 | 全部场景记录；S13 | 2 天 |

合计约 20.5 人天（单人，不含签发工具 1 人天）。v9 增量见 P0 契约 8.1。

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
4. 对账的接管转换之后，把当前仍连在本 leader 上的 volume 与 filer 重新置为在线（`Connected` 回调），覆盖“同一 master 失去又重获 leader、流未断开”时连接不会重新注册的情况。
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
4. 新 filer 引导遇到隐藏条目时每 5 秒重试，直到拿到令牌；元数据聚合在事件进入聚合日志与回放之前检查，遇到隐藏条目断开并从该事件重新订阅（不只在 Replay 内拒收）。
5. 指标只实现了 `license_denied_reads_total{server,path}`（prometheus 名带仓库统一前缀，为 `SeaweedFS_license_denied_reads_total`），`license_state`、`license_expires_at_seconds` 与节点数量类指标随 P4/P5。
6. `filer.Entry.ContentHidden` 在 HTTP JSON 里的字段名是 `ContentHidden`（与该结构其它字段一样为大驼峰）。

**未覆盖：** `volume.check.disk` 只在副本间有差异时才读 needle，本次集群无副本，未触发（`ReadNeedleBlob` 守卫已由探测客户端验证）。`CacheRemoteObjectToLocalCluster` 未接远端存储，只有代码路径。S14（新旧混部）未测。被拒准入（`NODE_REJECTED`）的节点侧处理已实现，master 侧在 P4 接入。

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
| 7 | 签发工具 | massdb-sql 加产品档案 | 独立复制 |
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
- **voter 加入两阶段、移除前重提交并与导入共用临界区、goraft 模式的 `voters_acked` 运维检查，提交与对账绑定 raft 配置索引**（3.3）。
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
- **运维手册**：导出申请 → 签发 → 导入 → 续期 → 降额与宽限释放 → 存量集群升级（`license.init` → `-license.file`）→ 状态目录恢复 → 开启 gRPC mTLS → 故障排查。
