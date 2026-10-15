# MassDB OSS 授权证书 P0 契约（已冻结）

日期：2026-10-13。源码基线：`f2fcc241e`（= `dd3b853cc` + 执行计划文档，Go 代码与 `dd3b853cc` 无差异）。对应[执行计划 v9](license-certificate-execution-plan-20261013.md)。

**状态：已确认。** 第 9 节决策由用户于 2026-10-13 确认，其中决策 2、4、9 改选备选方案；其余按默认。本文冻结证书声明、原因码、线格式常量、源码挂点与决策，P1–P5 以本文为准；与执行计划冲突时以本文为准并回改计划。

P0 只交付契约与核对记录，不含实现与运行测试。

---

## 0. 结论摘要

| 项 | 冻结结果 |
| --- | --- |
| 证书格式 | 复用 MassDB SQL 协议 v1 的 JWS 与解析规则，产品档案改为 `MassDB OSS`（第 1 节） |
| 原因码 | 9 个状态码（其中 7 个是读拒绝原因）、17 个管理错误码、注册结果 3 类、数据面错误码 `CONTENT_HIDDEN`（第 2 节） |
| 挂点 | 计划 v8 的 53 处源码引用全部核对（行号漂移 2 处），修订 5 处（第 7 节）；新增 G1、G2 两组挂点（第 4、5 节） |
| 决策 2 | **改为限制，对齐 MassDB SQL**：从未导入证书时 volume、filer 一个都不能注册，master 只允许单 master 新集群引导，其余加入都需证书；新增离线初始化与启动导入，支持先取证后部署/升级（5.1） |
| 决策 4 | **改为封堵**：volume gRPC 中返回数据正文的 7 个 RPC 加守卫，volume↔volume 维护调用经 gRPC 元数据携带内部令牌放行（5.2） |
| 决策 9 | **改为封堵**：filer gRPC 返回的 `Entry.Content` 在拒读态隐藏并置 `content_hidden`，`UpdateEntry` 保留原正文，系统目录豁免，mount 对隐藏正文返回 `EIO` 并限频重取（5.3） |
| 其余决策 | 1、3、5、6、7、8、10、11 按默认 |

---

## 1. 证书声明

### 1.1 载体

| 项 | 规则 |
| --- | --- |
| 编码 | compact JWS，三段，ASCII；每段为规范、无 padding 的 base64url（解码后重新编码必须逐字节相同） |
| 保护头 | 精确字段集 `{"alg","typ","kid"}`；`alg="Ed25519"`，`typ="massdb-license+jws"`；`kid` 为非空文本、≤ 128 UTF-16 代码单元、不含固定空白与禁用码点。拒绝 `jku`、`jwk`、`x5u`、`crit`、`b64`、`alg=none`、`EdDSA` 等一切其它头 |
| 签名 | Ed25519，64 字节；验签输入是原始 `segment0 + "." + segment1` 字节，不重新序列化 |
| 大小 | 证书原文 ≤ 64 KiB（65536 字节） |
| 信任根 | `weed/license/trust.go` 内嵌签发工具 `export-trust` 产出的清单：`{"schema_version":1,"keys":[{"kid","purpose","public_key_spki"}]}`，1–32 个 key，`kid` 全局唯一，`public_key_spki` 为规范 base64url 的 44 字节 Ed25519 SPKI（前缀 `302a300506032b6570032100`）。OSS 只使用 `purpose="license"` 的 key，其它 purpose 在加载时忽略 |
| 测试根 | 仅经 `-ldflags "-X github.com/seaweedfs/seaweedfs/weed/license.testTrustManifest=<base64url(清单 JSON)>"` 注入，与内嵌清单合并；注入时启动打 `glog.Warning`，`GET /license` 返回 `"trust_root":"test"`。单测通过包内 `_test.go` 设置，不暴露导出函数 |

### 1.2 JSON 解析（保护头与声明共用）

严格 UTF-8；顶层必须是对象；拒绝重复键；键名 ≤ 128 UTF-16 代码单元；嵌套深度 ≤ 8；字符串 ≤ 4096 UTF-16 代码单元且无孤立代理；数字只接受整数，词法长度 ≤ 20 字符，拒绝小数、指数、`NaN/Infinity`；布尔不能冒充整数。Go 实现用 `encoding/json.Decoder` 的 token 流自行检查重复键与数字词法（`UseNumber`），不引入新依赖。

### 1.3 声明字段（精确字段集，未知字段一律拒绝）

| 字段 | 类型与范围 | OSS 取值 |
| --- | --- | --- |
| `schema_version` | 整数，= 1 | 1 |
| `policy_version` | 整数，= 1 | 1 |
| `license_id` | 文本 ≤ 256，不允许任何空白 | 签发方分配 |
| `issuer`、`customer_id`、`edition` | 文本 ≤ 256，允许中间空白，首尾不允许 | 签发方分配 |
| `product` | 精确字符串 | **`"MassDB OSS"`** |
| `deployment_id` | 规范小写 UUID（`8-4-4-4-12`，36 字符） | 集群 `cluster_id` |
| `issued_at`、`not_before`、`expires_at` | 整数 `[0, 253402300799]`（UTC 秒）；`issued_at <= expires_at` 且 `not_before < expires_at` | |
| `sequence` | 整数 `[1, 9223372036854775807]` | 同一部署严格递增 |
| `features` | 数组，≤ 128 个不重复文本，各 ≤ 128，不允许空白 | 读授权需含 **`"DATA_READ"`**；未知能力忽略；空数组合法但不授予读 |
| `limits` | 对象，精确字段集 `{"max_master_nodes","max_volume_servers","max_filers"}`，各为整数 `[1, 2147483647]` | 0、缺失、负数都不表示无限制 |

文本规则沿用协议 v1 的固定码点表（与签发工具 `FORBIDDEN_TEXT_RANGES`、`WHITESPACE_RANGES` 及每平面末两个 noncharacter 完全一致），不做 Unicode 归一化，按码点精确比较。长度一律按 UTF-16 代码单元计。

### 1.4 申请 DTO（`GET /license/deployment`）

精确字段集，`registered_*` 为账本条目数（在线 + 宽限），无证书时也导出：

```json
{"schema_version":1,"product":"MassDB OSS","deployment_id":"<uuid>","registered_master_nodes":3,"registered_volume_servers":7,"registered_filers":2}
```

### 1.5 master 导入规则（验签之外）

按顺序检查，第一个失败即返回对应错误码（2.3），状态不变：

1. 大小、JWS、JSON、字段约束 → `LICENSE_MALFORMED`；`kid` 不在信任根 → `LICENSE_UNTRUSTED_KID`；验签失败 → `LICENSE_SIGNATURE_INVALID`。
2. `product != "MassDB OSS"` → `LICENSE_PRODUCT_MISMATCH`；`deployment_id != cluster_id` → `LICENSE_DEPLOYMENT_MISMATCH`。
3. `expires_at <= now` → `LICENSE_EXPIRED`（不接受已过期证书，避免高序号过期证书顶掉仍有效的证书）。
4. 与 `newest` 比较：字节相同 → 幂等成功（只重新推送，不重置水位）；`sequence` 相同字节不同 → `LICENSE_SEQUENCE_CONFLICT`；`sequence` 更低 → `LICENSE_SEQUENCE_NOT_INCREASING`。
5. 新证书自身的三类额度分别 ≥ 当前账本条目数，否则 `LICENSE_NODE_LIMIT_TOO_SMALL`（P1 修订：它生效后只有它的额度适用；`min(prev, newest)` 只用于等待期的准入，不用于导入校验）。
6. 缺少 `DATA_READ` 不拒绝导入，状态为 `FEATURE_MISSING`。
7. master 不检查 `customer_id` 连续性与 `license_id` 唯一性（签发方登记与 `sign --previous` 负责）。

