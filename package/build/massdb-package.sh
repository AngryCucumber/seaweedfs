#!/usr/bin/env bash
# 构建 MassDB OSS 安装包（make massdb-package 调用）。
#
#   VERSION      可选，必须等于代码版本（决策 D2：沿用上游 3.84，不注入新版本号）
#   ARCH         arm64、amd64 或两者（空格分隔），默认两者
#   TRUST        测试信任根：签发中心导出的信任清单（JSON 或其 base64url 串）；
#                生产信任清单（weed/license/trust.go）为空时必须给
#   LARGE_DISK   =1 时用 5BytesOffset 构建（大卷，包名带 _large_disk），与普通构建数据不通用
#   OUT          输出目录，默认 <仓库>/dist
#   ALLOW_DIRTY  =1 时允许工作区有未提交改动，注入的提交号带 -dirty
#
# 产物：<OUT>/massdb-oss-<版本>-linux-<arch>.tar.gz 及 .sha256，包内唯一顶层目录与包名相同
# （不含 .tar.gz）。包名不带提交号（2026-10-21 用户决定），构建身份看 package.json 的 commit。
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OUT=${OUT:-$ROOT/dist}
ARCHS=${ARCH:-arm64 amd64}

die() {
	echo "massdb-package: $*" >&2
	exit 1
}

pkginfo() {
	(cd "$ROOT" && go run ./package/tools/pkginfo "$@")
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@"
	else
		shasum -a 256 "$@"
	fi
}

commit=$(git -C "$ROOT" rev-parse HEAD)
if [ -n "$(git -C "$ROOT" status --porcelain)" ]; then
	[ "${ALLOW_DIRTY:-}" = 1 ] || die "工作区有未提交的改动；确需打包时加 ALLOW_DIRTY=1（提交号带 -dirty）"
	commit=$commit-dirty
fi

version=$(pkginfo version)
if [ -n "${VERSION:-}" ] && [ "$VERSION" != "$version" ]; then
	die "VERSION=$VERSION 与代码版本 $version 不符；版本号沿用上游，不另注入（决策 D2）"
fi

trust_b64=
if [ -n "${TRUST:-}" ]; then
	[ -f "$TRUST" ] || die "找不到信任清单 $TRUST"
	trust_b64=$(pkginfo trust-b64 "$TRUST")
fi

tags=
suffix=
large=false
if [ "${LARGE_DISK:-}" = 1 ]; then
	tags=5BytesOffset
	suffix=_large_disk
	large=true
fi

if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	tar_owner=(--owner=0 --group=0 --numeric-owner)
else
	tar_owner=(--uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata)
fi

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
for arch in $ARCHS; do
	case $arch in
	arm64 | amd64) ;;
	*) die "不支持的 ARCH: $arch（arm64 或 amd64）" ;;
	esac
	name=massdb-oss-$version-linux-$arch$suffix
	stage=$OUT/stage/$name
	rm -rf "$stage" "$OUT/$name.tar.gz" "$OUT/$name.tar.gz.sha256"
	mkdir -p "$stage/bin"

	ldflags="-s -w -extldflags -static -X github.com/seaweedfs/seaweedfs/weed/util.COMMIT=$commit"
	if [ -n "$trust_b64" ]; then
		ldflags="$ldflags -X github.com/seaweedfs/seaweedfs/weed/license.testTrustManifest=$trust_b64"
	fi
	echo "massdb-package: 构建 $name"
	(cd "$ROOT/weed" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -tags "$tags" -ldflags "$ldflags" -o "$stage/bin/weed" .)

	install -m 644 "$ROOT/package/bin/massdb-oss-role.sh" "$stage/bin/"
	for role in master volume filer s3; do
		mkdir -p "$stage/$role/bin" "$stage/$role/conf"
		install -m 755 "$ROOT/package/$role/bin/start_$role.sh" "$ROOT/package/$role/bin/stop_$role.sh" "$stage/$role/bin/"
		for f in "$ROOT/package/$role/conf/"*; do
			install -m 644 "$f" "$stage/$role/conf/"
		done
	done

	args=(-commit "$commit" -arch "$arch")
	[ "$large" = true ] && args+=(-large-disk)
	[ -n "$trust_b64" ] && args+=(-trust-b64 "$trust_b64")
	pkginfo package-json "${args[@]}" >"$stage/package.json"
	pkginfo check-binary "$stage/bin/weed" "$stage/package.json"

	COPYFILE_DISABLE=1 tar "${tar_owner[@]}" -C "$OUT/stage" -czf "$OUT/$name.tar.gz" "$name"
	(cd "$OUT" && sha256 "$name.tar.gz" >"$name.tar.gz.sha256")
	rm -rf "$stage"
	echo "massdb-package: $OUT/$name.tar.gz"
done
rmdir "$OUT/stage" 2>/dev/null || true
