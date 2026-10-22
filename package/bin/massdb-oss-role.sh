# shellcheck shell=bash
# MassDB OSS 角色启停的公共逻辑，由 <角色>/bin/start_<角色>.sh、stop_<角色>.sh 引用，不单独执行。
# 引用前由角色脚本设置：
#   ROLE              角色名，同时是 weed 子命令：master / volume / filer / s3
#   REQUIRED_DIRS     （启动）必须在 <角色>.conf 里设置、且目录已存在的键，空格分隔，如 "mdir"
#   STOP_TIMEOUT      （停止）等待进程退出的秒数，环境变量可覆盖
#
# 安装包布局（package/README.md）：
#   <包根>/bin/weed                  weed 二进制（技术来源 SeaweedFS）
#   <包根>/<角色>/conf/<角色>.conf    启动参数，一行一个 key = value
#   <包根>/<角色>/conf/*.toml         toml 配置；WEED_CONFIG_DIR 和 -config_dir 都指向这里
#   <包根>/<角色>/bin/<角色>.pid      进程号
#   <logdir>/<角色>.out               --daemon 时的标准输出与标准错误
#
# 兼容 bash 3.2（macOS 自带），不用关联数组。

MASSDB_OSS_HOME=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ROLE_HOME=$MASSDB_OSS_HOME/$ROLE
ROLE_BIN=$ROLE_HOME/bin
ROLE_CONF_DIR=$ROLE_HOME/conf
ROLE_CONF=$ROLE_CONF_DIR/$ROLE.conf
PID_FILE=$ROLE_BIN/$ROLE.pid
WEED=$MASSDB_OSS_HOME/bin/weed

# weed 的全局参数（glog 与 -config_dir），必须写在子命令之前
WEED_GLOBAL_KEYS=" logdir v vmodule logtostderr alsologtostderr stderrthreshold log_backtrace_at config_dir "

massdb_oss_log() {
	echo "[$ROLE] $*"
}

massdb_oss_err() {
	echo "[$ROLE] 错误: $*" >&2
}

