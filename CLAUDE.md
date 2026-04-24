# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository layout

Three related codebases live here:

- **`weed/`** — the main Go `weed` binary (master, volume, filer, s3, mq, mount, admin, shell, iam, worker). Module: `github.com/seaweedfs/seaweedfs`, Go `1.25.0`.
- **`seaweed-volume/`** — Rust port of the volume server. Binary-compatible (`.dat`/`.idx`/`.vif`) and speaks the same HTTP/gRPC protocols, so it interoperates with an unmodified Go master. Built with `cargo`. See `VOLUME_SERVER_RUST_PLAN.md` for the porting plan.
- **`seaweedfs-rdma-sidecar/`** — Go + Rust (UCX) RDMA read-path acceleration sidecar. Has its own `go.mod`.

Auxiliary top-level dirs: `cmd/weed-db` and `cmd/weed-sql` (standalone tools), `other/java` (Java client — proto is copied here by `weed/pb/Makefile`), `test/` (integration tests, each subdir has its own `Makefile`), `docker/` (compose files + dev Dockerfiles), `telemetry/`, `k8s/`.

## Build / install / test

The root `Makefile` is the entry point and delegates into `weed/Makefile` and `weed/admin/Makefile`.

```bash
make install          # admin-generate + `cd weed && go install` → $GOPATH/bin/weed
make full_install     # same, but with -tags "elastic gocdk sqlite ydb tarantool tikv rclone"
make test             # admin-generate + cd weed && go test -tags "..." -v ./...
make server           # install + start weed server with s3+filer+volume for local dev
make weed-commands    # builds the standalone cmd/weed-db and cmd/weed-sql binaries
```

**Build tags** (`elastic gocdk sqlite ydb tarantool tikv rclone`) gate optional filer backends and storage drivers. CI (`.github/workflows/go.yml`) builds and tests with this tag set — reproduce it locally before pushing when touching filer stores or remote backends.

**Run a single test:**
```bash
cd weed && go test -tags "elastic gocdk sqlite ydb tarantool tikv rclone" -run TestFoo ./path/to/pkg
```
Integration tests under `test/<subsystem>/` each have their own `Makefile` (e.g. `test/s3/iam/`, `test/volume_server/`, `test/mq/`, `test/fuse_integration/`). They typically start `weed` processes and drive them via HTTP/gRPC; run `make help` in those dirs to see targets.

**`go vet`** filters out expected protobuf `sync.Mutex` copy warnings; see the `vet` job in `.github/workflows/go.yml` if vet seems noisy.

## Admin UI: templ generation is required before build