### 1.6 OSS v1 不使用的协议 v1 部分

- 时钟修复文档 `typ="massdb-license-clock-repair+jws"` 与挑战流程：OSS 用“导入更高序号且 `issued_at <= now` 的证书”修复（计划 4.5），不接受修复文档。
- `purpose="time_repair"` 的信任 key。

### 1.7 签发工具（独立项目；2026-10-14 修订）

原定在 massdb-sql 的 `license_issuer.py` 上加 `--product massdb-oss` 档案，现改为：massdb-sql 的签发工具只作测试用（`weed/license/testdata` 的互通向量），不加 OSS 档案；生产签发工具放在另一个独立项目（项目待定），按本节的声明、文本规则与信任清单格式实现协议 v1 与 OSS 产品档案，要求见计划 7.3。生产 kid 与公钥由它产出，是发布的前置条件，不阻塞 P5 的功能验收。

---

## 2. 原因码

### 2.1 授权状态（`GET /license` 的 `status`，节点快照的 `reason`）

| 码 | 出现位置 | 含义 |
| --- | --- | --- |
| `VALID` | master、节点 | 生效证书在有效窗口内且含 `DATA_READ`，无拒读原因 |
| `NO_LICENSE` | master、节点 | 从未导入证书（节点：已收到首帧且 `newest` 为空） |
| `NOT_YET_VALID` | master、节点 | `now < newest.not_before` 且 `prev` 为空 |
| `EXPIRED` | master、节点 | 生效证书 `expires_at <= now` |
| `FEATURE_MISSING` | master、节点 | 生效证书在窗口内但缺 `DATA_READ` |
| `CLOCK_SUSPECT` | master、节点 | master 墙钟 < 水位 − 5 s；经 `master_deny_reason` 下发 |
| `RECONCILING` | **仅 master** | 选主对账未完成；不下发节点（计划 3.3 第 5、11 条） |
| `NOT_READY` | **仅节点** | 无缓存且未收到首帧 |
| `NODE_REJECTED` | **仅节点** | 本节点注册因额度被拒；volume 经 `license_reject_reason`、filer 经 `PermissionDenied` 得知 |

节点拒读原因的优先级（多个同时成立时取第一个）：`NODE_REJECTED` > `NOT_READY` > `CLOCK_SUSPECT` > `NO_LICENSE` > `NOT_YET_VALID` > `EXPIRED` > `FEATURE_MISSING`。master 的 `status` 在 `RECONCILING` 时优先显示 `RECONCILING`，其余同上（去掉两个仅节点码）。P1 修订：`NODE_REJECTED` 排到 `NOT_READY` 之前，否则首次心跳即被拒、没有缓存的新节点会显示 `NOT_READY`，与 S9 期望的 `NODE_REJECTED` 不符。

### 2.2 读拒绝的对外表现

| 出口 | 表现 |
| --- | --- |
| volume HTTP GET（V1）、filer HTTP GET（F1）、`?proxyChunkId=`（F2） | HTTP 403，`Content-Type: application/json`，头 `X-MassDB-License-Reason: <码>`，body 精确为 `{"error":"license_denied","reason":"<码>","expires_at":<生效证书 expires_at，无则 0>,"cluster_id":"<id，未知则空串>"}`，body 按快照预序列化 |
| S3 `GetObject` | 标准 S3 `AccessDenied`（403 XML），保留 `X-MassDB-License-Reason` 头 |
| S3 `CopyObject` 源读取 | `InvalidCopySource` |
| volume gRPC 数据 RPC（G2，5.2） | `codes.PermissionDenied`，消息 `license: <码>` |
| filer gRPC 返回的 Entry（G1，5.3） | 不报错；`Content` 清空、`content_hidden=true`、`FileSize` 不变 |
| HTTP `?metadata=true` 与目录 JSON（M1） | 200；`Content` 清空、`content_hidden=true`，头 `X-MassDB-Content-Hidden: true` |
| mount 读 | `EIO`（分块文件、manifest 文件、隐藏正文的内联文件） |

### 2.3 管理接口错误码

HTTP body `{"error":"<码>","message":"<不含证书原文的说明>"}`；gRPC 返回对应 status，消息以 `license: <码>` 开头。

| HTTP | gRPC | 码 | 场景 |
| --- | --- | --- | --- |
| 400 | InvalidArgument | `LICENSE_MALFORMED` | JWS/JSON/字段约束失败，或请求体缺证书 |
| 400 | InvalidArgument | `LICENSE_UNTRUSTED_KID` | `kid` 不在信任根 |
| 400 | InvalidArgument | `LICENSE_SIGNATURE_INVALID` | 验签失败 |
| 400 | InvalidArgument | `LICENSE_PRODUCT_MISMATCH` | `product` 不是 `MassDB OSS` |
| 400 | InvalidArgument | `LICENSE_DEPLOYMENT_MISMATCH` | `deployment_id` 与集群不符 |
| 400 | InvalidArgument | `LICENSE_EXPIRED` | 导入时 `expires_at <= now` |
| 413 | InvalidArgument | `REQUEST_TOO_LARGE` | 请求体 > 96 KiB |
| 409 | FailedPrecondition | `LICENSE_SEQUENCE_NOT_INCREASING` | `sequence` 低于 `newest` |
| 409 | FailedPrecondition | `LICENSE_SEQUENCE_CONFLICT` | 同 `sequence` 不同字节 |
| 409 | FailedPrecondition | `LICENSE_NODE_LIMIT_TOO_SMALL` | 新额度低于账本条目数 |
| 409 | FailedPrecondition | `MEMBERSHIP_CHANGED` | 移除 voter 执行前授权版本或配置索引已变化，调用方重试 |
| 409 | FailedPrecondition | `NODE_ONLINE` | `DELETE /license/nodes/{addr}` 指向在线节点 |
| 404 | NotFound | `NODE_NOT_FOUND` | 账本内无该地址 |
| 429 | ResourceExhausted | `RATE_LIMITED` | `validate` 超过 10 次/分钟 |
| 503 | Unavailable | `NOT_LEADER` | 非 leader；body 附 `leader` 地址（可能为空） |
| 503 | Unavailable | `RECONCILING` | 选主对账中 |
| 503 | Unavailable | `PARTIAL_COMMIT` | 未达 voter 多数派；body 附 `voters_ok`、`peers_failed` |

`validate` 与 `import` 共用 1.5 的检查与错误码，成功时返回 200 和摘要（不含原文），不落盘、不推送。

P2 修订：请求体除 `{"certificate":"<jws>"}` 与 multipart `file` 外也接受证书原文；`NOT_LEADER` 的 `leader` 是 leader 的 HTTP 地址；移除 voter 前的重提交未达剩余 voter 多数派时返回 `PARTIAL_COMMIT`。

### 2.4 注册结果

| 场景 | volume（`SendHeartbeat`） | filer（`KeepConnected`） | master 加入 |
| --- | --- | --- | --- |
| 超额（含无证书时额度 0） | 先发 `HeartbeatResponse{license_reject_reason:"NODE_LIMIT_EXCEEDED"}`，再返回 `PermissionDenied`，消息 `license: NODE_LIMIT_EXCEEDED volume_server <used>/<limit>`；volume 置 `NODE_REJECTED` 并按既有循环重试 | `PermissionDenied`，消息 `license: NODE_LIMIT_EXCEEDED filer <used>/<limit>`；masterclient 置 `NODE_REJECTED` 并按既有循环重试 | `RaftAddServer` 返回错误 `license: NODE_LIMIT_EXCEEDED master <used>/<limit>`；`OnPeerUpdate`、`updatePeers` 只打 warn |
| `RECONCILING` | 返回 `Unavailable`，消息 `license: RECONCILING`；节点**不改**授权状态，按既有循环重试 | 同左 | `RaftAddServer` 返回 `license: RECONCILING` |
| 成功 | 首帧带 `LicenseUpdate`（同时填 `VolumeSizeLimit`、`Preallocate`、`Leader`） | 首帧带 `LicenseUpdate` | — |

