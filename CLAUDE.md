# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A fork of SeaweedFS 3.84 (Go module `github.com/seaweedfs/seaweedfs`, Go 1.23) being turned into **MassDB OSS**. Upstream code is largely untouched; fork-specific work is the MassDB branding on the master/volume/filer UI pages, a hashed-key Cassandra filer store, and an **authorization certificate (license) system** (implemented, phases P0–P5 done on branch `3.84-license`; see "License system" below).

License documents are in Chinese under `docs/`: `license-p0-contract-*.md` is the frozen contract (claims, reason codes, wire constants, hook points) and wins on conflict; `license-certificate-execution-plan-*.md` holds the design plus the implementation and test records of each phase (sections 8.1–8.20, including deviations and what was not covered); `license-operations-manual-*.md` is the operator manual. Read the contract and the relevant plan sections before changing license code, and record behaviour changes there (dated revisions, as the existing ones are).

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

On macOS, test binaries of packages that pull in gopsutil (`server`, `command`, `shell`, `s3api`, ...) crash at init in `go-m1cpu`'s cgo code, so run them with `CGO_ENABLED=0`; the `tikv` tag then fails to build on darwin (`gosigar` needs cgo), so the exact CI command only works on Linux. `go vet` reports about 30 upstream findings; compare against the base before treating one as new.

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
- The Cassandra store supports hashed metadata keys with legacy-row compatibility; schema in `filer/cassandra/schema.cql` and `cassandra_schema.go`, documented in `filer/cassandra/README.md`. Prefixed listing is a range query on the partition (`prefixUpperBound`); integration tests take `SEAWEEDFS_CASSANDRA_TEST_HOSTS` (plus `_USERNAME`/`_PASSWORD`).

## Installation packages

`package/` builds the MassDB Manager deployment packages (spec and conventions in `package/README.md`): `make massdb-package ARCH=arm64 TRUST=<trust manifest>` cross-compiles `weed` with `CGO_ENABLED=0`, injects `util.COMMIT` and a test trust root, and writes `package.json`; `make massdb-meta-package CASSANDRA_TARBALL=… JMX_EXPORTER_JAR=…` wraps Apache Cassandra 5.0.x with the platform's start/stop scripts and the defaults in `package/meta/patches`. The version stays the upstream 3.84 (Manager decision D2). Role scripts share `package/bin/massdb-oss-role.sh` (bash 3.2 compatible); `package/test/scripts_test.sh` tests them with a fake `weed` (`REAL_WEED=` adds checks with a real binary). Output goes to the ignored `dist/`.

## License system

A vendor-signed certificate (compact Ed25519 JWS, protocol v1 shared with MassDB SQL) limits reading data and the number of masters, volume servers and filers. Without a valid certificate, reads of file content are refused (volume and filer HTTP `GET`, S3 `GetObject`, webdav, `weed mount` reads, seven volume gRPC data RPCs, inline content in filer gRPC and metadata outputs); writes, deletes and metadata stay open. Without any certificate only a single master can start and volume servers and filers cannot register.

- `weed/license/`: certificate parsing and trust store (`jws.go`, `trust.go`), the `(newest, prev)` state (`state.go`), the master-side `Manager` (`manager.go`: import, voter-majority commit, election reconcile, member ledger with grace period), the volume/filer `Node` and its lock-free read `Guard`. `licensetest/` builds nodes in a given state for other packages' tests.
- Master: `server/master_license*.go` (raft adapter, peer RPCs, address check on master-to-master RPCs), `master_grpc_server_license.go`, `master_server_handlers_license.go` (`/license`, `/license/deployment`, `/license/validate`, `/license/import`, `DELETE /license/nodes/{addr}`), admission in `SendHeartbeat`/`KeepConnected` (`master_grpc_server.go`). State is `<mdir>/license/state.json`. CLI: `weed license.init`, `weed master -license.file`, `weed server -master.license.file`, shell `license.show/import/validate`; `master.toml` `[master.license] peer_ips`.
- Volume and filer: `server/license_node.go` (guards, outgoing internal token), `server/filer_license.go` (hiding inline content, write merge rules, `KvGet` limits), `filer/internal_token.go`, `mount/filehandle_license.go`. Each node caches certificates in `license-cache.json` (volume's first `-dir`, filer's `-defaultStoreDir`).
- The production trust manifest in `trust.go` is still empty, so a plain build accepts no certificate. Test builds inject a key with `-ldflags "-X github.com/seaweedfs/seaweedfs/weed/license.testTrustManifest=<base64url manifest>"` (`make massdb-package TRUST=` does this). The production issuer is MassDB Manager's license center `manager-license` (plan 7.3, revised 2026-10-21); the massdb-sql issuer only produces the interop vectors in `license/testdata`. `TestExternalIssuerVectors` checks vectors from another issuer when `SEAWEEDFS_LICENSE_VECTORS_DIR` is set (format in plan 8.21); the license center's interop test depends on its name and that variable.
- `docker/compose/local-license-compose.yml` is a licensed cluster with the first-deployment steps in its header.
- Import follows MassDB SQL's replacement rule (contract 1.5): a higher sequence replaces both certificates whenever its limits cover the ledger, and validate and import report `changes` (shorter expiry, coverage gap, lower limits, discarded pending, removed features) without refusing. Unlike MassDB SQL since 2026-10-16, a future certificate with lower limits is accepted (plan 8.20).

Rules that past bugs came from: the internal read token is only attached in the filer's maintenance reads (`fetchWholeChunk`, `ChunkStreamReader`, `StreamContent`) and between filers/volumes, never on user reads; directory exemptions are decided on the cleaned full path of the entry actually looked up (directory plus name); the filer keeps stored content for a hidden update only when its size, chunks and remote object all match the stored entry; a master confirms state only once `state.json` holds it (directory synced after the rename), also while a write is still running and when the leader counts itself to remove a master; adopting a peer's certificate also takes its clock repair; mount handles keep local uid/gid and unflushed attribute or size changes when content comes back but take the inline bytes, chunks, remote object, size and md5 from the filer together (chunks or a remote object cannot be cut to a local size change, so that case stays hidden), zero hidden content past the smallest size a truncation left it at, every write path (`Write`, `CopyFileRange`) clears the hidden flag, a retry result is dropped once the read failure it started from was replaced, and the retry kind follows the hidden flag of that failure, not its latest error; a remote-only file is cached before the first read, write or size change on a handle (truncating to 0 drops its remote object instead), the download installs its chunks together with the chunk group reads use, and the filer event of that download does not replace the handle; a write's timestamp is later than every chunk of the entry and than the handle's previous write, because the filer's clock stamps cached and refetched chunks, and `SetAttr` holds the handle lock like a write; certificate pairs compare `prev` too; goraft starts every term count from 0 after all masters restart, so ledger versions use the leader's epoch, not the raw raft term.
