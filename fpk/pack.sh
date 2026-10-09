#!/bin/bash
# 把 fpk/<appname>/ 打成 <appname>.fpk。
#
# fpk 本质就是一个 ustar tar.gz：
#     app.tgz  cmd/  config/  ICON.PNG  ICON_256.PNG  manifest  wizard/
# 其中 app.tgz 又是 app/ 的内容，再附一份顶层 config/ 的副本。
#
# 优先用飞牛官方 fnpack（装了就用，产物出自飞牛工具链）；
# 没装时用标准 tar 复刻同样的结构 —— 因此 CI 不需要额外下载 fnpack。
#
# 用法：./pack.sh [appname] [输出路径]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
APP="${1:-fnos-emby-bridge}"
SRC="${ROOT}/${APP}"
OUT="${2:-$(cd "${ROOT}/.." && pwd)/dist/${APP}.fpk}"

[ -d "${SRC}" ] || { echo "找不到打包目录：${SRC}" >&2; exit 1; }
[ -f "${SRC}/manifest" ] || { echo "缺少 ${SRC}/manifest" >&2; exit 1; }

mkdir -p "$(dirname "${OUT}")"

if command -v fnpack >/dev/null 2>&1; then
    echo "==> fnpack build（飞牛官方工具）"
    rm -f "${ROOT}/${APP}.fpk"
    ( cd "${ROOT}" && fnpack build --directory "./${APP}" >/dev/null )
    [ -f "${ROOT}/${APP}.fpk" ] || { echo "fnpack 没有产出 ${APP}.fpk" >&2; exit 1; }
    mv -f "${ROOT}/${APP}.fpk" "${OUT}"
else
    echo "==> 用标准 tar 复刻 fpk 结构（未安装 fnpack）"
    TMP="$(mktemp -d)"
    trap 'rm -rf "${TMP}"' EXIT
    PKG="${TMP}/pkg"
    mkdir -p "${PKG}"

    # 外层成员
    cp -a "${SRC}/manifest" "${SRC}/cmd" "${SRC}/config" \
          "${SRC}/ICON.PNG" "${SRC}/ICON_256.PNG" "${SRC}/wizard" "${PKG}/"

    # app.tgz = app/ 内容 + 顶层 config/ 的副本
    APPSTAGE="${TMP}/app"
    mkdir -p "${APPSTAGE}"
    cp -a "${SRC}/app/." "${APPSTAGE}/"
    cp -a "${SRC}/config" "${APPSTAGE}/config"

    ( cd "${APPSTAGE}" && find . -mindepth 1 -printf '%P\n' | LC_ALL=C sort ) > "${TMP}/applist"
    tar --format=ustar --owner=0 --group=0 --numeric-owner -cf - \
        -C "${APPSTAGE}" -T "${TMP}/applist" | gzip -9n > "${PKG}/app.tgz"

    ( cd "${PKG}" && find . -mindepth 1 -printf '%P\n' | LC_ALL=C sort ) > "${TMP}/list"
    tar --format=ustar --owner=0 --group=0 --numeric-owner -cf - \
        -C "${PKG}" -T "${TMP}/list" | gzip -9n > "${OUT}"
fi

echo "    产物：${OUT}（$(stat -c%s "${OUT}") 字节）"
