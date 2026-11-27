#!/bin/bash
# 从唯一图标源 fpk/图标.png 生成各处需要的 PNG 尺寸。
#
# 图标只在 fpk/图标.png 维护一份（已抠掉白底，带 alpha），后台界面 / fpk / 仓库说明
# 都用它派生出来的文件；换图标时替换源图后重跑本脚本即可（生成的 PNG 已入库，
# CI 不需要 ffmpeg）。
#
# 源图由 fpk/图标.jpg 经 tools/iconalpha 抠白生成（背景与桥形均透明）：
#   go run ./tools/iconalpha -in fpk/图标.jpg -out fpk/图标.png -all
#
# 依赖：ffmpeg（生成）——只有需要重新生成时才要装。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
SRC="${ROOT}/图标.png"
APP="${ROOT}/fnos-emby-bridge"

[ -f "${SRC}" ] || { echo "找不到图标源文件：${SRC}"; exit 1; }
command -v ffmpeg >/dev/null 2>&1 || { echo "需要 ffmpeg 来生成图标"; exit 1; }

gen() { # gen <尺寸> <输出>
    ffmpeg -y -loglevel error -i "${SRC}" \
        -vf "scale=$1:$1:flags=lanczos" -pix_fmt rgba "$2"
    echo "  $1×$1 → ${2#${ROOT}/}  ($(stat -c%s "$2") 字节)"
}

echo "==> 源图：$(ffprobe -v error -select_streams v -show_entries stream=width,height \
    -of csv=p=0 "${SRC}")  图标.png"

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

# 后台界面（internal/admin/admin_html.go）内嵌的是 base64 data URI，
# 这里直接把生成的 256px 图标写回去，避免两处图标不一致。
echo "==> 同步后台界面内嵌图标"
python3 - "$ROOT/../internal/admin/admin_html.go" "${ROOT}/../docs/icon.png" <<'PYEOF'
import base64, re, sys
go_file, png_file = sys.argv[1], sys.argv[2]
data = base64.b64encode(open(png_file, 'rb').read()).decode()
src = open(go_file, encoding='utf-8').read()
new, n = re.subn(r'const iconDataURI = "data:image/png;base64,[^"]*"',
                 'const iconDataURI = "data:image/png;base64,%s"' % data, src)
if n != 1:
    sys.exit('未找到 iconDataURI（期望正好一处，实际 %d）' % n)
open(go_file, 'w', encoding='utf-8').write(new)
print('   internal/admin/admin_html.go 已更新（%d 字节 base64）' % len(data))
PYEOF

echo "==> 完成"
