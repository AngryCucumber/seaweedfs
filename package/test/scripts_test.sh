#!/usr/bin/env bash
# MassDB OSS 安装包启停脚本测试（package/bin/massdb-oss-role.sh 与各角色 start/stop 脚本）。
#
#   package/test/scripts_test.sh
#
# 环境变量：
#   FAKE_WEED  预先编译好的 package/test/fakeweed（没有 Go 的机器上用）；不设时现场编译
#   REAL_WEED  可选，真实的 weed：额外验证 weed master 确实从角色 conf 目录读到 master.toml，
#              并能被 stop_master.sh 正常停止
# 不需要 root，不占用固定端口（真实 weed 用随机高端口）。
set -u

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# 取真实路径：macOS 的 $TMPDIR 在 /var（指向 /private/var 的软链接）下，脚本与 weed 记下的是解析后的路径
T=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/massdb-oss-scripts.XXXXXX")" && pwd -P)
PKG=$T/pkg
FAILED=0
PASSED=0
LEFTOVER=()

cleanup() {
	local pid
	for pid in ${LEFTOVER[@]+"${LEFTOVER[@]}"}; do
		kill -9 "$pid" 2>/dev/null
	done
	rm -rf "$T"
}
trap cleanup EXIT

ok() {
	PASSED=$((PASSED + 1))
	echo "ok   $*"
}

bad() {
	FAILED=$((FAILED + 1))
	echo "FAIL $*"
}

expect() { # expect <描述> <条件，eval 执行>
	if eval "$2"; then ok "$1"; else bad "$1"; fi
}

alive() {
	local stat
	stat=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
	case $stat in *Z*) return 1 ;; esac
	[ -n "$stat" ]
}

wait_dead() { # wait_dead <pid> <秒>
	local i
	for ((i = 0; i < $2 * 10; i++)); do
		alive "$1" || return 0
		sleep 0.1
	done
	return 1
}

record_args() { # 输出记录里的参数，一行一个
	sed -n 's/^arg=//p' "$1"
}

record_value() { # record_value <记录> <键>
	sed -n "s/^$2=//p" "$1"
}

if [ -z "${FAKE_WEED:-}" ]; then
	FAKE_WEED=$T/fakeweed
	(cd "$ROOT" && CGO_ENABLED=0 go build -o "$FAKE_WEED" ./package/test/fakeweed) || exit 1
fi

# 用 package/ 的源文件组一个安装包目录，bin/weed 换成假 weed
fresh_package() {
	rm -rf "$PKG"
	mkdir -p "$PKG/bin"
	cp "$ROOT/package/bin/massdb-oss-role.sh" "$PKG/bin/"
	cp "$1" "$PKG/bin/weed"
	chmod 755 "$PKG/bin/weed"
	local role
	for role in master volume filer s3; do
		mkdir -p "$PKG/$role"
		cp -R "$ROOT/package/$role/bin" "$ROOT/package/$role/conf" "$PKG/$role/"
	done
	mkdir -p "$T/data/master" "$T/data/volume1" "$T/data/volume2" "$T/data/filer"
}

export START_CHECK_SECONDS=1
export FAKE_WEED_RECORD=$T/record
unset FAKE_WEED_MODE

# ---- 1. 参数转换、全局参数位置、WEED_CONFIG_DIR ----
fresh_package "$FAKE_WEED"
printf '%s\r\n' \
	'# 注释行' \
	'' \
	'   port   =   9333   ' \
	'ip=10.0.0.1' \
	'logdir = '"$T/log/master" \
	'mdir = '"$T/data/master" \
	'peers = 10.0.0.1:9333,10.0.0.2:9333,10.0.0.3:9333' \
	'volumePreallocate = true' \
	'v = 1' \
	'defaultReplication = 001' \
	'license.file = /path/with=equals sign.jws' \
	'empty.value =' \
	'alsologtostderr = false' >"$PKG/master/conf/master.conf"