`NODE_REJECTED` 的解除：volume 收到不带 `license_reject_reason` 的 `LicenseUpdate`；filer 的 KeepConnected 建流成功并收到首帧。

P4 修订：

- 准入地址：volume 用心跳里的 `ip:port`（与拓扑 `DataNode.Url()` 相同），filer 用 `KeepConnectedRequest.client_address`；只有 `ClientType == "filer"` 计数。
- 拒绝帧只填 `license_reject_reason`，volume 收到后置 `NODE_REJECTED` 并跳过该帧的卷参数比较（不填 `VolumeSizeLimit`、`Preallocate`，避免被当作参数变化）。节点只在状态由未拒绝变为拒绝时打一条 error 日志，之后的重试只有既有的 V(0) 连接失败日志；master 的拒绝日志每分钟至多 1 条，附带这一分钟内被合并的次数。
- 断开计时：master 按地址记录本 leader 上存活的注册流数，最后一条流结束才把账本条目置为离线并开始宽限；已离线条目再次断开不重置 `last_seen`；不再是 leader 的 master 只减计数、不改本地账本副本（以 leader 的 `SyncLedger` 为准）。这样快速重连时旧流晚于新流关闭，不会把在线节点误置为宽限。

### 2.5 数据面错误码

| 码 | 出现位置 | 含义 |
| --- | --- | --- |
| `CONTENT_HIDDEN` | filer gRPC `CreateEntry/UpdateEntry` 返回 `FailedPrecondition`，消息 `license: CONTENT_HIDDEN`；客户端辅助函数返回 `filer_pb.ErrContentHidden` | 条目正文因拒读被隐藏，无法据此写入或输出（5.3） |

---

## 3. 线格式与常量

| 项 | 冻结值 |
| --- | --- |
| 能力名 | `DATA_READ` |
| 内部令牌 HTTP 头 | `X-MassDB-Internal-Read: <标准 base64，含 padding，44 字符>`，解码后必须恰为 32 字节 |
| 内部令牌 gRPC 元数据键 | `x-massdb-internal-read`，值同上（5.2） |
| 拒绝原因头 | `X-MassDB-License-Reason` |
| 隐藏正文头 | `X-MassDB-Content-Hidden: true` |
| `master.proto` | `HeartbeatResponse.license_reject_reason = 8`、`HeartbeatResponse.license = 9`、`KeepConnectedResponse.license = 3`（当前最大字段号分别为 7、2，已核对）；新增消息与 RPC 见计划 4.1 |
| `filer.proto` | `Entry.content_hidden`（字段号见 5.3） |
| 状态文件 | master `<mdir>/license/state.json`；volume 第一个 `-dir` 下 `license-cache.json`；filer `-defaultStoreDir` 下 `license-cache.json`；均为写临时文件 + `fsync` + `rename` |
| 无证书时的准入 | volume 0、filer 0；master 只允许 `-peers` ≤ 1 的新集群引导，动态加入 0（5.1） |
| 宽限 | 10 分钟 |
| 节点时钟 | 500 ms ticker；锚点为 `LicenseUpdate.master_now_unix_ms`（P1 修订为毫秒）加锚定后的单调流逝，不早于本地墙钟减 5 分钟（2026-10-14 修订，计划 4.3） |
| 账本批次 | ≤ 5 秒合并一次，在途至多一个，超时 5 秒 |
| 锁内 RPC 截止 | 单 peer 3 秒、整体 10 秒 |
| 时钟可疑阈值 | 墙钟 < 水位 − 5 秒；水位每 60 秒落盘 |
| 重试间隔 | mount 失败态重解析/重取 ≥ 5 秒；`RECONCILING` 未达多数派每 5 秒重试 |
| 限流 | `validate` 每进程 10 次/分钟；拒绝日志每节点每分钟 1 条 |
| 请求体上限 | 96 KiB；证书 64 KiB |
| 指标 | `license_state{reason}`、`license_expires_at_seconds`、`license_denied_reads_total{server,path}`（`path` ∈ `volume_http/filer_http/filer_proxy/volume_grpc/filer_grpc_hidden`）、`license_nodes_online{type}`、`license_nodes_grace{type}`、`license_nodes_limit{type}` |
| raft 任期 | hashicorp 用 `Raft.CurrentTerm()`（v1.7.2 提供，替代计划 v8 的 `Stats()["term"]` 解析）；goraft 用 `Server.Term()` |
| raft 配置索引 | hashicorp `GetConfiguration().Index()`；goraft 无，记 0 |

P3 修订：`internal_read_key` 按第 7 节“key 分发”的规则只在 volume 心跳与 filer 的 KeepConnected 流里下发（P2 实现漏发给 volume，P3 修正）。指标在 P3 只实现 `license_denied_reads_total`，prometheus 名为 `SeaweedFS_license_denied_reads_total`（仓库统一前缀），其余随 P4/P5。

P4 修订：其余 5 个指标在 master 上导出，每 5 秒按本 master 的 `GET /license` 视图刷新：`SeaweedFS_license_state{reason}`（当前状态为 1，其余 6 个 master 状态码为 0）、`SeaweedFS_license_expires_at_seconds`（生效证书的 `expires_at`，无证书为 0）、`SeaweedFS_license_nodes_{online,grace,limit}{type}`（`type` ∈ `master/volume_server/filer`；master 的 `online` 是 raft 成员数，`grace` 恒为 0）。volume 与 filer 只导出 `license_denied_reads_total`：`weed server` 同进程共用一个注册表，节点再导出 `license_state` 会与 master 的值冲突。

---

## 4. 源码挂点（基线行号）

“锚点”是挂点处现有代码的一行，P3 开工前按锚点文本复核行号漂移。G1、G2 的完整清单见 5.2、5.3。

### 4.1 读守卫

| ID | 位置 | 锚点 | 规则 |
| --- | --- | --- | --- |
| V1 | `weed/server/volume_server_handlers_read.go:53` | 49 行 `if !vs.maybeCheckJwtAuthorization(r, vid, fid, false) {` 之后、54 行 `needle.NewVolumeId(vid)` 之前 | 仅 GET；有效态直接放行；拒读态校验 `X-MassDB-Internal-Read`。两个入口 `privateStoreHandler`（`volume_server_handlers.go:70`）、`publicReadOnlyHandler`（`:157`）都经此处；`ReadMode=proxy` 转发时复制全部请求头，内部令牌随之传递 |
| F1 | `weed/server/filer_server_handlers_read.go:161` | `if checkPreconditions(w, r, entry) {` 之前 | 仅 GET；拒绝时删除可能已设的 `X-Seaweedfs-Is-Directory-Key`（139 行）。覆盖 partNumber（166）、图片缩放 `filer.ReadAll`（245）、内联正文（258-260）、流式读（283） |
| F2 | `weed/server/filer_server_handlers.go:66` | `fs.proxyToVolumeServer(w, r, fileId)` 之前 | 仅 GET（代理按原方法转发，`filer_server_handlers_proxy.go:47`） |
| M1a | `weed/server/filer_server_handlers_read.go:124` | 目录分支 `if query.Get("metadata") == "true" {` | 隐藏规则同 5.3（含系统豁免），另加响应头 |
| M1b | `weed/server/filer_server_handlers_read.go:147` | 文件分支 `if query.Get("metadata") == "true" {`，写出在 157 行 | 同上；`resolveManifest=true`（149）经内部令牌解析 manifest，只返回 chunk 列表 |
| M1c | `weed/server/filer_server_handlers_read_dir.go:63` | `if r.Header.Get("Accept") == "application/json" {` | 同上，逐条目处理 |
| G1 | 见 5.3 | | filer gRPC 6 处隐藏点 + `KvGet` |
| G2 | 见 5.2 | | volume gRPC 7 个 RPC 入口 |

