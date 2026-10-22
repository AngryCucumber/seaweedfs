# MassDB OSS 安装包

MassDB Manager 部署中心使用的安装包（技术来源 SeaweedFS），与 MassDB SQL 的安装包同形：按角色分目录，每个角色有启停脚本与一个启动参数文件。规范来自 Manager 计划《MassDB OSS 部署中心、安装包、配置管理与授权中心执行计划》3.1 节，2026-10-21 在 O1 定稿。

## 产物

| 目标 | 产物 |
| --- | --- |
| `make massdb-package` | `massdb-oss-<版本>-linux-<arch>.tar.gz`（种类 `massdb-oss`；大卷构建带 `_large_disk`） |
| `make massdb-meta-package` | `massdb-oss-meta-<Cassandra 版本>.tar.gz`（种类 `massdb-oss-meta`，元数据库，技术来源 Apache Cassandra） |

两者都是单一顶层目录（与包名相同）的 tar.gz，文件属主记为 `root:root`，旁边有 `.sha256`。默认输出到仓库下的 `dist/`（git 忽略）。

包名与顶层目录不带提交号（2026-10-21 用户决定）。版本号固定沿用 3.84（决策 D2），所以不同构建的包同名，区分构建要看 `package.json` 的 `commit`（或包的 sha256）。注意 Manager Agent 按顶层目录名解包到 `releases/<顶层目录>`，同名而校验值不同的包会原地替换该目录。

```bash
make massdb-package ARCH=arm64 TRUST=<信任清单 JSON 或 base64url 文件>
make massdb-meta-package CASSANDRA_TARBALL=apache-cassandra-5.0.8-bin.tar.gz JMX_EXPORTER_JAR=jmx_prometheus_javaagent-1.6.0.jar
```

`massdb-package` 的参数：

| 参数 | 说明 |
| --- | --- |
| `VERSION` | 可选；必须等于代码版本（`weed/util/constants.go`，现为 3.84）。版本号沿用上游，不另注入（决策 D2） |
| `ARCH` | `arm64`、`amd64` 或两者（默认两者）；`CGO_ENABLED=0` 交叉编译 linux，`-s -w -extldflags -static`，与上游发布构建相同 |
| `TRUST` | 测试信任根：签发中心导出的信任清单，JSON 原文或其 base64url 串均可，按 `weed/license` 的规则校验后经 `-ldflags -X …/weed/license.testTrustManifest=` 注入。生产信任清单（`weed/license/trust.go`）为空时必须给；生产清单非空时暂不支持再叠加测试根 |
| `LARGE_DISK=1` | 以 `5BytesOffset` 构建，包名带 `_large_disk`；与普通构建的卷数据不通用 |
| `ALLOW_DIRTY=1` | 工作区有未提交改动时仍出包，`util.COMMIT` 与 `package.json` 的提交号带 `-dirty` |
| `OUT` | 输出目录 |

提交号注入为完整提交号（`util.COMMIT`），`weed version` 与 master `/dir/status` 显示 `30GB 3.84 <提交号>`。出包时用 `package/tools/pkginfo check-binary` 确认 `package.json` 的 `trust` 原样出现在二进制里（平台的包检查用同样的办法证明信任根）。

## 布局

```text
massdb-oss-3.84-linux-<arch>/
  package.json
  bin/weed                         带授权的 weed（CGO_ENABLED=0）
  bin/massdb-oss-role.sh           启停公共逻辑，由各角色脚本引用
  master/bin/start_master.sh  stop_master.sh
  master/conf/master.conf  master.toml
  volume/bin/start_volume.sh  stop_volume.sh
  volume/conf/volume.conf
  filer/bin/start_filer.sh  stop_filer.sh
  filer/conf/filer.conf  filer.toml
  s3/bin/start_s3.sh  stop_s3.sh
  s3/conf/s3.conf
```

运行时产生：`<角色>/bin/<角色>.pid`；未设 `logdir` 时 `<角色>/log/`。S3 访问密钥文件 `s3.json`（filer 内嵌 S3 用 `filer/conf/s3.json`，独立 S3 用 `s3/conf/s3.json`）由平台渲染，不随包发布；缺少时对应进程启动失败，不会以无认证方式开放 S3。格式与现场 Ansible 的 `s3.json` 相同，现场文件（含 `anonymous` 只读身份）原样放进来即可用。

### package.json

```json
{
  "schemaVersion": 1,
  "kind": "massdb-oss",
  "product": "MassDB OSS",
  "version": "3.84",
  "commit": "<完整提交号>[-dirty]",
  "os": "linux",
  "arch": "arm64",
  "largeDisk": false,
  "licenseSupported": true,
  "trust": "<二进制内置的信任清单>",
  "trustEncoding": "ldflags-base64url",
  "roles": ["master", "volume", "filer", "s3"]
}
```