out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
expect "master 后台启动返回 0" '[ "$status" = 0 ]'
pid=$(cat "$PKG/master/bin/master.pid" 2>/dev/null)
expect "pid 文件是 weed 的进程号" '[ -n "$pid" ] && [ "$(record_value "$FAKE_WEED_RECORD" pid)" = "$pid" ]'
expect "进程存活" 'alive "$pid"'
expected="-logdir=$T/log/master
-v=1
-alsologtostderr=false
-config_dir=$PKG/master/conf
master
-port=9333
-ip=10.0.0.1
-mdir=$T/data/master
-peers=10.0.0.1:9333,10.0.0.2:9333,10.0.0.3:9333
-volumePreallocate=true
-defaultReplication=001
-license.file=/path/with=equals sign.jws"
actual=$(record_args "$FAKE_WEED_RECORD")
if [ "$actual" = "$expected" ]; then
	ok "参数转换：全局参数在子命令前、去空白与 CRLF、逗号多值、布尔、值含 = 与空格、空值跳过"
else
	bad "参数转换，实际参数：
$actual"
fi
expect "WEED_CONFIG_DIR 指向角色 conf 目录" '[ "$(record_value "$FAKE_WEED_RECORD" WEED_CONFIG_DIR)" = "$PKG/master/conf" ]'
expect "工作目录是角色目录" '[ "$(record_value "$FAKE_WEED_RECORD" cwd)" = "$PKG/master" ]'
expect "标准输出写 <logdir>/master.out" 'grep -q "fakeweed started" "$T/log/master/master.out"'
expect "master.out 记有启动命令行" 'grep -q "^==== .* start: $PKG/bin/weed -logdir=" "$T/log/master/master.out"'
if command -v setsid >/dev/null 2>&1; then
	expect "setsid：weed 在自己的会话里" '[ "$(ps -o sid= -p "$pid" | tr -d " ")" = "$pid" ]'
else
	echo "skip setsid（本机没有 setsid 命令，脚本退回只用 nohup）"
fi

# ---- 2. 重复启动被拒绝 ----
out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
expect "pid 存活时拒绝重复启动（返回 1）" '[ "$status" = 1 ]'
expect "拒绝时提示已在运行" 'grep -q "已在运行（pid $pid）" <<<"$out"'
expect "拒绝后 pid 文件不变" '[ "$(cat "$PKG/master/bin/master.pid")" = "$pid" ]'

# ---- 3. 停止与幂等 ----
out=$("$PKG/master/bin/stop_master.sh" 2>&1)
status=$?
expect "停止返回 0" '[ "$status" = 0 ]'
expect "进程已退出" 'wait_dead "$pid" 1'
expect "停止后删除 pid 文件" '[ ! -e "$PKG/master/bin/master.pid" ]'
expect "weed 收到的是 SIGTERM" 'grep -q "fakeweed: SIGTERM" "$T/log/master/master.out"'
out=$("$PKG/master/bin/stop_master.sh" 2>&1)
status=$?
expect "未运行时再停止返回 0" '[ "$status" = 0 ]'
expect "未运行时提示未在运行" 'grep -q "未在运行" <<<"$out"'

# ---- 4. 未设 config_dir / logdir 时的默认值；conf 里设了 config_dir 时两处一致 ----
printf 'mdir = %s\n' "$T/data/master" >"$PKG/master/conf/master.conf"
"$PKG/master/bin/start_master.sh" --daemon >/dev/null 2>&1
pid=$(cat "$PKG/master/bin/master.pid" 2>/dev/null)
actual=$(record_args "$FAKE_WEED_RECORD" | head -n 3 | tr '\n' ' ')
expect "未设 logdir 时用 <角色>/log，未设 config_dir 时用 <角色>/conf" '[ "$actual" = "-logdir=$PKG/master/log -config_dir=$PKG/master/conf master " ]'
expect "默认日志目录里有 master.out" '[ -f "$PKG/master/log/master.out" ]'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1
mkdir -p "$T/otherconf"
printf 'mdir = %s\nconfig_dir = %s\n' "$T/data/master" "$T/otherconf" >"$PKG/master/conf/master.conf"
"$PKG/master/bin/start_master.sh" --daemon >/dev/null 2>&1
expect "conf 设了 config_dir 时 -config_dir 与 WEED_CONFIG_DIR 都用它" '[ "$(record_value "$FAKE_WEED_RECORD" WEED_CONFIG_DIR)" = "$T/otherconf" ] && [ "$(record_args "$FAKE_WEED_RECORD" | grep -c -- "-config_dir=$T/otherconf")" = 1 ]'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1