### 4.2 内部读取通道（计划 1.4）

| ID | 位置 | 锚点 | 改动 |
| --- | --- | --- | --- |
| I1 | `weed/filer/filechunk_manifest.go:111` | `fetchWholeChunk` 内 `retriedStreamFetchChunkData(bytesBuffer, urlStrings, "", ...)` | 传令牌头 |
| I2 | `weed/filer/filechunk_manifest.go:127` | `func retriedStreamFetchChunkData(` | 增加 `extraHeader` 参数，传到 `ReadUrlAsStreamAuthenticated` |
| I3 | `weed/util/http/http_global_client_util.go:293`、`:354` | `ReadUrlAsStreamAuthenticated` 与加密分支 `readEncryptedUrl` | 两处都在 `maybeAddAuth` 旁加 extraHeader；新增 `ReadUrlAsStreamWithHeader` |
| I4 | `weed/filer/stream.go:354` | `ChunkStreamReader.fetchChunkToBuffer` 内 `util_http.ReadUrlAsStream(` | 改用 `ReadUrlAsStreamWithHeader` 带令牌 |
| I5 | `weed/filer/stream.go:153-154` | `StreamContent` 调 `PrepareStreamContent(..., noJwtFunc, ...)` | 经私有 `prepareStreamContent` 传令牌头；公开的 `PrepareStreamContentWithThrottler`（`:83`，filer HTTP 用户路径 `filer_server_handlers_read.go:283`）传空 |
| I6 | `weed/filer/internal_token.go`（新） | — | 包级 `SetInternalReadToken`，`atomic.Value` |

调用方审计（v9 核对）：带令牌的三个函数在可承载 filer 的进程内只有内部调用方——`fetchWholeChunk` 只经 manifest 解析（`ResolveOneChunkManifest`）；`ChunkStreamReader` 只经 merge（`filer_server_handlers_write_merge.go:40`）、日志回放（`filer_notify_read.go:326`）及 `NewFileReader`（仅 `filer.replicate` sink 与 `filer.remote.sync` 独立进程）；`StreamContent`/`ReadEntry` 只经 filer.conf、IAM API 与 shell/`filer.cat`（独立进程）。用户读路径都不带令牌：filer HTTP 流式读（`PrepareStreamContentWithThrottler`）、图片缩放（`filer.ReadAll` → `util_http.RetriedFetchChunkData`）、webdav 与 mount（`fetchChunkRange`）。manifest 块经令牌读取只暴露 chunk id 列表。

### 4.3 master

| ID | 位置 | 锚点 | 改动 |
| --- | --- | --- | --- |
| MS1 | `weed/server/master_grpc_server.go:132` | `SendHeartbeat` 内 `if dn == nil {` | 注册前：`RECONCILING` 延后、账本判新与预占（在 `GetOrCreateDataNode` 前）；注册成功那条 `VolumeSizeLimit` 响应带首帧；之后每次心跳比较版本 |
| MS2 | `weed/server/master_grpc_server.go:276` | `ms.addClient(req.FilerGroup, req.ClientType, peerAddress)` 之前 | `RECONCILING` 延后；`ClientType=="filer"` 预占；`addClient` 后发首帧；325 行 ticker 分支比较版本；321 行日志只打类型与版本；key 只给 filer |
| MS3 | `weed/server/master_grpc_server_raft.go:51`、`weed/server/master_server.go:372`、`weed/server/raft_hashicorp.go:101` | 三处 `AddVoter` | 加入两阶段 + `CheckMasterJoin` |
| MS4 | `weed/server/master_grpc_server_raft.go:87`、`weed/server/raft_hashicorp.go:107`、`weed/server/master_server.go:383` | 三处移除 | `BeforeRemoveVoter`，manager 锁覆盖到移除完成 |
| MS5 | `weed/server/master_server.go` 的 `if !ms.option.DisableHttp` 路由块 | — | `/license*` 5 条路由 |

P3 修订：MS2 的授权首帧在 `addClient` 之后、卷位置之前单独发送（`KeepConnectedResponse` 只填 `license`），没有 volume 时客户端也能收到首帧。MS1 带授权的心跳响应填 `VolumeSizeLimit` 与 `Preallocate`，不填 `Leader`（leader 自己的响应在上游从不带 `Leader`）。

### 4.4 volume、filer、客户端

| ID | 位置 | 锚点 | 改动 |
| --- | --- | --- | --- |
| VS1 | `weed/server/volume_server.go:112` | `vs.checkWithMaster()` 之前 | 创建 `license.Node`，读缓存 |
| VS2 | `weed/server/volume_grpc_client_to_master.go:133` | `volumeOptsChanged := false` 之前 | 处理 `in.License`、`in.LicenseRejectReason`。注意 134 行无条件比较 `Preallocate`，带 license 的响应必须填当前值；`VolumeSizeLimit`（136，非 0 才用）与 `Leader`（150，非空才用）也照填 |
| FS1 | `weed/server/filer_server.go:159` | `fs.checkWithMaster()` 之前 | 创建 `license.Node`、读缓存、注册回调、`saveToFilerLimit>0` 告警 |
| WC1 | `weed/wdclient/masterclient.go:226`、`:248` | 首帧与循环里的 `if resp.VolumeLocation != nil {` | 两处调用 `OnLicenseUpdate`；KeepConnected 返回 `PermissionDenied` 且消息以 `license:` 开头时置 `NODE_REJECTED`，`Unavailable` + `license: RECONCILING` 不改状态 |
| MT1 | `weed/mount/filehandle.go:78-81` | `filer.NewChunkGroup(` 与其后的 `Warningf` | 记录 `resolveManifestErr` 与时间戳 |
| MT2 | `weed/mount/filehandle_read.go:57` | `if offset < int64(len(entry.Content)) {` 之前 | manifest 失败态与 `ContentHidden` 返回 `EIO`，≥ 5 秒限频重解析/重取 |
| S3a | `weed/s3api/s3api_object_handlers_copy.go:91` | `util_http.DownloadFile(srcUrl, ...)` 之后 | 非 200/206 返回 `ErrInvalidCopySource` |
| S3b | `weed/s3api/s3api_object_handlers.go:251` | `func passThroughResponse(` | 有 `X-MassDB-License-Reason` 时写 `ErrAccessDenied` |

---

## 5. 改选决策的冻结设计

### 5.1 决策 2：无证书时不允许加入节点（对齐 MassDB SQL）

MassDB SQL 的规则是“无可信额度仅允许已证明的新集群首 FE 引导；增加 BE 需要证书”（massdb-sql 执行计划 5.1 额度表“引导”行）。映射到 SeaweedFS：master 对应 FE，volume 与 filer 对应 BE。

