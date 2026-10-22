#!/usr/bin/env bash
# MassDB OSS 元数据库启动脚本（Apache Cassandra，CassandraDaemon）。
# 用法: start_cassandra.sh [--daemon]
#   --daemon 时后台运行：标准输出写 <log_dir>/cassandra.out，pid 写 bin/cassandra.pid（由 bin/cassandra -p 写入）。
# 设置读 ../conf/massdb-cassandra.conf（log_dir、jmx_exporter_port）。JDK 用 JAVA_HOME 或 PATH 上的 java（5.0 需 11 或 17）。
# 堆内存：jvm17-server.options 里成对的 -Xms/-Xmx（平台渲染）；没有时 cassandra-env.sh 自动计算。
# 不以 root 运行（Cassandra 自身也会拒绝）。

CASSANDRA_HOME=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CONF=$CASSANDRA_HOME/conf/massdb-cassandra.conf
PID_FILE=$CASSANDRA_HOME/bin/cassandra.pid
. "$CASSANDRA_HOME/bin/massdb-cassandra-common.sh" || exit 1

daemon=0
for arg in "$@"; do
	case $arg in
	--daemon) daemon=1 ;;
	-h | --help)
		echo "用法: start_cassandra.sh [--daemon]"
		exit 0
		;;
	*)
		cass_err "未知参数: $arg"
		exit 2
		;;
	esac
done

cass_read_conf || exit 1
if pid=$(cass_pid_from_file); then
	if cass_alive "$pid" && cass_is_ours "$pid"; then
		cass_err "已在运行（pid $pid），拒绝重复启动"
		exit 1
	fi
	cass_log "清理残留的 pid 文件（pid $pid 已不在运行或不是 Cassandra）"
	rm -f "$PID_FILE"
elif [ -e "$PID_FILE" ]; then
	rm -f "$PID_FILE"
fi

if ! mkdir -p "$LOG_DIR"; then
	cass_err "无法创建日志目录 $LOG_DIR"
	exit 1
fi
export CASSANDRA_HOME
export CASSANDRA_CONF=$CASSANDRA_HOME/conf
export CASSANDRA_LOG_DIR=$LOG_DIR
if [ "$JMX_EXPORTER_PORT" != 0 ]; then
	agent=$CASSANDRA_HOME/lib/jmx_prometheus_javaagent.jar
	if [ ! -f "$agent" ]; then
		cass_err "找不到 JMX exporter $agent（不需要时把 jmx_exporter_port 设为 0）"
		exit 1
	fi
	export JVM_EXTRA_OPTS="${JVM_EXTRA_OPTS:+$JVM_EXTRA_OPTS }-javaagent:$agent=$JMX_EXPORTER_PORT:$CASSANDRA_CONF/jmx-exporter.yaml"
fi
cd "$CASSANDRA_HOME" || exit 1

if [ "$daemon" = 0 ]; then
	echo $$ >"$PID_FILE"
	exec "$CASSANDRA_HOME/bin/cassandra" -f -p "$PID_FILE"
fi

out=$LOG_DIR/cassandra.out
echo "==== $(date '+%Y-%m-%d %H:%M:%S') start: $CASSANDRA_HOME/bin/cassandra -p $PID_FILE" >>"$out" || exit 1
# bin/cassandra 不带 -f 时自己把 JVM 放到后台、写 pid 文件后返回；JVM 启动完成后关闭标准输出。
if command -v setsid >/dev/null 2>&1; then
	setsid nohup "$CASSANDRA_HOME/bin/cassandra" -p "$PID_FILE" >>"$out" 2>&1 </dev/null
else
	nohup "$CASSANDRA_HOME/bin/cassandra" -p "$PID_FILE" >>"$out" 2>&1 </dev/null
fi
status=$?
sleep "${START_CHECK_SECONDS:-5}"
pid=$(cass_pid_from_file)
if [ "$status" != 0 ] || [ -z "$pid" ] || ! cass_alive "$pid"; then
	rm -f "$PID_FILE"
	cass_err "启动失败或启动后立即退出，$out 末尾："
	tail -n 20 "$out" >&2
	exit 1
fi
cass_log "已启动（pid $pid），输出写入 $out，日志在 $LOG_DIR"
