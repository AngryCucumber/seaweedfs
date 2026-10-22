#!/usr/bin/env bash
# MassDB OSS master 启动脚本（weed master，技术来源 SeaweedFS）：按 ../conf/master.conf 启动。
# 用法: start_master.sh [--daemon]
ROLE=master
REQUIRED_DIRS="mdir"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/massdb-oss-role.sh" || exit 1
massdb_oss_start "$@"
