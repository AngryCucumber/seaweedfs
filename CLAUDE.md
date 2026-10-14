# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A fork of SeaweedFS 3.84 (Go module `github.com/seaweedfs/seaweedfs`, Go 1.23) being turned into **MassDB OSS**. Upstream code is largely untouched; fork-specific work so far is the MassDB branding on the master/volume/filer UI pages and a hashed-key Cassandra filer store. The active project is an **authorization certificate (license) system**; its design lives in `docs/license-certificate-execution-plan-*.md` (Chinese, long, versioned). Read the latest version before touching anything license-related: it fixes which HTTP read paths get guarded (volume and filer `GET` content only), what must stay open (writes, metadata, gRPC, internal filer reads via an internal token), and the master-side raft/membership rules. The doc is a plan, not a record of finished work.

Commit messages use the `[type](scope) summary` style, e.g. `[feat](ui) ...`, `[docs](license) ...`.

## Build, test, run

All Go code is under `weed/`; the module root has no main package. Run `go` commands from `weed/` or use `./...` relative to it.

```bash
cd weed && go install                      # plain build -> $GOPATH/bin/weed
make full_install                          # same with optional store tags
cd weed && go build -tags "elastic gocdk sqlite ydb tikv rclone" -v .   # what CI builds
```

Tests (CI runs exactly this):

```bash
cd weed && go test -tags "elastic gocdk sqlite ydb tikv rclone" -v ./...
```

Single package or test:

```bash
cd weed && go test ./filer/... -run TestName -v
```

Some tests need external services and are skipped or gated (e.g. `filer/cassandra/cassandra_integration_test.go`). Build tags gate optional stores: `elastic`, `gocdk`, `sqlite`, `ydb`, `tikv`, `rclone`, `rocksdb` (needs cgo + librocksdb; see `docker/Dockerfile.rocksdb_*`). `5BytesOffset` switches needle offsets to 5 bytes for volumes over 8 GiB (`weed/util/constants_{4,5}bytes.go`); release binaries built with it carry a `_large_disk` suffix, and a volume's `.vif` file records which offset size wrote it, so the two builds are not interchangeable on the same data.

Local dev cluster with S3 on port 8000:

```bash
make server
```

`weed/Makefile` has `debug_*` targets that build with `-gcflags="all=-N -l"` and launch under `dlv` on `:2345` for each component (server, mount, s3, volume, mq, filer.sync, ...).

Protobuf regeneration after editing any `weed/pb/*.proto`:

```bash
cd weed/pb && make gen
```

Generated `*_pb/` packages are checked in. `filer.proto` is also copied to the Java client under `other/java`.

Docker images and the FUSE end-to-end test (`.github/workflows/e2e.yml`) are driven from `docker/Makefile` (`make build`, `make build_e2e`, compose files under `docker/compose/`). S3 compatibility tests live in `test/s3/` and `.github/workflows/s3tests.yml`.

## Architecture

`weed` is a single binary; `weed/weed.go` dispatches to subcommands registered in `weed/command/command.go` (`Commands` slice). Each subcommand is one `command/*.go` file that parses flags with the vendored `util/fla9` package and starts servers from `weed/server`. `weed server` runs master + volume (+ filer, s3, webdav, iam) in one process; the standalone commands run the same server types individually.

Three server roles, each with an HTTP handler set and a gRPC service defined in `weed/pb`:

- **Master** (`server/master_*.go`, `topology/`): tracks volumes across data centers/racks/nodes, assigns file ids, elects a leader via raft. Two raft implementations coexist: legacy goraft (`raft_server.go`) and hashicorp raft (`raft_hashicorp.go`), chosen by the `-raftHashicorp` flag. Volume servers heartbeat to the master over a gRPC stream; clients hold a `KeepConnected` stream to learn volume locations (`wdclient/masterclient.go`, `vid_map.go`).
- **Volume server** (`server/volume_*.go`, `storage/`): stores needles in `.dat`/`.idx` files per volume (`storage/needle`, `needle_map`), with erasure coding in `storage/erasure_coding`, tiering to cloud backends in `storage/backend`, and vacuum/compaction. Write and read HTTP paths are `volume_server_handlers.go` / `_read.go`; most admin and copy operations are gRPC (`volume_grpc_*.go`).
- **Filer** (`server/filer_*.go`, `filer/`): maps a POSIX-like namespace onto chunks stored on volume servers. Metadata goes through the `FilerStore` interface (`filer/filerstore.go`); each backend is a subpackage (`leveldb`, `mysql2`, `postgres2`, `cassandra`, `redis3`, `etcd`, `tikv`, ...) that registers itself in `init()` via `filer.Stores = append(...)` and is pulled in by blank imports in `command/imports.go`. Config section names in `filer.toml` match `GetName()`. Large files use chunk manifests (`filer/filechunk_manifest.go`) and streaming readers (`filer/stream.go`, `reader_at.go`). Filer metadata changes are published as a log (`filer_notify*.go`, `meta_aggregator.go`) that other filers, `weed mount`, and `filer.sync`/`filer.backup` subscribe to over gRPC.

Clients on top of the filer: `s3api/` (S3 gateway with IAM config stored at `/etc/iam/identity.json` inside the filer), `mount/` (FUSE), `ftpd/`, webdav (`server/webdav_server.go`), `iamapi/`. `shell/` is `weed shell`: each `command_*.go` registers into `shell.Commands` in `init()` and is the admin surface for EC, balancing, volume checks, and `fs.*` operations. `mq/` is SeaweedMQ (broker, agent, pub/sub clients) and is largely independent of the storage path.

Configuration: all `*.toml` files (`security`, `filer`, `master`, `notification`, `replication`, `shell`) are loaded by viper through `util.LoadConfiguration`, searched in `-config_dir`, `.`, `$HOME/.seaweedfs`, `/etc/seaweedfs`. Defaults are generated by `weed scaffold -config=<name>` from `command/scaffold/*.toml`; edit the scaffold when adding a config key. `security.toml` holds the JWT signing keys that master issues and volume servers verify on write (and optionally read), plus gRPC TLS settings.

Logging is `weed/glog` with verbosity levels (`glog.V(n)`); `-v` on the command line sets the level. Build-time version/commit are injected via `-ldflags -X github.com/seaweedfs/seaweedfs/weed/util.COMMIT=...` (see `docker/Makefile`).

## Fork-specific notes

- Branding lives in the embedded templates `server/{master_ui,volume_server_ui,filer_ui}/*.html` and `weed/static/` (served through `embed.FS` in `weed.go`).
- The `unmaintained/`, `other/`, `k8s/`, and `snap/` directories are upstream extras and not part of the build.
- The Cassandra store supports hashed metadata keys with legacy-row compatibility; schema in `filer/cassandra/schema.cql` and `cassandra_schema.go`, documented in `filer/cassandra/README.md`.
