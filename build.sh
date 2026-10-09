#!/bin/bash
# 一键构建：编译桥接二进制 → 打进 fpk → 输出到 dist/
#
# 依赖：
#   - Go 1.24+（或把工具链解到 toolchain/ 并设置 GOROOT）
#   - 飞牛官方 fnpack（https://developer.fnnas.com/docs/cli/fnpack/）
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
APPNAME="fnos-emby-bridge"
FPK_DIR="${ROOT}/fpk/${APPNAME}"
BIN_OUT="${ROOT}/dist/${APPNAME}-patched"

# 若仓库内自带工具链则优先使用
if [ -x "${ROOT}/toolchain/go/bin/go" ]; then
    export GOROOT="${ROOT}/toolchain/go"
    export PATH="${GOROOT}/bin:${PATH}"
fi
export GOFLAGS="${GOFLAGS:--mod=mod}"

command -v go >/dev/null 2>&1 || { echo "找不到 go，请先安装 Go 1.24+ 或把工具链放到 toolchain/"; exit 1; }

mkdir -p "${ROOT}/dist"

echo "==> 校验 + 测试 + 编译 ${APPNAME}"
(
    cd "${ROOT}"
    go vet ./...
    go test ./...
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags="-s -w" -o "${BIN_OUT}" ./cmd/${APPNAME}
)
echo "    产物：${BIN_OUT}（$(stat -c%s "${BIN_OUT}") 字节）"

echo "==> 放入打包工程"
mkdir -p "${FPK_DIR}/app/server"
cp -f "${BIN_OUT}" "${FPK_DIR}/app/server/${APPNAME}"
chmod 755 "${FPK_DIR}/app/server/${APPNAME}"

echo "==> fnpack build"
if ! command -v fnpack >/dev/null 2>&1; then
    echo "找不到 fnpack，跳过打包。二进制已生成：${BIN_OUT}"
    exit 0
fi
(
    cd "${ROOT}/fpk"
    fnpack build --directory "./${APPNAME}"
    cp -f "${APPNAME}.fpk" "${ROOT}/dist/${APPNAME}.fpk"
)
echo "==> 完成"
ls -la "${ROOT}/dist"