| 项 | 规则 |
| --- | --- |
| 适用条件 | `newest` 为空（从未导入，或 `<mdir>/license/` 全部丢失）。证书过期不算无证书，沿用最后一张证书的额度（计划 3.4） |
| volume、filer | 额度 0：每个新地址的心跳/KeepConnected 都按 2.4 返回 `NODE_LIMIT_EXCEEDED`，消息注明 `no license imported`；节点按既有循环重试，导入后在下一次重试时注册 |
| master 动态加入 | 额度 0：`RaftAddServer`、`OnPeerUpdate`、`updatePeers` 的 `AddVoter` 全部拒绝 |
| master 启动检查（新增） | 在 raft 引导/加入之前，用本地状态判定：取 `<mdir>/license/state.json` 的 `newest`，或 `-license.file`（本地按 1.5 第 1–3 条校验，且 `deployment_id` 等于本地 `cluster_id`）。**无证书时只允许 `-peers` ≤ 1**；有证书时要求 `-peers` 数 ≤ `max_master_nodes`。不满足则进程以 `license: NODE_LIMIT_EXCEEDED master <peers>/<limit>` 退出，并提示 `weed license.init` 流程 |
| 已有成员 | hashicorp 已有 raft 配置的成员重启（非 `-raftBootstrap`）不做上一行检查，只计数，不踢（对应 MassDB SQL“已注册成员不因到期或额度被移除”）。goraft 默认每次启动清空 raft 日志与配置（`raft_server.go:121-126`），无法区分新建与重启，因此 goraft 多 master 每次启动都要求本地已有证书 |
| 首次部署 | 单 master：直接启动 → `GET /license/deployment` → 签发 → 导入 → volume/filer 自动注册。多 master：任一台执行 `weed license.init -mdir=<dir>` 得到 `cluster_id` 与申请 DTO，其余各台 `license.init -clusterId=<同一 id>`；签发后各台以 `-license.file` 启动 |
| 存量集群升级 | 升级后的 master 没有证书时，**所有** volume 与 filer 都无法重新注册，写入完全中断，直到导入。因此升级必须先取证：用新版二进制对各 master 的 `<mdir>` 执行 `license.init`（多 master 用同一 id），签发后以 `-license.file` 启动升级后的 master；leader 在退出 `RECONCILING`、开放注册之前提交该证书（计划 3.4） |
| 恢复 | 状态目录全部丢失时，用证书里的 `deployment_id` 执行 `license.init -clusterId=<id>`，再以 `-license.file` 启动。丢失状态不会换来免费额度（无证书时额度为 0） |
| `weed server` 单进程 | 内置 master 可启动，内置 volume 与 filer 在导入前注册被拒；评估与开发环境也需要证书（可签发短期试用证书） |
| 运维告警 | `<mdir>` 位于 `os.TempDir()` 之下时 master 启动打 warn（`-mdir` 默认值就是临时目录） |
| 边界 | `-clusterId` 与手改 `state.json` 等价，集群绑定只防误用、不防有意复制 |

P2 修订：每个 master 在 raft 启动前把 `-license.file` 按 1.5 第 1–5 条（额度按本地账本，master 数由启动检查判定）应用到自己的 `state.json`，已应用或被更高序号取代的文件跳过；启动检查与选主对账都基于这份本地状态。

### 5.2 决策 4：volume gRPC 数据读守卫（G2）

守卫范围是“把用户数据字节返回给调用方”的 RPC，按 3.84 全量盘点（`volume_server.proto` 的 `VolumeServer` 服务共 42 个 RPC）只有下列 7 个：

| RPC | 实现 | 守卫条件 | 被拦后的影响（证书无效时） |
| --- | --- | --- | --- |
| `CopyFile` | `volume_grpc_copy.go:319` | `req.Ext` **不在**元数据白名单 `{".idx",".ecx",".ecj",".vif",".cpx"}`（精确匹配）时检查 | 无影响：`.dat`/`.ec00–.ec13` 只由 volume→volume 拉取，带令牌；shell `volume.fsck`、`volume.check.disk` 只取 `.idx`/`.ecx` |
| `ReadNeedleBlob` | `volume_grpc_read_write.go:11` | 总是检查 | shell `volume.check.disk` 失败（字节经过 shell 进程） |
| `ReadAllNeedles` | `volume_grpc_read_all.go:10` | 总是检查 | 构建内无调用方 |
| `VolumeTailSender` | `volume_grpc_tail.go:17` | 总是检查 | 无影响：只由 `VolumeTailReceiver`（volume→volume，`operation/tail_volume.go:35`）调用，带令牌 |
| `VolumeIncrementalCopy` | `volume_grpc_copy_incremental.go:13` | 总是检查 | 独立命令 `weed backup` 失败 |
| `VolumeEcShardRead` | `volume_grpc_erasure_coding.go:332` | 总是检查 | 无影响：只由 `store_ec.go:308` volume→volume 调用，带令牌 |
| `Query` | `volume_grpc_query.go:12` | 总是检查 | 构建内无调用方 |

规则：

1. 守卫写在这 7 个 handler 的入口（流式 RPC 只在建流时检查一次，已开始的流不因到期中断），**不加 gRPC 拦截器**，其余 35 个 RPC 零改动（H11 保持）。
2. 判定：`AllowRead()` 为真直接放行；否则读 incoming metadata `x-massdb-internal-read`，按计划 1.4 第 3 条同样的规则（key 已初始化、解码 32 字节、恒时比较、`NOT_READY` 一律拒绝）放行或返回 `codes.PermissionDenied`，消息 `license: <码>`。
3. volume→volume 的 3 个出站调用点用 `metadata.AppendToOutgoingContext` 附加令牌（本地 key 未初始化时不附加）：`volume_grpc_copy.go:207`（`doCopyFileWithThrottler`，覆盖 `VolumeCopy` 与 `VolumeEcShardsCopy`）、`operation/tail_volume.go:35`（经 `TailVolumeFromSource` 增加令牌参数）、`storage/store_ec.go:308`。令牌只加在这 3 处的 RPC ctx 上，不放进共享连接缓存，`weed server` 同进程的 shell 拿不到。
4. `store_ec.go:308` 的附加是 **H1 的必要条件**：EC 卷的 HTTP DELETE、`BatchDelete`、`VolumeNeedleStatus` 都会经 `ReadEcShardNeedle` 读远端分片，不附加令牌会让证书无效时 EC 卷删除失败。
5. 不拦的维护面：shell 的 move/balance/copy/fix.replication/ec.*/tier.* 都只发编排 RPC，字节在 volume 之间流动；`ReadNeedleMeta`、`VolumeNeedleStatus`、`ReadVolumeFileStatus` 只返回元数据；`VolumeTierMoveDatToRemote` 把数据写到运维配置的云端，等同运维持有磁盘（1.3 磁盘文件边界）。
6. 剩余边界：任何能对 master 打开 `KeepConnected` 并自称 `ClientType="filer"`、且有空闲 filer 名额的 gRPC 客户端能拿到 `internal_read_key`（`ClientType` 不鉴权）；未启用 gRPC mTLS 与 HTTPS 时令牌明文传输。gRPC mTLS 与 HTTPS 不作为部署要求，理由与开启方式见计划 1.3（2026-10-14 修订）。
7. 盘点中发现的上游路径穿越漏洞（与证书无关）已在当前分支先行修复，见第 7 节；G2 的扩展名白名单与之共用 `isCopyFileExtAllowed`。

P3 修订：

- 上表 `CopyFile` 行“shell `volume.fsck`、`volume.check.disk` 只取 `.idx`/`.ecx`”对 `volume.fsck` 不完整：它还在 shell 进程内经 volume HTTP 解析 manifest（无令牌）。上游遇到解析失败只打印一行并继续，会把 manifest 引用的数据块报告为孤儿（加 `-reallyDeleteFromVolume` 会删除在用数据）；P3 改为解析失败即中止并报错，证书无效且存在 manifest 文件时 `volume.fsck` 不可用。
- `volume.check.disk` 只在副本之间有差异时才调用 `ReadNeedleBlob`；副本一致时证书无效也能完成。
- volume HTTP 在 `ReadMode=proxy` 下转发沿用原方法（上游把 HEAD 改成 GET），HEAD 经代理也不受证书影响（第 1 节“保留”）。

