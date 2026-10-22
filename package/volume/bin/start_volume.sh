#!/usr/bin/env bash
# MassDB OSS volume 启动脚本（weed volume，技术来源 SeaweedFS）：按 ../conf/volume.conf 启动。
# 用法: start_volume.sh [--daemon]
ROLE=volume
REQUIRED_DIRS="dir"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/massdb-oss-role.sh" || exit 1
massdb_oss_start "$@"
