#!/bin/bash
# 从唯一图标源 fpk/图标.jpg 生成各处需要的 PNG 尺寸。
#
# 图标只在 fpk/图标.jpg 维护一份，后台界面 / fpk / 仓库说明都用它派生出来的文件，
# 换图标时只替换源图后重跑本脚本即可（生成的 PNG 已入库，CI 不需要 ffmpeg）。
#
# 依赖：ffmpeg（生成）——只有需要重新生成时才要装。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
SRC="${ROOT}/图标.jpg"
APP="${ROOT}/fnos-emby-bridge"

[ -f "${SRC}" ] || { echo "找不到图标源文件：${SRC}"; exit 1; }
command -v ffmpeg >/dev/null 2>&1 || { echo "需要 ffmpeg 来生成图标"; exit 1; }

gen() { # gen <尺寸> <输出>
    ffmpeg -y -loglevel error -i "${SRC}" \
        -vf "scale=$1:$1:flags=lanczos" -pix_fmt rgb24 "$2"
    echo "  $1×$1 → ${2#${ROOT}/}  ($(stat -c%s "$2") 字节)"
}

echo "==> 源图：$(ffprobe -v error -select_streams v -show_entries stream=width,height \
    -of csv=p=0 "${SRC}")  图标.jpg"

# fpk 安装包图标（fnOS 约定：ICON.PNG 64×64、ICON_256.PNG 256×256）
gen 64  "${APP}/ICON.PNG"
gen 256 "${APP}/ICON_256.PNG"

# 应用中心/桌面入口用的图标
gen 64  "${APP}/app/ui/images/icon_64.png"
gen 256 "${APP}/app/ui/images/icon_256.png"

# 仓库说明（README）与后台界面内嵌用图
mkdir -p "${ROOT}/../docs"
gen 256 "${ROOT}/../docs/icon.png"
gen 128 "${ROOT}/../docs/icon-128.png"

echo "==> 完成"