### 5.3 决策 9：filer gRPC 内联正文隐藏（G1）

目标：证书无效时，经 filer gRPC 也读不到内联正文，同时不让任何回写、复制、备份流程因“读到空正文”而清空或伪造数据。

**协议。** `filer.proto` 的 `Entry` 增加 `bool content_hidden = 13;`（当前最大字段号 12；字段 6 未用且未声明 reserved，不复用）。`other/java/client/src/main/proto/filer.proto` 同步。`weed/filer.Entry` 增加 `ContentHidden bool`（JSON `omitempty`），在 `FromPbEntry` 与 `ToExistingProtoEntry` 间映射。**服务端存储、事件日志、KV 硬链接 blob 里永远不出现 `content_hidden=true`**（写入时清零，见下）。

**隐藏条件**（全部成立才隐藏）：`!AllowRead()`；请求没有有效内部令牌（gRPC 元数据 `x-massdb-internal-read`，流式 RPC 在建流时判定一次）；`len(Content) > 0`；条目不在系统豁免内。隐藏动作只作用在发往客户端的那份 proto 上：`Content = nil`，`ContentHidden = true`，若 `Attributes.FileSize == 0` 则补为原正文长度。不得在 `ToExistingProtoEntry` 层隐藏（它同时用于事件日志与存储编码）。

**系统豁免**（这些路径的正文是配置，S3/IAM/filer/mount/MQ 依赖经 gRPC 读取，隐藏会让 S3 以无身份、鉴权关闭的状态启动等；按条目实际的完整路径规范化后判断，2026-10-14 修订、2026-10-15 补充：查询会解析拼接后路径中的 `.` 与 `..`，目录 `/etc/../data`、或目录 `/etc` 加名字 `../data/a`，实际读取的都是 `/data` 下的条目，不得按 `/etc` 豁免）：

| 豁免 | 读取方 |
| --- | --- |
| `/etc/` 下全部条目 | `/etc/iam/identity.json`、`/etc/iam/policies.json`（S3、IAM API、shell）；`/etc/s3/circuit_breaker.json`；`/etc/seaweedfs/filer.conf`（各 filer 热加载、mount WORM、S3 生命周期、shell）；`/etc/remote/mount.mapping`、`/etc/remote/<name>.conf`（filer、`filer.remote.*`、shell `remote.*`） |
| `/topics/` 下名为 `topic.conf` 或以 `.offset` 结尾的条目 | MQ broker、consumer group、`mq.topic.compact` |

**服务端隐藏点（G1）：**

| 挂点 | 位置 |
| --- | --- |
| `LookupDirectoryEntry` | `server/filer_grpc_server.go:35-37` |
| `ListEntries` | `server/filer_grpc_server.go:61-63` |
| `TraverseBfsMetadata` | `server/filer_grpc_server_traverse_meta.go:39-42` |
| `SubscribeMetadata`、`SubscribeLocalMetadata` | 共用 `eachEventNotificationFn` 的发送点 `server/filer_grpc_server_sub_meta.go:279-285`（`OldEntry` 与 `NewEntry` 都处理；每个订阅者每个事件都是新反序列化的对象，可原地改） |
| `StreamRenameEntry` | `server/filer_grpc_server_rename.go:186` |
| `CacheRemoteObjectToLocalCluster` | `server/filer_grpc_server_remote.go:194`（远端缓存对象通常无正文，为完整性纳入；同进程 HTTP 调用只用 chunks） |
| `KvGet`（旁路） | `server/filer_grpc_server_kv.go:9-23`：拒读态且无令牌时，只允许键前缀为 `Meta`、`sync.`、`remote.sync.` 且值 ≤ 8 字节（filer.sync、remote sync、`cluster.ps` 的同步偏移），其余返回 `PermissionDenied`。原因：硬链接 blob 是含正文的完整 Entry，键 `hard_link_id` 经 Lookup/List 可见；leveldb v1 的条目与 KV 共用键空间 |

**filer 之间的内部调用必须带令牌**（filer 已持有 key）：`filer/meta_aggregator.go:183`（订阅包括自身在内各 filer 的 `SubscribeLocalMetadata`，供 peer Replay 到非共享存储与 filer.conf 热加载）、`filer/filer.go:82-101` 新 filer 引导的 `TraverseBfsMetadata`。防御：`filer/meta_replay.go` 的 Replay 与引导写库前遇到 `ContentHidden` 条目一律报错重试，不入库（覆盖 key 尚未到达时的启动竞态）。

**写入合并规则**（`CreateEntry` 与 `UpdateEntry` 的 gRPC handler，在调用 `filer.CreateEntry/UpdateEntry` 之前）：

| 来者 | 处理 |
| --- | --- |
| `content_hidden=false` | 原样（现有行为） |
| `content_hidden=true` 且 `Content` 非空 | 客户端显式给了新正文，按来者写入 |
| `content_hidden=true`、`Content` 为空、已有条目存在且 chunks 与 `FileSize` 都与已有条目相同 | 纯元数据更新（S3 标签、`touch`、mount chmod/xattr/硬链接、Java HDFS 属性更新）：**保留已有 `Content`** |
| `content_hidden=true`、`Content` 为空、无 chunks 且 `FileSize > 0`，且不满足上一行 | 字节未知（隐藏条目被复制到新位置或内容已变），返回 `FailedPrecondition`，消息 `license: CONTENT_HIDDEN` |
| 其它（chunks 变化，或截断到 0） | 客户端改写了数据（mount 写入、`copy_file_range`、截断），按来者写入，旧正文丢弃。与上游一致：mount 写内联文件本就直接丢弃原正文（`mount/weedfs_file_write.go:60`），隐藏不引入新的数据丢失 |

所有分支写库前把 `content_hidden` 清为 false。chunks 比较只在 `content_hidden=true` 时进行，有效态零成本。

**客户端改动：**

| 消费方 | 改动 |
| --- | --- |
| `pb/filer_pb_tail.go:63`（`WithFilerClientFollowMetadata`，全部订阅方的唯一入口） | `MetadataFollowOption` 增加 `FailOnHiddenContent`；为真时 `NewEntry.ContentHidden` 的事件返回 `ErrContentHidden`，调用方按既有循环从上次偏移重试 |
| 落盘或转发正文的 5 个命令：`filer.sync`、`filer.backup`、`filer.meta.backup`、`filer.remote.sync`、`filer.remote.gateway` | 置 `FailOnHiddenContent=true`：证书无效时停在该事件（与决策 3 一致），续期后从原偏移继续，不在目标端留下空文件 |
| 不消费正文的订阅方：mount、S3（`/etc/` 与 buckets 目录）、`filer.meta.tail`、`fs.verify` | 保持默认 false |
| `filer.ReadInsideFiler`、`filer.ReadEntry`（`filer/read_write.go`） | 条目 `ContentHidden` 时返回 `ErrContentHidden`，不再返回空字节或补零字节 |
| shell `fs.cat`、`weed filer.cat` | 同上，报错而不是输出补零内容 |
| shell `fs.meta.save`、`fs.meta.notify`、`filer.meta.backup` 的首轮遍历 | 遇到 `ContentHidden` 条目报错退出，不产出缺正文的转储或通知 |
| mount 读（`mount/filehandle_read.go:57` 之前） | 条目 `ContentHidden` 时返回 `EIO`；失败态下距上次 ≥ 5 秒才用 `LookupDirectoryEntry` 重取，未隐藏则替换句柄条目并写回 meta cache 后正常读（single-flight，与 1.6 的 manifest 重解析共用节流）。原因：mount meta cache 与打开的句柄会无限期保留隐藏条目，续期不产生事件 |

