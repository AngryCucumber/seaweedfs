#!/usr/bin/env bash
# MassDB OSS 元数据库停止脚本：向 bin/cassandra.pid 记录的 CassandraDaemon 发 SIGTERM
# （Cassandra 退出前会 drain，刷写 memtable），等待退出，默认 90 秒，超时返回 1，不发 SIGKILL。
# 用法: stop_cassandra.sh [--timeout 秒]

CASSANDRA_HOME=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PID_FILE=$CASSANDRA_HOME/bin/cassandra.pid
. "$CASSANDRA_HOME/bin/massdb-cassandra-common.sh" || exit 1

timeout=${STOP_TIMEOUT:-90}
while [ $# -gt 0 ]; do
	case $1 in
	--timeout)
		[ $# -ge 2 ] || {
			cass_err "--timeout 需要秒数"
			exit 2
		}
		timeout=$2
		shift 2
		;;
	-h | --help)
		echo "用法: stop_cassandra.sh [--timeout 秒]"
		exit 0
		;;
	*)
		cass_err "未知参数: $1"
		exit 2
		;;
	esac
done
case $timeout in
'' | *[!0-9]*)
	cass_err "等待秒数无效: $timeout"
	exit 2
	;;
esac

if ! pid=$(cass_pid_from_file); then
	[ -e "$PID_FILE" ] && rm -f "$PID_FILE"
	cass_log "未在运行"
	exit 0
fi
if ! cass_alive "$pid"; then
	rm -f "$PID_FILE"
	cass_log "未在运行（清理残留的 pid 文件，pid $pid）"
	exit 0
fi
if ! cass_is_ours "$pid"; then
	rm -f "$PID_FILE"
	cass_log "pid $pid 不是 CassandraDaemon，按残留 pid 文件清理，未发送信号"
	exit 0
fi
if ! kill -TERM "$pid"; then
	cass_err "向 pid $pid 发送 SIGTERM 失败"
	exit 1
fi
cass_log "已向 pid $pid 发送 SIGTERM，最多等待 $timeout 秒"
waited=0
while cass_alive "$pid"; do
	if [ "$waited" -ge "$timeout" ]; then
		cass_err "等待 $timeout 秒后 pid $pid 仍在运行（未发送 SIGKILL）"
		exit 1
	fi
	sleep 1
	waited=$((waited + 1))
done
rm -f "$PID_FILE"
cass_log "已停止（等待 ${waited} 秒）"
