#!/bin/bash
# 模拟 Emby 官方客户端连接序列，逐端点探测桥接（真机部署后对着真实桥接跑一遍即可自检）。
# 用法：BRIDGE=http://192.168.1.10:8096 bash scripts/emby-client-probe.sh
BRIDGE="${BRIDGE:-http://127.0.0.1:8096}"
show() { # <描述> <curl参数...>
  local desc="$1"; shift
  local out
  out=$(curl -s -o /tmp/resp.txt -w "%{http_code}" --max-time 8 "$@")
  printf "%-52s HTTP %s  %s\n" "$desc" "$out" "$(head -c 80 /tmp/resp.txt | tr -d '\n')"
}

echo "===== 1. 服务器探测 ====="
show "GET /emby/System/Info/Public" "$BRIDGE/emby/System/Info/Public"
show "GET /System/Info/Public"      "$BRIDGE/System/Info/Public"

echo "===== 2. 登录 ====="
show "POST /emby/Users/AuthenticateByName" -X POST "$BRIDGE/emby/Users/AuthenticateByName" \
  -H 'Content-Type: application/json' -d '{"UserName":"'"${FNOS_USER:-admin}"'","Pwd":"'"${FNOS_PASS:-pass}"'"}'
TOKEN=$(python3 -c "
import json,sys
try: d=json.load(open('/tmp/resp.txt'))
except: sys.exit()
print(d.get('AccessToken') or d.get('Token') or '')
")
echo "  -> AccessToken: ${TOKEN:-无}"
[ -z "$TOKEN" ] && { echo "登录失败，后续跳过"; exit 1; }

echo "===== 3. 媒体库浏览 ====="
show "GET /emby/Users/{uid}/Views" "$BRIDGE/emby/Users/fnos-user/Views?api_key=$TOKEN"
show "GET /emby/Items?Recursive=true" "$BRIDGE/emby/Items?Recursive=true&api_key=$TOKEN"

echo "===== 4. 播放协商 ====="
ITEM=$(curl -s "$BRIDGE/emby/Items?Limit=1&api_key=$TOKEN" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print(d['Items'][0]['Id'] if d.get('Items') else '')")
echo "  -> 测试条目 Id: ${ITEM:-无}"
[ -n "$ITEM" ] && show "POST /emby/Items/{id}/PlaybackInfo" -X POST "$BRIDGE/emby/Items/$ITEM/PlaybackInfo?api_key=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"AutoOpenLiveStream":true}'

echo "===== 5. 取流（Range 续传）====="
[ -n "$ITEM" ] && show "GET /emby/Videos/{id}/stream?Static=true" -H "Range: bytes=0-1023" \
  "$BRIDGE/emby/Videos/$ITEM/stream?Static=true&MediaSourceId=$ITEM&api_key=$TOKEN" -o /tmp/stream.bin
[ -f /tmp/stream.bin ] && echo "  -> 收到 $(stat -c%s /tmp/stream.bin) 字节"

echo "===== 6. 海报 ====="
[ -n "$ITEM" ] && show "GET /emby/Items/{id}/Images/Primary" "$BRIDGE/emby/Items/$ITEM/Images/Primary?api_key=$TOKEN" -o /dev/null

echo "===== 7. 进度回传 ====="
show "POST /emby/Sessions/Playing/Progress" -X POST "$BRIDGE/emby/Sessions/Playing/Progress?api_key=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"ItemId":"'"${ITEM:-fv_001}"'","PositionTicks":600000000}'
show "POST /emby/Items/{id}/Progress" -X POST "$BRIDGE/emby/Items/${ITEM:-fv_001}/Progress?api_key=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"PositionTicks":600000000}'