**仍是边界：** `other/java`（不在构建内）的读取方读到空正文；Java 客户端回写依赖 protobuf-java 保留未知字段（需在使用 Java 客户端的部署中验证）。能经 filer HTTP 写 `/etc/` 的用户可把内联正文放在豁免路径下经 gRPC 读回（`/etc` 写入在 filer 强制内联，`filer_server_handlers_write.go:113-115`）。证书有效期间已建立的订阅流在到期后的事件开始隐藏，到期前已发出的事件不追回。

P3 修订：

- 隐藏判定：`!AllowRead()` 在每次 RPC 或每条事件时判断；令牌在流内第一次需要时读取一次。
- 元数据聚合（`meta_aggregator.go`）在事件写入聚合日志与 Replay 之前检查隐藏条目，遇到即断开并从该事件重新订阅；新 filer 引导遇到隐藏条目每 5 秒重试，直到拿到令牌。
- `weed filer.cat` 出错时打印到 stderr 并以 1 退出（上游吞掉全部错误）；`fs.meta.save` 遇到隐藏条目时删除未完成的转储文件；`filer.meta.backup` 首轮遍历报错退出。
- webdav 读入口对隐藏条目返回 `ErrContentHidden`（上游 webdav 不读 `Entry.Content`，内联文件本就返回全零，这一上游缺陷未改）。
- mount：来自 meta cache 的隐藏条目在新句柄上首次读取立即重取一次，之后 ≥ 5 秒一次；mount 写入丢弃内联正文（上游行为）后清除隐藏标志。2026-10-15 修订（计划 8.9）：`copy_file_range` 同样清除隐藏标志。句柄上截短隐藏正文后又扩长回原大小时，按写入合并规则第三行 filer 会补回被截掉的字节；因此截到的最小长度小于当前大小时，mount 在 flush 时返回 `EIO`（`CONTENT_HIDDEN`），不发送（大小不同于原值时 filer 本就拒绝）；续期重取的正文从该长度起补零。重取应答回来时句柄若已不是发起时的状态（截断到 0、装入新条目），结果整个丢弃（计划 8.10）。
- 8.2 的 S20 中“mount 硬链接后正文完整”“mount 截断到 0 得到空文件”原受上游缺陷限制（基线版本的 mount 对内联文件做硬链接即返回 `EIO` 并清空原正文，`truncate -s 0` 对内联文件不生效），P3 已修复并验证。写入合并规则补充：带 `content_hidden` 新建的名字不存在已有条目但带 `hard_link_id` 时，以共享的硬链接数据作为已有条目；隐藏正文的文件截断到非 0 长度返回 `CONTENT_HIDDEN`。
- 边界补充：master 的 gRPC `GetLicense(include_state=true)`（master 之间对账用）不鉴权，能连 master 的任何客户端都可取得 `internal_read_key`；filer 按 `notification.toml` 发往外部队列的事件不隐藏内联正文。2026-10-14 修订：前者已加固，master 之间的 `GetLicense(include_state=true)`、`GetLedger`、`SyncLicense`、`SyncLedger` 只接受来源 IP 属于 raft 成员、`-peers` 或 `[master.license] peer_ips` 的调用，其余返回 `PermissionDenied`（计划 1.3、8.4）；gRPC mTLS 不作为部署要求。

---

## 6. 第 9 节决策记录（2026-10-13 用户确认）

| # | 问题 | 确认结果 |
| --- | --- | --- |
| 1 | 节点计数口径 | 默认：成员账本（在线 + 10 分钟宽限），全量快照复制 |
| 2 | 从未导入证书时是否限制注册 | **改选：限制，对齐 MassDB SQL**：volume、filer 额度 0，master 只允许单 master 新集群引导；配套 `license.init`、`-license.file` 与 master 启动检查（5.1） |
| 3 | `filer.sync`、`filer.backup`、`filer.remote.sync` | 默认：证书无效时拦截（含 5.3 的隐藏正文停在事件处） |
| 4 | volume gRPC 原始读 | **改选：封堵**，7 个 RPC 入口守卫 + volume↔volume 令牌（5.2）；不用拦截器 |
| 5 | `HEAD` 与元数据接口 | 默认：放行（内联正文按 M1/G1 隐藏） |
| 6 | 管理接口访问控制 | 默认：依赖厂商签名；白名单不视为防护 |
| 7 | 签发工具 | **2026-10-14 改选：独立项目**，不改 massdb-sql；massdb-sql 的签发工具只作测试用（1.7） |
| 8 | `-license.file` | 默认：做；因决策 2 成为升级路径的必需项 |
| 9 | `Entry.Content` | **改选：封堵**，`content_hidden` + 写入合并规则 + 系统豁免 + 客户端改动（5.3） |
| 10 | 内部令牌轮换接口 | 默认：不做 |
| 11 | 成员账本复制批次 | 默认：≤ 5 秒合并、voter 多数派确认 |

决策 2 于 2026-10-13 二次确认：由“引导额度 1/1/1”改为对齐 MassDB SQL 的“无证书不能加入节点”。“豁免路径取 `/etc/` 与 `/topics/` 下的 `topic.conf`、`*.offset`”是本契约按用户选择补定的具体值，可在 P1 开工前调整。

---

## 7. 核对记录