massdb_oss_trim() {
	local s=$1
	s=${s#"${s%%[![:space:]]*}"}
	s=${s%"${s##*[![:space:]]}"}
	printf '%s' "$s"
}

# 读 <角色>.conf 到 CONF_KEYS / CONF_VALUES。格式：一行一个 key = value，按第一个 = 拆分，
# 键值两侧空白去掉；# 开头的行和空行跳过；值原样传给 weed，不做引号和变量展开；
# 多值按 weed 的写法用逗号连接；同一个键出现两次报错。
massdb_oss_read_conf() {
	CONF_KEYS=()
	CONF_VALUES=()
	if [ ! -f "$ROLE_CONF" ]; then
		massdb_oss_err "找不到配置文件 $ROLE_CONF"
		return 1
	fi
	local line key value n=0 i
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		line=$(massdb_oss_trim "${line%$'\r'}")
		case $line in
		'' | '#'*) continue ;;
		*=*) ;;
		*)
			massdb_oss_err "$ROLE_CONF 第 $n 行不是 key = value 格式: $line"
			return 1
			;;
		esac
		key=$(massdb_oss_trim "${line%%=*}")
		value=$(massdb_oss_trim "${line#*=}")
		if ! [[ $key =~ ^[A-Za-z][A-Za-z0-9._-]*$ ]]; then
			massdb_oss_err "$ROLE_CONF 第 $n 行的键无效: $key"
			return 1
		fi
		for ((i = 0; i < ${#CONF_KEYS[@]}; i++)); do
			if [ "${CONF_KEYS[i]}" = "$key" ]; then
				massdb_oss_err "$ROLE_CONF 第 $n 行重复设置了 $key"
				return 1
			fi
		done
		CONF_KEYS+=("$key")
		CONF_VALUES+=("$value")
	done <"$ROLE_CONF"
}

# 输出某个键的值（没设置或值为空时输出空串）
massdb_oss_conf_get() {
	local i
	for ((i = 0; i < ${#CONF_KEYS[@]}; i++)); do
		if [ "${CONF_KEYS[i]}" = "$1" ]; then
			printf '%s' "${CONF_VALUES[i]}"
			return 0
		fi
	done
	return 1
}

# 生成 WEED_ARGS：全局参数 + 子命令 + 子命令参数。值为空的键跳过（用 weed 默认值）。
# 没设 logdir 时用 <角色>/log，没设 config_dir 时用 <角色>/conf。
massdb_oss_build_args() {
	local global=() role=() i key value
	for ((i = 0; i < ${#CONF_KEYS[@]}; i++)); do
		key=${CONF_KEYS[i]}
		value=${CONF_VALUES[i]}
		[ -n "$value" ] || continue
		case $WEED_GLOBAL_KEYS in
		*" $key "*) global+=("-$key=$value") ;;
		*) role+=("-$key=$value") ;;
		esac
	done
	LOG_DIR=$(massdb_oss_conf_get logdir)
	if [ -z "$LOG_DIR" ]; then
		LOG_DIR=$ROLE_HOME/log
		global+=("-logdir=$LOG_DIR")
	fi
	WEED_CONFIG_DIR=$(massdb_oss_conf_get config_dir)
	if [ -z "$WEED_CONFIG_DIR" ]; then
		WEED_CONFIG_DIR=$ROLE_CONF_DIR
		global+=("-config_dir=$WEED_CONFIG_DIR")
	fi
	WEED_ARGS=("${global[@]}" "$ROLE" "${role[@]}")
}

# 检查 REQUIRED_DIRS 里的键已设置且目录存在（逗号分隔的多个目录逐个检查）。
# 脚本不创建数据目录，避免挂载点没挂上时数据写到系统盘。
massdb_oss_check_dirs() {
	local key value dir dirs
	for key in $REQUIRED_DIRS; do
		value=$(massdb_oss_conf_get "$key")
		if [ -z "$value" ]; then
			massdb_oss_err "$ROLE_CONF 必须设置 $key（持久数据目录）"
			return 1
		fi
		IFS=, read -r -a dirs <<<"$value"
		for dir in "${dirs[@]}"; do
			if [ ! -d "$dir" ]; then
				massdb_oss_err "$key 指向的目录不存在: $dir；脚本不自动创建数据目录，请先创建并确认已挂载"
				return 1
			fi
		done
	done
}

# 进程存在且不是僵尸
massdb_oss_alive() {
	local stat
	stat=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
	case $(massdb_oss_trim "$stat") in
	'' | Z*) return 1 ;;
	esac
}

# 把进程的命令行逐个参数放进 PROC_ARGS。优先读 /proc/<pid>/cmdline（参数边界准确），
# 没有 /proc 时用 ps -ww（不截断长命令行）。
massdb_oss_proc_args() {
	PROC_ARGS=()
	local arg args
	if [ -r "/proc/$1/cmdline" ]; then
		while IFS= read -r -d '' arg; do
			PROC_ARGS+=("$arg")
		done <"/proc/$1/cmdline"
		[ ${#PROC_ARGS[@]} -gt 0 ] && return 0
	fi
	args=$(ps -ww -o args= -p "$1" 2>/dev/null) || return 1
	read -r -a PROC_ARGS <<<"$args"
	[ ${#PROC_ARGS[@]} -gt 0 ]
}

# 进程是本角色的 weed：可执行文件名为 weed，第一个不以 - 开头的参数是本角色子命令。
# 用于识别 pid 被别的进程复用的残留 pid 文件。
massdb_oss_is_ours() {
	local word
	massdb_oss_proc_args "$1" || return 1
	case ${PROC_ARGS[0]} in
	weed | */weed) ;;
	*) return 1 ;;
	esac
	for word in "${PROC_ARGS[@]:1}"; do
		case $word in
		-*) continue ;;
		esac
		[ "$word" = "$ROLE" ]
		return
	done
	return 1
}

# 输出 pid 文件里记录的进程号（文件不存在或内容不是数字时返回 1）
massdb_oss_pid_from_file() {
	local pid
	[ -f "$PID_FILE" ] || return 1
	IFS= read -r pid <"$PID_FILE" || [ -n "$pid" ] || return 1
	pid=$(massdb_oss_trim "$pid")
	case $pid in
	'' | *[!0-9]*) return 1 ;;
	esac
	printf '%s' "$pid"
}

massdb_oss_write_pid() {
	echo "$1" >"$PID_FILE.tmp" && mv -f "$PID_FILE.tmp" "$PID_FILE"
}

massdb_oss_start() {
	local daemon=0 arg pid out
	for arg in "$@"; do
		case $arg in
		--daemon) daemon=1 ;;
		-h | --help)
			echo "用法: start_$ROLE.sh [--daemon]"
			echo "  按 $ROLE_CONF 启动 weed $ROLE；--daemon 时后台运行，输出写 <logdir>/$ROLE.out"
			return 0
			;;
		*)
			massdb_oss_err "未知参数: $arg"
			return 2
			;;
		esac
	done
	if [ ! -x "$WEED" ]; then
		massdb_oss_err "找不到可执行的 $WEED"
		return 1
	fi
	cd "$ROLE_HOME" || return 1
	massdb_oss_read_conf || return 1

	if pid=$(massdb_oss_pid_from_file); then
		if massdb_oss_alive "$pid" && massdb_oss_is_ours "$pid"; then
			massdb_oss_err "已在运行（pid $pid），拒绝重复启动"
			return 1
		fi
		massdb_oss_log "清理残留的 pid 文件（pid $pid 已不在运行或不是本角色进程）"
		rm -f "$PID_FILE"
	elif [ -e "$PID_FILE" ]; then
		massdb_oss_log "清理内容无效的 pid 文件"
		rm -f "$PID_FILE"
	fi

	massdb_oss_build_args
	massdb_oss_check_dirs || return 1
	if ! mkdir -p "$LOG_DIR"; then
		massdb_oss_err "无法创建日志目录 $LOG_DIR"
		return 1
	fi
	export WEED_CONFIG_DIR

	if [ "$daemon" = 0 ]; then
		massdb_oss_write_pid $$ || return 1
		exec "$WEED" "${WEED_ARGS[@]}"
	fi

	out=$LOG_DIR/$ROLE.out
	echo "==== $(date '+%Y-%m-%d %H:%M:%S') start: $WEED ${WEED_ARGS[*]}" >>"$out" || return 1
	if command -v setsid >/dev/null 2>&1; then
		setsid nohup "$WEED" "${WEED_ARGS[@]}" >>"$out" 2>&1 </dev/null &
	else
		nohup "$WEED" "${WEED_ARGS[@]}" >>"$out" 2>&1 </dev/null &
	fi
	pid=$!
	massdb_oss_write_pid "$pid" || return 1
	sleep "${START_CHECK_SECONDS:-3}"
	if ! massdb_oss_alive "$pid"; then
		rm -f "$PID_FILE"
		massdb_oss_err "启动后立即退出，$out 末尾："
		tail -n 20 "$out" >&2
		return 1
	fi
	massdb_oss_log "已启动（pid $pid），输出写入 $out"
}

# 发 SIGTERM 并等待退出；超时返回非 0，不发 SIGKILL（由调用方决定）。
massdb_oss_stop() {
	local timeout=${STOP_TIMEOUT:-30} pid waited=0
	while [ $# -gt 0 ]; do
		case $1 in
		--timeout)
			timeout=$2
			shift 2 || {
				massdb_oss_err "--timeout 需要秒数"
				return 2
			}
			;;
		-h | --help)
			echo "用法: stop_$ROLE.sh [--timeout 秒]"
			echo "  向 $PID_FILE 记录的进程发 SIGTERM，等待退出（默认 ${STOP_TIMEOUT:-30} 秒），超时返回 1"
			return 0
			;;
		*)
			massdb_oss_err "未知参数: $1"
			return 2
			;;
		esac
	done
	case $timeout in
	'' | *[!0-9]*)
		massdb_oss_err "等待秒数无效: $timeout"
		return 2
		;;
	esac

	if ! pid=$(massdb_oss_pid_from_file); then
		[ -e "$PID_FILE" ] && rm -f "$PID_FILE"
		massdb_oss_log "未在运行"
		return 0
	fi
	if ! massdb_oss_alive "$pid"; then
		rm -f "$PID_FILE"
		massdb_oss_log "未在运行（清理残留的 pid 文件，pid $pid）"
		return 0
	fi
	if ! massdb_oss_is_ours "$pid"; then
		rm -f "$PID_FILE"
		massdb_oss_log "pid $pid 不是本角色的 weed 进程，按残留 pid 文件清理，未发送信号"
		return 0
	fi
	if ! kill -TERM "$pid"; then
		massdb_oss_err "向 pid $pid 发送 SIGTERM 失败"
		return 1
	fi
	massdb_oss_log "已向 pid $pid 发送 SIGTERM，最多等待 $timeout 秒"
	while massdb_oss_alive "$pid"; do
		if [ "$waited" -ge "$timeout" ]; then
			massdb_oss_err "等待 $timeout 秒后 pid $pid 仍在运行（未发送 SIGKILL）"
			return 1
		fi
		sleep 1
		waited=$((waited + 1))
	done
	rm -f "$PID_FILE"
	massdb_oss_log "已停止（等待 ${waited} 秒）"
}