# ---- 5. pid 残留 ----
printf 'mdir = %s\n' "$T/data/master" >"$PKG/master/conf/master.conf"
echo 999999 >"$PKG/master/bin/master.pid"
out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
pid=$(cat "$PKG/master/bin/master.pid" 2>/dev/null)
expect "pid 已不存在的残留 pid 文件：清理后启动" '[ "$status" = 0 ] && [ "$pid" != 999999 ] && alive "$pid"'
expect "提示清理了残留 pid 文件" 'grep -q "清理残留的 pid 文件" <<<"$out"'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1

sleep 300 &
other=$!
LEFTOVER+=("$other")
echo "$other" >"$PKG/master/bin/master.pid"
out=$("$PKG/master/bin/stop_master.sh" 2>&1)
status=$?
expect "pid 被别的进程复用：停止返回 0 且不向它发信号" '[ "$status" = 0 ] && alive "$other"'
expect "pid 被复用时删除 pid 文件" '[ ! -e "$PKG/master/bin/master.pid" ]'
echo "$other" >"$PKG/master/bin/master.pid"
out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
pid=$(cat "$PKG/master/bin/master.pid" 2>/dev/null)
expect "pid 被别的进程复用：启动不被拒绝，照常启动" '[ "$status" = 0 ] && [ "$pid" != "$other" ] && alive "$pid" && alive "$other"'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1
kill "$other" 2>/dev/null

echo 'not-a-pid' >"$PKG/master/bin/master.pid"
out=$("$PKG/master/bin/stop_master.sh" 2>&1)
status=$?
expect "内容无效的 pid 文件：停止时清理" '[ "$status" = 0 ] && [ ! -e "$PKG/master/bin/master.pid" ]'
echo 'garbage' >"$PKG/master/bin/master.pid"
"$PKG/master/bin/start_master.sh" --daemon >/dev/null 2>&1
expect "内容无效的 pid 文件：启动时清理" 'alive "$(cat "$PKG/master/bin/master.pid")"'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1

