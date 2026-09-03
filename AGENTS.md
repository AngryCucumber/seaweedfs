# AGENTS.md

This file provides guidance to the AI agent when working with code in this repository.

## Project

SeaweedFS distributed file system. Go module is `github.com/seaweedfs/seaweedfs` (declared at repo root) but the `main` package and almost all code live under `weed/`. By convention all `go` commands are run from `weed/` (`cd weed; go ...`), matching CI and the root `Makefile`.

## Build / test

The default build tag set is **required** — without it, optional filer stores compile out and many tests silently skip or fail:

```
cd weed && go build -tags "elastic gocdk sqlite ydb tikv rclone" ./...
cd weed && go test  -tags "elastic gocdk sqlite ydb tikv rclone" ./...
```

`make test` from the repo root runs the same command. Go 1.23 is required (see `go.mod`).

Tag notes:
- `rocksdb` is **not** in the default set; it needs CGO and the RocksDB dev environment (`docker/Dockerfile.rocksdb_dev_env`). Don't enable it unless the task explicitly requires it.
- `5BytesOffset` switches volume offsets from 4 to 5 bytes — mutually exclusive with the default 4-byte build (`weed/util/constants_4bytes.go`).
- `large_disk` is used only for special docker images.

## Protobuf / FlatBuffers

`.proto` files live in `weed/pb/`. After editing them, regenerate with `cd weed/pb && make gen` (requires `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`). `make fbs` regenerates `message.fbs` via `flatc`. Commit the regenerated `*_pb/` files together with the `.proto` change.

## Repo layout gotchas

- `unmaintained/` holds one-off tools that are intentionally not built, not tested, and not released. Don't try to fix compilation errors there or wire them into CI.
- `weed/command/` contains every CLI subcommand (`weed master`, `weed volume`, `weed filer`, `weed s3`, `weed mount`, `weed shell`, …). New subcommands must be registered in `weed/command/command.go`.
- `weed/pb/` is generated code plus a small amount of hand-written glue (`server_address.go`, `grpc_client_server.go`, `filer_pb_tail.go`, `shared_values.go`) — don't blow those away when regenerating.
- `test/` contains Java/Maven S3 client tests and a `random_access` Hadoop-style harness; they are not invoked by `go test`.

## Running locally

`make server` from the repo root installs the `weed` binary and starts a single-node cluster (master + volume + filer + S3 on `:8000`) using `docker/compose/s3.json`. `make benchmark` runs the same server against `warp`. Docker-based dev/e2e flows live under `docker/` (`make dev`, `make build_e2e`, `make s3tests`, etc.).

## PRs

Target `master`. The PR template (`.github/pull_request_template.md`) asks for problem / approach / testing notes and a checkbox for added unit tests — fill it in rather than leaving it blank. Commit messages in the log mix free-form and conventional-commit styles (`chore(deps): ...`); either is acceptable.
