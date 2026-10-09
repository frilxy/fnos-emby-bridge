#!/bin/bash
# 一键构建：校验 + 测试 → 编译桥接 → 打 fpk → 输出到 dist/
#
# 依赖：
#   - Go 1.24+（或把工具链解到 toolchain/，脚本会自动优先使用它）
#   - 打包：装了飞牛官方 fnpack 就用它，没装则用标准 tar 复刻同样的结构
#     （见 fpk/pack.sh）——因此 CI 不需要额外下载 fnpack。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
APPNAME="fnos-emby-bridge"
FPK_DIR="${ROOT}/fpk/${APPNAME}"
DIST="${ROOT}/dist"
BIN_OUT="${DIST}/${APPNAME}-patched"

# 仓库内 / 与仓库同级的 toolchain/ 里有 Go 就优先使用（CI 走 setup-go，不影响）
for candidate in "${ROOT}/toolchain/go" "$(cd "${ROOT}/.." && pwd)/toolchain/go"; do
    if [ -x "${candidate}/bin/go" ]; then
        export GOROOT="${candidate}"
        export PATH="${GOROOT}/bin:${PATH}"
        break
    fi
done
export GOFLAGS="${GOFLAGS:--mod=mod}"

# 受限环境（如 NAS 上 HOME 不可写）时，把构建缓存与模块缓存退到仓库同级目录；
# CI 里 HOME 可写，不会触发这段。
writable() { mkdir -p "$1" 2>/dev/null && [ -w "$1" ]; }
SIDE="$(cd "${ROOT}/.." && pwd)"
if [ -z "${GOCACHE:-}" ] && ! writable "${HOME:-/tmp}/.cache/go-build"; then
    export GOCACHE="${SIDE}/gocache"
    writable "${GOCACHE}"
fi
if [ -z "${GOPATH:-}" ] && ! writable "${HOME:-/tmp}/go"; then
    export GOPATH="${SIDE}/gopath"
    writable "${GOPATH}"
fi

command -v go >/dev/null 2>&1 || {
    echo "找不到 go，请先安装 Go 1.24+，或把工具链解到 toolchain/" >&2
    exit 1
}

VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "${FPK_DIR}/manifest" | head -1)"
[ -n "${VERSION}" ] || { echo "读不到 ${FPK_DIR}/manifest 里的 version" >&2; exit 1; }

mkdir -p "${DIST}"

echo "==> 校验 + 测试 + 编译 ${APPNAME} v${VERSION}"
(
    cd "${ROOT}"
    go vet ./...
    go test ./...
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-s -w" -o "${BIN_OUT}" "./cmd/${APPNAME}"
)
echo "    产物：${BIN_OUT}（$(stat -c%s "${BIN_OUT}") 字节）"

echo "==> 放入打包工程"
mkdir -p "${FPK_DIR}/app/server"
cp -f "${BIN_OUT}" "${FPK_DIR}/app/server/${APPNAME}"
chmod 755 "${FPK_DIR}/app/server/${APPNAME}"

echo "==> 生成 fpk"
# 只产出带版本号的那一个：文件本身就表明版本，Release 里也只放这一个
"${ROOT}/fpk/pack.sh" "${APPNAME}" "${DIST}/${APPNAME}-${VERSION}.fpk"

echo "==> 完成"
ls -la "${DIST}"