# ---- 6. 启动后立即退出 ----
out=$(FAKE_WEED_MODE=exit "$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
expect "weed 立即退出时启动返回 1" '[ "$status" = 1 ]'
expect "立即退出时删除 pid 文件" '[ ! -e "$PKG/master/bin/master.pid" ]'
expect "立即退出时打印 .out 末尾" 'grep -q "simulated startup failure" <<<"$out"'

# ---- 7. 停止超时：不发 SIGKILL，返回非 0 ----
FAKE_WEED_MODE=ignore-term "$PKG/master/bin/start_master.sh" --daemon >/dev/null 2>&1
pid=$(cat "$PKG/master/bin/master.pid")
LEFTOVER+=("$pid")
out=$("$PKG/master/bin/stop_master.sh" --timeout 2 2>&1)
status=$?
expect "SIGTERM 后超时返回 1" '[ "$status" = 1 ]'
expect "超时后进程仍在（没有 kill -9）" 'alive "$pid"'
expect "超时后保留 pid 文件" '[ "$(cat "$PKG/master/bin/master.pid")" = "$pid" ]'
out=$(STOP_TIMEOUT=1 "$PKG/master/bin/stop_master.sh" 2>&1)
status=$?
expect "STOP_TIMEOUT 环境变量生效" '[ "$status" = 1 ] && grep -q "最多等待 1 秒" <<<"$out"'
kill -9 "$pid"
wait_dead "$pid" 2
rm -f "$PKG/master/bin/master.pid"
expect "volume 默认等待 60 秒，其它角色 30 秒" 'grep -q "默认 60 秒" <<<"$("$PKG/volume/bin/stop_volume.sh" --help)" && grep -q "默认 30 秒" <<<"$("$PKG/filer/bin/stop_filer.sh" --help)"'

# ---- 8. 前台模式：exec 后 pid 不变 ----
"$PKG/master/bin/start_master.sh" >"$T/foreground.out" 2>&1 &
fg=$!
for _ in $(seq 1 50); do
	[ -s "$FAKE_WEED_RECORD" ] && [ "$(record_value "$FAKE_WEED_RECORD" pid)" = "$fg" ] && break
	sleep 0.1
done
expect "前台模式 exec weed，pid 文件就是脚本的 pid" '[ "$(cat "$PKG/master/bin/master.pid" 2>/dev/null)" = "$fg" ] && [ "$(record_value "$FAKE_WEED_RECORD" pid)" = "$fg" ]'
"$PKG/master/bin/stop_master.sh" >/dev/null 2>&1
wait "$fg" 2>/dev/null
expect "前台模式也能用停止脚本停下" '! alive "$fg"'

# ---- 9. 配置错误与目录检查 ----
conf_error() { # conf_error <描述> <期望提示> <conf 内容>
	printf '%s\n' "$3" >"$PKG/master/conf/master.conf"
	out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
	if [ $? = 1 ] && grep -q "$2" <<<"$out" && [ ! -e "$PKG/master/bin/master.pid" ]; then ok "$1"; else bad "$1: $out"; fi
}
conf_error "不是 key = value 的行报错" "第 2 行不是 key = value" "mdir = $T/data/master
port 9333"
conf_error "重复的键报错" "第 3 行重复设置了 port" "mdir = $T/data/master
port = 1
port = 2"
conf_error "无效的键报错" "键无效" "mdir = $T/data/master
-port = 1"
conf_error "master 必须设置 mdir" "必须设置 mdir" "port = 9333"
conf_error "mdir 不存在时报错，不自动创建" "目录不存在" "mdir = $T/data/missing"
expect "不自动创建数据目录" '[ ! -e "$T/data/missing" ]'
printf 'dir = %s,%s\n' "$T/data/volume1" "$T/data/volume-missing" >"$PKG/volume/conf/volume.conf"
out=$("$PKG/volume/bin/start_volume.sh" --daemon 2>&1)
status=$?
expect "volume 多个 dir 逐个检查" '[ "$status" = 1 ] && grep -q "volume-missing" <<<"$out"'
rm -f "$PKG/master/conf/master.conf"
out=$("$PKG/master/bin/start_master.sh" --daemon 2>&1)
status=$?
expect "缺少 conf 文件报错" '[ "$status" = 1 ] && grep -q "找不到配置文件" <<<"$out"'
out=$("$PKG/master/bin/start_master.sh" --bogus 2>&1)
status=$?
expect "未知参数返回 2" '[ "$status" = 2 ]'
out=$("$PKG/master/bin/stop_master.sh" --timeout x 2>&1)
status=$?
expect "无效的等待秒数返回 2" '[ "$status" = 2 ]'

# ---- 10. 包内默认 conf：四个角色都能转换，S3 内嵌在 filer ----
fresh_package "$FAKE_WEED"
for role in master volume filer s3; do
	conf=$PKG/$role/conf/$role.conf
	sed -i.bak -e "s#/data/massdb-oss-storage/massdb-oss-log/$role#$T/log/$role#" \
		-e "s#/data/massdb-oss-storage/massdb-oss-master#$T/data/master#" \
		-e "s#/data/massdb-oss-storage/massdb-oss-volume#$T/data/volume1#" \
		-e "s#/data/massdb-oss-storage/massdb-oss-filer#$T/data/filer#" "$conf" && rm -f "$conf.bak"
	out=$("$PKG/$role/bin/start_$role.sh" --daemon 2>&1)
	status=$?
	args=$(record_args "$FAKE_WEED_RECORD" | tr '\n' ' ')
	case $role in
	master) want="-logdir=$T/log/master -alsologtostderr=false -config_dir=$PKG/master/conf master -port=9333 -metricsPort=9380 -mdir=$T/data/master -volumePreallocate=true -volumeSizeLimitMB=5000 -defaultReplication=000 " ;;
	volume) want="-logdir=$T/log/volume -alsologtostderr=false -config_dir=$PKG/volume/conf volume -port=9340 -metricsPort=9381 -mserver=localhost:9333 -dir=$T/data/volume1 -index=leveldbLarge -max=0 -minFreeSpace=10GiB -concurrentUploadLimitMB=0 -concurrentDownloadLimitMB=0 " ;;
	filer) want="-logdir=$T/log/filer -alsologtostderr=false -config_dir=$PKG/filer/conf filer -port=9350 -metricsPort=9382 -master=localhost:9333 -defaultStoreDir=$T/data/filer -s3=true -s3.port=9010 -s3.config=conf/s3.json -concurrentUploadLimitMB=0 -saveToFilerLimit=0 " ;;
	s3) want="-logdir=$T/log/s3 -alsologtostderr=false -config_dir=$PKG/s3/conf s3 -port=9010 -metricsPort=9383 -ip.bind=0.0.0.0 -filer=localhost:9350 -config=conf/s3.json " ;;
	esac
	if [ "$status" = 0 ] && [ "$args" = "$want" ]; then ok "包内默认 $role.conf"; else bad "包内默认 $role.conf: $out
  实际: $args
  期望: $want"; fi
	"$PKG/$role/bin/stop_$role.sh" >/dev/null 2>&1