- `trust`、`trustEncoding`：测试根包为注入的 base64url 串与 `ldflags-base64url`；生产根包为 `trust.go` 里的 JSON 原文与 `embedded-json`。两种情况下 `trust` 都原样存在于 `bin/weed` 中。
- `commit` 与运行中节点报告的提交号相同，平台据此判断「是否同一构建」，不比较新旧（决策 D2）。

## 启停脚本约定

`start_<角色>.sh [--daemon]`：

- 读 `<角色>/conf/<角色>.conf`：一行一个 `key = value`，按第一个 `=` 拆分，键值两侧空白与行尾 `\r` 去掉；`#` 开头的行与空行跳过；值原样传给 weed（不做引号、变量展开），多值按 weed 的写法用逗号连接，布尔写 `true`/`false`；值为空的键不传（用 weed 默认值）；同一键出现两次、键不合法或行不是 `key = value` 时报错退出。
- 每个键转成 `-key=value`。`logdir v vmodule logtostderr alsologtostderr stderrthreshold log_backtrace_at config_dir` 是 weed 的全局参数，放在子命令之前，其余放在子命令之后：`weed -logdir=… -config_dir=… master -port=…`。
- 未设 `logdir` 时用 `<角色>/log`；未设 `config_dir` 时用 `<角色>/conf`。脚本导出 `WEED_CONFIG_DIR` 并传 `-config_dir`，两者指向同一目录（weed 的参数库会把 `WEED_<参数名>` 环境变量当作命令行没给的参数，所以单独的环境变量也生效；显式传参是为了在命令行上可见）。注意：同样的机制下，环境里其它 `WEED_<参数名>` 变量也会被 weed 当作参数，手工启动时不要残留。
- 工作目录切到角色目录，conf 里的相对路径（如 `s3.config = conf/s3.json`）以它为起点。
- 启动前检查必需的数据目录已设置且存在：master 的 `mdir`、volume 的 `dir`（逗号分隔逐个检查）、filer 的 `defaultStoreDir`。脚本不创建数据目录，避免挂载点没挂上时写到系统盘；日志目录会自动创建。
- `--daemon`：`setsid nohup` 后台启动（没有 `setsid` 的系统只用 `nohup`），标准输出与标准错误追加到 `<logdir>/<角色>.out`（每次启动先写一行带时间的命令行），pid 写 `<角色>/bin/<角色>.pid`；等 `START_CHECK_SECONDS`（默认 3）秒确认进程还在，否则删 pid 文件、打印 `.out` 末尾并返回 1。
- 不带 `--daemon`：写 pid 文件后 `exec weed`，前台运行，pid 不变。
- pid 文件里的进程仍存活且是本角色的 weed（可执行文件名为 `weed`、第一个非参数词是本角色子命令；读 `/proc/<pid>/cmdline`，没有时用 `ps -ww`）时拒绝启动并返回 1；进程已不在、pid 被别的进程复用或文件内容无效时，按残留清理后照常启动。

`stop_<角色>.sh [--timeout 秒]`：向 pid 文件里的进程发 SIGTERM，等待退出后删 pid 文件并返回 0；等待默认 volume 60 秒（weed 先停心跳，等 `-preStopSeconds` 再关闭），其它 30 秒，`STOP_TIMEOUT` 环境变量或 `--timeout` 可改；超时返回 1 且不发 SIGKILL（由调用方决定）。未在运行、pid 文件残留或 pid 被复用时清理 pid 文件并返回 0，不向别的进程发信号。

Manager Agent 的 deployer 以运行用户执行 `current/<角色>/bin/start_<角色>.sh --daemon`、读 `current/<角色>/bin/<角色>.pid`、停止时先跑停止脚本再按需 SIGTERM/SIGKILL，与以上约定一致。

## 默认参数

包内 `<角色>.conf` 取现场 Ansible 的参数与目录，端口按决策 D8；平台部署时改写其中的平台管理键（各文件头部列出），同时作为配置模板库的「Ansible 标准」默认模板。

| 角色 | 端口（gRPC 为 +10000） | 现场参数 | 目录 |
| --- | --- | --- | --- |
| master | 9333，metrics 9380 | `volumePreallocate=true`、`volumeSizeLimitMB=5000`、`defaultReplication=000`（3 台及以上 volume 时平台改为 001） | `mdir=/data/massdb-oss-storage/massdb-oss-master` |
| volume | 9340，metrics 9381 | `index=leveldbLarge`、`max=0`、`minFreeSpace=10GiB`、`concurrentUploadLimitMB=0`、`concurrentDownloadLimitMB=0` | `dir=/data/massdb-oss-storage/massdb-oss-volume` |
| filer | 9350，metrics 9382；内嵌 S3 9010 | `concurrentUploadLimitMB=0`、`saveToFilerLimit=0`、`s3=true`、`s3.config=conf/s3.json` | `defaultStoreDir=/data/massdb-oss-storage/massdb-oss-filer` |
| s3（可选，独立 S3 主机） | 9010，metrics 9383 | `ip.bind=0.0.0.0`（weed s3 默认只听 localhost）、`filer=localhost:9350`、`config=conf/s3.json` | — |

