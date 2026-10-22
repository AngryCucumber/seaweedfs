#!/usr/bin/env bash
# MassDB OSS s3 启动脚本（weed s3，技术来源 SeaweedFS）：按 ../conf/s3.conf 启动。
# 用法: start_s3.sh [--daemon]
ROLE=s3
REQUIRED_DIRS=""
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/massdb-oss-role.sh" || exit 1
massdb_oss_start "$@"
