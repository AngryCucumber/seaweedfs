# shellcheck shell=bash
# start_cassandra.sh / stop_cassandra.sh 的公共函数，不单独执行。兼容 bash 3.2。

cass_log() {
	echo "[cassandra] $*"
}

cass_err() {
	echo "[cassandra] 错误: $*" >&2
}

cass_trim() {
	local s=$1
	s=${s#"${s%%[![:space:]]*}"}
	s=${s%"${s##*[![:space:]]}"}
	printf '%s' "$s"
}

# 读 massdb-cassandra.conf，设置 LOG_DIR 与 JMX_EXPORTER_PORT；未知键和重复键报错。
cass_read_conf() {
	LOG_DIR=$CASSANDRA_HOME/logs
	JMX_EXPORTER_PORT=7070
	[ -f "$CONF" ] || return 0
	local line key value n=0 seen=" "
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		line=$(cass_trim "${line%$'\r'}")
		case $line in
		'' | '#'*) continue ;;
		*=*) ;;
		*)
			cass_err "$CONF 第 $n 行不是 key = value 格式: $line"
			return 1
			;;
		esac
		key=$(cass_trim "${line%%=*}")
		value=$(cass_trim "${line#*=}")
		case $seen in
		*" $key "*)
			cass_err "$CONF 第 $n 行重复设置了 $key"
			return 1
			;;
		esac
		seen="$seen$key "
		case $key in
		log_dir) [ -n "$value" ] && LOG_DIR=$value ;;
		jmx_exporter_port)
			case $value in
			'' | *[!0-9]*)
				cass_err "$CONF 第 $n 行 jmx_exporter_port 不是端口号: $value"
				return 1
				;;
			esac
			JMX_EXPORTER_PORT=$value
			;;
		*)
			cass_err "$CONF 第 $n 行是未知的键: $key"
			return 1
			;;
		esac
	done <"$CONF"
}

cass_alive() {
	local stat
	stat=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
	case $(cass_trim "$stat") in
	'' | Z*) return 1 ;;
	esac
}

# 进程是 CassandraDaemon（识别 pid 被复用的残留 pid 文件）。读 /proc/<pid>/cmdline，
# 没有 /proc 时用 ps -ww（Cassandra 的命令行很长，主类在最后）。
cass_is_ours() {
	local args
	if [ -r "/proc/$1/cmdline" ]; then
		args=$(tr '\0' ' ' <"/proc/$1/cmdline")
	else
		args=$(ps -ww -o args= -p "$1" 2>/dev/null) || return 1
	fi
	case $args in
	*org.apache.cassandra.service.CassandraDaemon*) return 0 ;;
	esac
	return 1
}

cass_pid_from_file() {
	local pid
	[ -f "$PID_FILE" ] || return 1
	IFS= read -r pid <"$PID_FILE" || [ -n "$pid" ] || return 1
	pid=$(cass_trim "$pid")
	case $pid in
	'' | *[!0-9]*) return 1 ;;
	esac
	printf '%s' "$pid"
}