各角色 `logdir=/data/massdb-oss-storage/massdb-oss-log/<角色>`（按角色分目录：glog 自带滚动，每级 10 MB × 5 个文件，按 `weed.<主机>.<用户>.log.<级别>.` 前缀清理旧文件，同一用户的角色共用目录会互相清理），`alsologtostderr=false`，`.out` 只留启动错误与崩溃信息。`filer.toml` 只有 `[cassandra]` 段（元数据只用 Cassandra，决策 D4），表结构见 `weed/filer/cassandra/schema.cql`；`master.toml` 只有 `[master.license] peer_ips` 占位。

不照搬现场的部分：不用 `kill -9`、不靠 cron 守护、不在每次启动前删目录、不把标准输出写成不滚动的大文件、S3 不写死访问密钥也不开匿名读。

## 元数据库包（massdb-oss-meta）

```text
massdb-oss-meta-5.0.8/
  package.json                  kind、product、cassandraVersion、javaVersion（17）、jmxExporterVersion、roles
  cassandra/                    Apache Cassandra 5.0.x 官方包原样解压
  cassandra/bin/start_cassandra.sh  stop_cassandra.sh  massdb-cassandra-common.sh
  cassandra/conf/cassandra.yaml  jvm17-server.options   套用 package/meta/patches 后的默认值
  cassandra/conf/massdb-cassandra.conf                  启动脚本设置：log_dir、jmx_exporter_port
  cassandra/conf/jmx-exporter.yaml                      JMX exporter 规则
  cassandra/lib/jmx_prometheus_javaagent.jar
```

- 默认值（`package/meta/patches/*.patch`，以 `patch -p1 -F0` 严格套用，换 5.0.x 小版本若上下文不符则出包失败）：`cluster_name: 'weed_filer_cluster'`、数据 / commitlog / hints / saved_caches 在 `/data/massdb-oss-storage/massdb-oss-filer-meta/`、`authenticator: PasswordAuthenticator`（平台建自己的超级用户后禁用默认账号）、`concurrent_reads: 64` / `concurrent_writes: 192`（现场值）、`memtable_allocation_type: offheap_objects`、`endpoint_snitch: GossipingPropertyFileSnitch`；`num_tokens: 16` 与 `allocate_tokens_for_local_replication_factor: 3` 是 5.0 原版值，出包时校验。`jvm17-server.options` 末尾留了堆的占位：平台按主机内存成对写 `-Xms`/`-Xmx`，不写时 `cassandra-env.sh` 自动计算。地址、种子、端口与合并参数由平台渲染。
- `start_cassandra.sh [--daemon]`：读 `massdb-cassandra.conf`（未知键、重复键报错），设 `CASSANDRA_LOG_DIR`，`jmx_exporter_port` 非 0 时经 `JVM_EXTRA_OPTS` 加载 JMX exporter（默认 7070）；`--daemon` 时 `setsid nohup bin/cassandra -p cassandra/bin/cassandra.pid`，输出写 `<log_dir>/cassandra.out`；pid 存活且是 CassandraDaemon 时拒绝重复启动。JDK 用 `JAVA_HOME` 或 PATH 上的 java。不以 root 运行。
- `stop_cassandra.sh [--timeout 秒]`：SIGTERM（Cassandra 的关机钩子会 drain），默认等 90 秒，超时返回 1，不发 SIGKILL。
- JMX exporter 规则沿用 Manager 仪表盘已在用的指标名（`cassandra_compaction_pendingtasks`、`cassandra_clientrequest_latency_count_total{scope}` 等），并导出业务 keyspace 表级的分区大小、每次读的 SSTable 数与扫描墓碑数（`cassandra_table_*{keyspace,table}`）、被丢弃的写入、线程池积压；JVM 堆与 GC 由 exporter 自带。

## 测试

```bash
package/test/scripts_test.sh                      # 假 weed 覆盖参数转换、全局参数位置、WEED_CONFIG_DIR、幂等、pid 残留、超时等
REAL_WEED=<weed 二进制> package/test/scripts_test.sh   # 另外用真实 weed 验证 toml 从角色 conf 目录读取、SIGTERM 停止
```

macOS 与 Linux 都可运行（bash 3.2 起，不需要 root）；没有 Go 的机器上用 `FAKE_WEED=` 指定预先编译好的 `package/test/fakeweed`。