| 项 | 结果 |
| --- | --- |
| 基线 | `f2fcc241e` 与 `dd3b853cc` 之间只有计划文档，Go 代码无差异 |
| 计划 v8 的 53 处源码链接 | 全部逐行核对。行号漂移 2 处：`filer_server_handlers_read.go:146` 实为 147；`master_grpc_server.go:319` 是 `case`，日志在 321。另有未链接引用 `masterclient.go:248` 正确 |
| 遗漏 | 目录条目的 `?metadata=true`（124 行）也返回 `entry`，S3 目录标记对象的正文从这里可读；补为 M1a |
| 简化 | F1 前移到 `checkPreconditions` 之前并显式限 GET，拒绝路径不再清理对象头 |
| 方法范围 | F2 的 `proxyChunkId` 代理按原方法转发写入，守卫限 GET |
| key 分发 | master 对所有 KeepConnected 客户端广播同一流（不区分 `ClientType`）；计划 4.2 原文会把 `internal_read_key` 发给 shell/broker。改为只在 volume 心跳与 `ClientType=="filer"` 流里填 |
| raft API | hashicorp v1.7.2 有 `AddNonvoter`、`CurrentTerm()`、`LastIndex()`、`GetConfiguration().Index()`；seaweedfs/raft v1.1.3 有 `Term()`、`Peers()`。任期改用 `CurrentTerm()` |
| 静态 master 组建 | hashicorp 首次启动 `BootstrapCluster` 一次写入全部 `-peers`（`raft_hashicorp.go:179-186`），不经过加入入口；`updatePeers` 只在已有配置重启时运行（`:69`）。goraft 默认每次启动清空 raft 日志与配置（`raft_server.go:121-126`），由 `isTheFirstOne` 的 master 执行 `DoJoinCommand`（`command/master.go:226-228`）。两种模式都要在 raft 启动前加本地证书检查才能阻止无证书的多 master 组建（5.1） |
| 心跳响应字段 | volume 侧 `Preallocate` 无条件比较（`volume_grpc_client_to_master.go:134`），带 license 的响应必须填 |
| `-mdir` 默认 | `os.TempDir()`（`command/master.go:70`），状态可能随重启丢失 |
| proto 字段号 | `HeartbeatResponse` 最大 7、`KeepConnectedResponse` 最大 2、`filer.proto Entry` 最大 12，计划使用的 8/9/3/13 均空闲；`master.proto` 无同名 RPC |
| 生成工具链 | 生成文件为 protoc v5.28.3、protoc-gen-go v1.34.2、protoc-gen-go-grpc v1.5.1；本机三者都未安装（P2 前置：`go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2`、`go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1`，protoc 28.3 官方发布包） |
| 内部令牌调用方审计 | 见 4.2 末段，通过 |
| volume gRPC 盘点 | 42 个 RPC，7 个返回数据正文；volume↔volume 出站数据调用 3 处；无任何 gRPC 拦截器；shell 中只有 `volume.check.disk` 让数据经过 shell 进程，独立命令只有 `weed backup` |
| `Entry.Content` 盘点 | 6 个 RPC 返回 Entry 外加 `KvGet` 旁路；配置类正文全部在 `/etc/{iam,s3,seaweedfs,remote}` 与 `/topics/` 的 `topic.conf`、`*.offset`；filer 之间（含自身）经 gRPC 订阅与引导；全部订阅客户端经 `pb/filer_pb_tail.go:63` 一个入口 |
| 上游漏洞（已修复） | volume 端从不校验 collection 名，它被直接拼进文件路径：`CopyFile` 的 `ext` 与 EC 分支 `collection` 可读主机任意文件；`VolumeCopy` 采用源节点返回的 collection（源地址由请求指定）、`VolumeEcShardsCopy` 采用请求 collection，可在目标机任意位置写文件；`VolumeEcShardsDelete` 可删任意位置的 `.ec*/.ecx/.ecj/.vif`；`AllocateVolume`（master `/dir/assign?collection=` 可触发）可在任意位置建卷文件。修复：`storage.ValidateCollectionName`（拒绝 `/`、`\`、NUL）挂在 `Store.addVolume`、`Store.MountEcShards`、`VolumeCopy`、`VolumeEcShardsCopy/Delete/Rebuild`、`CopyFile` EC 分支；`CopyFile` 增加扩展名白名单（卷 `.dat/.idx/.vif`，EC `.ec00–.ec13/.ecx/.ecj/.vif`）。单测覆盖，`storage/...`、`server/...` 全部通过 |
| 上游缺陷（记录） | `RaftRemoveServer` 非 Force 时按 `master@id` 查客户端（`master_grpc_server_raft.go:80`），与 `addClient` 的键格式 `filerGroup.clientType@addr` 不一致，检查不生效；不在本计划修复 |

---

## 8. 对 P1–P5 的影响

### 8.1 改动量与估时

| 决策 | 新增改动 | 估时 |
| --- | --- | --- |
| 2 | 无证书额度 0（~10 行）、master 启动前本地证书检查（~40 行，两种 raft 模式）、`weed license.init`（~80 行）、`-license.file` 接管顺序、`-mdir` 告警 | +1.5 天（P2/P4） |
| 4 | 7 个 RPC 入口守卫与 gRPC 令牌校验（~60 行）、3 处出站附加令牌（~20 行）、单测 | +1 天（P3） |
| 9 | `filer.proto` 字段与 Go/Java 生成、`filer.Entry` 映射、6 处隐藏点与 `KvGet`、写入合并规则、filer 间令牌与 Replay/引导防御、订阅选项与 5 个命令、读取辅助与 cat/meta 命令、mount `EIO` 与重取（合计 ~250 行 + 测试） | +2.5 天（P3） |

合计由约 15.5 人天增至约 **20.5 人天**（不含签发工具；签发工具为独立项目，另计）。新包外的既有文件修改由约 310 行增至约 760 行；H2、H11 的措辞按 8.3 调整。

### 8.2 新增或修改的验收场景

| # | 场景 | 期望 |
| --- | --- | --- |
| S17（改） | 内联正文 | 预置 `saveToFilerLimit` 内联文件、S3 尾斜杠 ≤ 1024 字节对象、gRPC `CreateEntry` 带正文的条目：无证书时 HTTP `?metadata=true`（文件与目录两个分支）、目录 JSON、gRPC `LookupDirectoryEntry/ListEntries/TraverseBfsMetadata/SubscribeMetadata/StreamRenameEntry` 均 `Content` 为空、`content_hidden=true`、`FileSize` 不变；`/etc/iam/identity.json`、`/etc/seaweedfs/filer.conf`、`/etc/remote/*`、`/topics/**/topic.conf`、`*.offset` 不隐藏；mount 读内联文件 `EIO`；续期后保持打开的句柄在 ≤ 5 秒重试后可读 |
| S19 | volume gRPC 原始读（决策 4） | 无证书：不带令牌调用 7 个 RPC（`CopyFile` 取 `.dat`）返回 `PermissionDenied license: NO_LICENSE`；`CopyFile .idx` 成功；`volume.fsck` 成功；`volume.move/balance/fix.replication`、`ec.encode/rebuild/decode/balance` 成功；EC 卷 HTTP DELETE 成功；`volume.check.disk` 与 `weed backup` 报 license 错误；带正确令牌放行，错长度/错值拒绝，`NOT_READY` 一律拒绝；证书有效时全部不变 |
| S20 | 隐藏正文的回写与复制（决策 9） | 无证书时对隐藏的内联文件做 S3 打标签、CopyObject REPLACE、mount chmod/setxattr/硬链接：续期后正文完整；mount 截断到 0 得到空文件；`filer.sync`/`filer.backup`/`filer.meta.backup`/`filer.remote.sync` 停在该事件并报 `CONTENT_HIDDEN`，续期后从原偏移续传，目标端无空文件；`fs.meta.save`、`fs.cat`、`filer.cat` 报错；含隐藏条目的转储 `fs.meta.load` 不清空已有正文、对不存在的条目返回 `CONTENT_HIDDEN`；第二个非共享存储的 filer 回放 peer 事件后正文完整；新 filer 引导后正文完整；S3 重启后 IAM 鉴权生效；filer.conf 修改热加载生效；`KvGet` 读硬链接 id 被拒、同步偏移键可读 |
| S21 | 无证书准入与升级（决策 2） | 无证书：单 master 正常启动并可导出申请、导入证书；第 1 个 volume、第 1 个 filer 注册即被拒 `NODE_LIMIT_EXCEEDED`（filer 冷启动卡在等待 master），导入后在下一次重试时注册成功；3 master `-peers` 无证书启动时 hashicorp 与 goraft 都以 `license: NODE_LIMIT_EXCEEDED master 3/0` 退出；`license.init` + `-license.file` 后 3 master 正常组建；带证书时 `-peers` 超过 `max_master_nodes` 拒绝启动；hashicorp 已有配置的 master 在证书丢失后重启不被拒；动态 `RaftAddServer` 无证书被拒；存量 3 master + 7 volume + 2 filer 按 `license.init` → 签发 → `-license.file` 升级，全程无注册被拒；删除全部 `<mdir>/license/` 后用 `license.init -clusterId=<证书 deployment_id>` + `-license.file` 恢复；`weed server` 无证书时内置 volume/filer 被拒、导入后自动注册；`-mdir` 在临时目录时启动打 warn |

### 8.3 热路径约束调整

- H1：diff 范围增加 G2 的 7 个 RPC 入口与 3 个出站调用点、G1 的 6 处隐藏点与 `KvGet`、filer gRPC `CreateEntry/UpdateEntry` 的合并规则（只在 `content_hidden=true` 时生效）。写路径仍无新增调用：合并规则在来者未带标志时只多一次布尔判断。
- H2 改为：gRPC 元数据路径有效态每次 RPC 或每条事件多一次 `AllowRead()`，无复制、无分配；拒读态仅对 `len(Content) > 0` 的条目做豁免判断与置空。
- H11 保持：不加 gRPC 拦截器，守卫写在具体 handler 内。
- S13 增加负载组：拒读态大目录 gRPC `ListEntries`（10 万条，含 1% 内联条目）、拒读态 `SubscribeMetadata` 回放 100 万条事件；有效态这两组的 A/B 差异须在噪声带内。