done

# ---- 11. 真实 weed：toml 从角色 conf 目录读取，SIGTERM 停止 ----
# weed 读到语法错误的 toml 会 Fatal 退出并报出文件路径，以此确认读的是哪个目录。
if [ -n "${REAL_WEED:-}" ]; then
	fresh_package "$REAL_WEED"
	port=$((20000 + RANDOM % 20000))
	printf '%s\n' "port = $port" "mdir = $T/data/master" "logdir = $T/log/real-master" \
		"ip = 127.0.0.1" "volumeSizeLimitMB = 100" "alsologtostderr = false" >"$PKG/master/conf/master.conf"
	cp "$PKG/master/conf/master.toml" "$T/master.toml.good"
	echo 'this is = = not toml [' >>"$PKG/master/conf/master.toml"
	out=$(START_CHECK_SECONDS=3 "$PKG/master/bin/start_master.sh" --daemon 2>&1)
	status=$?
	expect "weed 读的是角色 conf 目录里的 master.toml（坏文件导致启动失败并报出该路径）" '[ "$status" = 1 ] && grep -q "Reading $PKG/master/conf/master.toml" <<<"$out"'

	mkdir -p "$T/elsewhere"
	(cd "$T/elsewhere" && WEED_CONFIG_DIR=$PKG/master/conf exec "$REAL_WEED" -logdir="$T/elsewhere" master \
		-port=$((port + 1)) -ip=127.0.0.1 -mdir="$T/data/master" >"$T/elsewhere/out" 2>&1) &
	direct=$!
	LEFTOVER+=("$direct")
	wait_dead "$direct" 5
	# weed 的参数库 fla9 把 WEED_<参数名> 环境变量当作命令行没给的参数，WEED_CONFIG_DIR 单独也生效
	expect "只设 WEED_CONFIG_DIR 环境变量时 weed 也读该目录的 toml" '! alive "$direct" && grep -q "Reading $PKG/master/conf/master.toml" "$T/elsewhere/out"'
	kill "$direct" 2>/dev/null

	cp "$T/master.toml.good" "$PKG/master/conf/master.toml"
	START_CHECK_SECONDS=3 "$PKG/master/bin/start_master.sh" --daemon >"$T/real-start.out" 2>&1
	status=$?
	pid=$(cat "$PKG/master/bin/master.pid" 2>/dev/null)
	expect "真实 weed master 后台启动" '[ "$status" = 0 ] && alive "$pid"'
	LEFTOVER+=("$pid")
	for _ in $(seq 1 50); do
		curl -sf "http://127.0.0.1:$port/cluster/status" >/dev/null 2>&1 && break
		sleep 0.2
	done
	expect "master 端口可访问" 'curl -sf "http://127.0.0.1:$port/cluster/status" -o /dev/null'
	"$PKG/master/bin/stop_master.sh" >"$T/real-stop.out" 2>&1
	status=$?
	expect "stop_master.sh 用 SIGTERM 停下真实 weed" '[ "$status" = 0 ] && ! alive "$pid"'
	[ "$FAILED" = 0 ] || cat "$T/real-start.out" "$T/real-stop.out" "$T/log/real-master/master.out" 2>/dev/null
else
	echo "skip 真实 weed（设 REAL_WEED 时验证 master.toml 的读取与停止）"
fi

echo
echo "通过 $PASSED，失败 $FAILED"
[ "$FAILED" = 0 ]