`weed/admin/view/**/*.templ` are [a-h/templ](https://github.com/a-h/templ) templates compiled to `*_templ.go`. The root `install` / `test` targets run `admin-generate` automatically, but **if you edit a `.templ` file or run `go build ./weed` directly, regenerate first:**

```bash
make admin-generate         # or: cd weed/admin && templ generate ./view
```

Other admin targets (all prefixed `admin-` at the top level, plain names inside `weed/admin/`): `admin-build`, `admin-run`, `admin-dev` (watches templ files), `admin-fmt`, `admin-clean`, `admin-test`. Admin UI defaults to port `23646` (`23647` in `dev`).

## Protobuf

All gRPC services are defined in `weed/pb/*.proto` (`master`, `volume_server`, `filer`, `s3`, `iam`, `mount`, `remote`, `mq_broker`, `mq_schema`, `mq_agent`, `worker`, `plugin`). After editing a `.proto`:

```bash
cd weed/pb && make gen
```

This regenerates the `*_pb/` Go packages, **and also copies `filer.proto` into `other/java/client/src/main/proto` and copies `volume_server.proto`, `master.proto`, `remote.proto` into `seaweed-volume/proto/`** — keep those three sibling projects in sync when you change wire definitions.

## CLI command system

`weed/weed.go` is the `main` entry point. It iterates over `command.Commands` (registered in `weed/command/command.go`). Each subcommand is a `*Command` struct (see `Command` definition in the same file) with `Run`, `UsageLine`, `Short`, `Long`, and a `Flag` set. To add a subcommand: create `weed/command/<name>.go` with a `var cmdX = &Command{...}`, and append it to `Commands`.

**Flag parsing uses `weed/util/fla9`, not stdlib `flag`.** The `flag` import in command files is aliased: `flag "github.com/seaweedfs/seaweedfs/weed/util/fla9"`. It supports TOML config files and repeated flags. Don't swap it for stdlib `flag`.

## Local debugging

`weed/Makefile` has `debug_*` targets that build with `-gcflags="all=-N -l"` and launch under `dlv` on `:2345`:

```
debug_server / debug_mount / debug_volume / debug_s3 / debug_mq / debug_mq_agent /
debug_filer_copy / debug_filer_sync / debug_filer_remote_sync_{dir,buckets} /
debug_master_follower / debug_webdav / debug_shell
```

`debug_volume` builds with `-tags=5BytesOffset` (volumes > 32GB).

## Architecture — the big picture

**Three-layer stack.** Understanding this is usually the prerequisite for productive changes:

1. **Master** (`weed/server/master_server*.go`, port `9333`) — singleton cluster coordinator. Holds the volume-id → volume-server mapping (topology) and issues sequential 64-bit file keys on write. Master HA uses Raft (`master_grpc_server_raft.go`). `weed/topology/` holds the in-memory DataCenter → Rack → DataNode → Volume tree.

2. **Volume server** (`weed/server/volume_*.go` and `weed/storage/`, port `8080` default) — stores blobs as "needles" appended to `.dat` files with a parallel `.idx` index, per Facebook's Haystack design. A client writes via `POST volume_url/<vid>,<fid><cookie>` and reads via `GET`; the 16-byte-per-blob in-memory needle map means one disk seek per read. `weed/storage/needle/` implements the on-disk needle format; `weed/storage/erasure_coding/` handles EC (10.4) for warm data; `weed/storage/backend/` wraps cloud-tiered backends. Volume vacuum/compaction is in `volume_vacuum.go`.

3. **Filer** (`weed/filer/` + `weed/server/filer_*.go`, port `8888`) — optional POSIX-ish directory layer **on top of** the blob store. Files are split into chunks; each chunk is a `(volume_id, file_key, cookie, size)` reference; the filer stores the chunk manifest per file. The directory tree itself lives in a **pluggable metadata store** — every subdirectory of `weed/filer/` (`mysql`, `postgres*`, `redis*`, `cassandra*`, `mongodb`, `elastic`, `leveldb*`, `rocksdb`, `sqlite`, `etcd`, `foundationdb`, `hbase`, `arangodb`, `redis_lua`, `tikv`, `ydb`, `tarantool`) is an implementation of the `FilerStore` interface in `filerstore.go`. When adding a backend, register it via `init()`→`Stores = append(...)` and gate compile-time dependencies behind a build tag matching the one in `Makefile`.

**Gateways sitting above filer:**

- **S3** (`weed/s3api/`, port `8333`) — AWS S3-compatible HTTP API. IAM/STS/policy logic is in `weed/s3api/policy*`, `weed/s3api/s3_iam_*.go`, and the broader `weed/iam/` tree. SSE (C/KMS/S3), object-lock, bucket CORS, and multipart uploads each have dedicated files.
- **Mount** (`weed/mount/`) — FUSE client. `weedfs.go` implements go-fuse callbacks; `page_writer/` + `dirty_pages_chunked.go` handle write buffering; optional RDMA fast-path in `rdma_client.go` pairs with the `seaweedfs-rdma-sidecar/` project.
- **WebDAV**, **SFTP** (`weed/sftpd/`), **Hadoop FS** (`other/java/hdfs*`).

**Message queue** (`weed/mq/`) — separate subsystem built on the blob store. Broker leaders are elected by masters; segments are stored as files on the filer via `weed/mq/logstore`. Sub-packages: `broker`, `agent`, `client`, `pub_balancer`, `sub_coordinator`, `topic`, `segment`, `offset`, `schema`, `kafka` (Kafka-protocol gateway via `weed mq.kafka.gateway`).

**Admin** (`weed/admin/`, port `23646`) — web UI using `templ`, `dash` and `handlers` for app logic, `topology/` for cluster views, `maintenance/` for background jobs.

**Cross-cutting packages:** `weed/pb/` (all gRPC clients/servers), `weed/util/` (config, http client, fla9 flag), `weed/security/` (JWT, TLS), `weed/glog/` (vendored glog), `weed/stats/` (Prometheus), `weed/wdclient/` (volume-server client used by filer/s3/mount), `weed/operation/` (high-level client ops like assign/upload/delete).

## Default ports (used by `make server`, `weed mini`, tests)

`9333` master · `8080` volume · `8888` filer · `8333` S3 · `7333` WebDAV · `8334` SFTP · `23646` admin · `9324` metrics · `17777` mq broker.

## Conventions worth following

- **Never push to `master`-style real branches from Claude sessions.** Changes on this machine go to the branch named in the session prompt.
- When adding a new filer store or cloud backend, mirror an existing one (e.g. copy `weed/filer/redis3/` as a template), add the same build-tag guards the siblings use, and append the tag to `Makefile`'s `full_install` / `test` if the CI needs to exercise it.
- Protos are the contract: when you change a `.proto`, regenerate, and also update `seaweed-volume/proto/` (for Rust port) and `other/java/client/src/main/proto` (Java client) by running `make -C weed/pb gen` — the Makefile copies them for you.
- Optional-feature code is gated by build tags rather than runtime flags; don't add an `if feature {}` where a `//go:build feature` file split is the existing pattern.
