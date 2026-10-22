#!/usr/bin/env bash
# 构建 MassDB OSS 元数据库安装包（make massdb-meta-package 调用），种类 massdb-oss-meta，
# 技术来源 Apache Cassandra。
#
#   CASSANDRA_TARBALL  Apache Cassandra 5.0.x 官方二进制包 apache-cassandra-5.0.x-bin.tar.gz
#   JMX_EXPORTER_JAR   jmx_prometheus_javaagent-<版本>.jar
#   OUT                输出目录，默认 <仓库>/dist
#
# 产物：<OUT>/massdb-oss-meta-<Cassandra 版本>.tar.gz 及 .sha256，包内唯一顶层目录与包名相同：
#   package.json
#   cassandra/                       官方包原样解压，conf/cassandra.yaml 与 jvm17-server.options 套用
#                                    package/meta/patches 的默认值
#   cassandra/bin/start_cassandra.sh stop_cassandra.sh massdb-cassandra-common.sh
#   cassandra/conf/massdb-cassandra.conf jmx-exporter.yaml
#   cassandra/lib/jmx_prometheus_javaagent.jar
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OUT=${OUT:-$ROOT/dist}
META=$ROOT/package/meta

die() {
	echo "massdb-meta-package: $*" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

[ -f "${CASSANDRA_TARBALL:-}" ] || die "CASSANDRA_TARBALL 必须指向 apache-cassandra-5.0.x-bin.tar.gz"
[ -f "${JMX_EXPORTER_JAR:-}" ] || die "JMX_EXPORTER_JAR 必须指向 jmx_prometheus_javaagent-<版本>.jar"

top=$(tar -tzf "$CASSANDRA_TARBALL" | head -n 1 || true)
top=${top%%/*}
case $top in
apache-cassandra-5.0.[0-9]*) cassandra_version=${top#apache-cassandra-} ;;
*) die "$CASSANDRA_TARBALL 不是 Apache Cassandra 5.0.x 二进制包（顶层目录 $top）" ;;
esac
case $cassandra_version in
*[!0-9.]*) die "无法识别的 Cassandra 版本 $cassandra_version" ;;
esac

jar_name=$(basename "$JMX_EXPORTER_JAR")
case $jar_name in
jmx_prometheus_javaagent-*.jar)
	jmx_version=${jar_name#jmx_prometheus_javaagent-}
	jmx_version=${jmx_version%.jar}
	;;
*) die "JMX_EXPORTER_JAR 文件名应为 jmx_prometheus_javaagent-<版本>.jar" ;;
esac

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	tar_owner=(--owner=0 --group=0 --numeric-owner)
else
	tar_owner=(--uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata)
fi

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
name=massdb-oss-meta-$cassandra_version
stage=$OUT/stage/$name
rm -rf "$stage" "$OUT/$name.tar.gz" "$OUT/$name.tar.gz.sha256"
mkdir -p "$stage"
echo "massdb-meta-package: 构建 $name"
tar -xzf "$CASSANDRA_TARBALL" -C "$stage"
mv "$stage/$top" "$stage/cassandra"
home=$stage/cassandra
[ -x "$home/bin/cassandra" ] && [ -f "$home/lib/apache-cassandra-$cassandra_version.jar" ] ||
	die "$CASSANDRA_TARBALL 里缺少 bin/cassandra 或 lib/apache-cassandra-$cassandra_version.jar"

for patch_file in "$META/patches/"*.patch; do
	patch -p1 -F0 -s -d "$home" <"$patch_file" || die "$(basename "$patch_file") 套用失败，Cassandra $cassandra_version 的默认配置与补丁不符，需重新生成补丁"
done
find "$home/conf" \( -name '*.orig' -o -name '*.rej' \) -delete
# 计划要求的默认值：5.0 原版已是这两个值，换版本时确认没有变
grep -qx 'num_tokens: 16' "$home/conf/cassandra.yaml" || die "cassandra.yaml 的 num_tokens 不是 16"
grep -qx 'allocate_tokens_for_local_replication_factor: 3' "$home/conf/cassandra.yaml" ||
	die "cassandra.yaml 的 allocate_tokens_for_local_replication_factor 不是 3"

install -m 755 "$META/cassandra/bin/start_cassandra.sh" "$META/cassandra/bin/stop_cassandra.sh" "$home/bin/"
install -m 644 "$META/cassandra/bin/massdb-cassandra-common.sh" "$home/bin/"
install -m 644 "$META/cassandra/conf/massdb-cassandra.conf" "$META/cassandra/conf/jmx-exporter.yaml" "$home/conf/"
install -m 644 "$JMX_EXPORTER_JAR" "$home/lib/jmx_prometheus_javaagent.jar"

cat >"$stage/package.json" <<EOF
{
  "schemaVersion": 1,
  "kind": "massdb-oss-meta",
  "product": "MassDB OSS Meta",
  "cassandraVersion": "$cassandra_version",
  "javaVersion": 17,
  "jmxExporterVersion": "$jmx_version",
  "roles": ["cassandra"]
}
EOF

COPYFILE_DISABLE=1 tar "${tar_owner[@]}" -C "$OUT/stage" -czf "$OUT/$name.tar.gz" "$name"
(cd "$OUT" && sha256 "$name.tar.gz" >"$name.tar.gz.sha256")
rm -rf "$stage"
rmdir "$OUT/stage" 2>/dev/null || true
echo "massdb-meta-package: $OUT/$name.tar.gz"
