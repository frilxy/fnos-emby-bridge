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
    # --no-recursion 必须加：否则 tar 会把列表里的目录名再递归展开一遍，
    # 与显式列出的文件重复（成员出现两次、目录项还带尾斜杠）
    tar --format=ustar --no-recursion --owner=0 --group=0 --numeric-owner -cf - \
        -C "${APPSTAGE}" -T "${TMP}/applist" | gzip -9n > "${PKG}/app.tgz"

    # fnpack 会在 manifest 末尾写入 checksum = md5(app.tgz)，安装器可能校验它
    # （实测确认就是这个值；每次构建因 app.tgz 内容不同而变化，属包内自洽校验）。
    # 用 tar 打包时必须自己补上，否则可能有被安装器拒绝的风险。
    sed -i '/^checksum[[:space:]]*=/d' "${PKG}/manifest"
    printf '%-22s= %s\n' checksum "$(md5sum "${PKG}/app.tgz" | cut -d' ' -f1)" \
        >> "${PKG}/manifest"

    ( cd "${PKG}" && find . -mindepth 1 -printf '%P\n' | LC_ALL=C sort ) > "${TMP}/list"
    tar --format=ustar --no-recursion --owner=0 --group=0 --numeric-owner -cf - \
        -C "${PKG}" -T "${TMP}/list" | gzip -9n > "${OUT}"
fi

echo "    产物：${OUT}（$(stat -c%s "${OUT}") 字节）"
