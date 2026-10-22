#!/usr/bin/env bash
# MassDB OSS volume 停止脚本：发 SIGTERM 等待退出，默认等 60 秒，超时返回 1，不发 SIGKILL。
# 用法: stop_volume.sh [--timeout 秒]
ROLE=volume
STOP_TIMEOUT=${STOP_TIMEOUT:-60}
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/massdb-oss-role.sh" || exit 1
massdb_oss_stop "$@"
