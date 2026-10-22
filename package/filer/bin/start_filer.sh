#!/usr/bin/env bash
# MassDB OSS filer 启动脚本（weed filer，技术来源 SeaweedFS）：按 ../conf/filer.conf 启动。
# 用法: start_filer.sh [--daemon]
ROLE=filer
REQUIRED_DIRS="defaultStoreDir"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/massdb-oss-role.sh" || exit 1
massdb_oss_start "$@"
