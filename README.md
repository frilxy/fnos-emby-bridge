# fnos-emby-bridge

把"飞牛影视"伪装成 Emby 服务端：前端讲 **Emby 协议**给 Emby 官方播放器，后端调 **飞牛影视私有接口**取数据。

用途：服务端要用飞牛影视（不想装 Emby Server），但客户端想用 Emby 官方播放器。本桥接补上中间那层协议翻译。

**状态：真机联调通过（2026-09 实测）**。支持多版本播放、多音轨/字幕元数据、后台账号登录、自定义后台安全路径。

## 原理

```
Emby 官方播放器 ──Emby 协议──> fnos-emby-bridge ──飞牛 /v/api/*──> 飞牛影视
                                    │
                                    └── 媒体字节流（本地 media/range / 云盘直链）
```

## 端点映射（0.9.8 真机契约）

| Emby 端点 | 飞牛接口 |
|---|---|
| `POST /Users/AuthenticateByName` | `POST /v/api/v2/user/loginByPassword`（密码 **SHA256 hex**；响应含 `AccessToken`） |
| `GET /System/Info` / `Public` / `Branding/*` | （桥接自造） |
| `GET /Users/{uid}/Views` | `GET /v/api/v1/mediadb/list`（自动列全部媒体库，无需配置） |
| `GET /Items?ParentId=库guid` | `POST /v/api/v1/item/list` `{ancestor_guid,...}` |
| `GET /Items?ParentId=剧/季guid` | 同上 `{parent_guid,...}`（两种模式互斥，桥接自动回退） |
| `GET /Items/{id}` | `GET /v/api/v1/item/{guid}` |
| `POST /Items/{id}/PlaybackInfo` | `POST /v/api/v1/play/info` + 声明 DirectPlay 能力（官方播放入口） |
| `POST /Videos/{id}/Playback` | 老端点兜底 |
| `GET /Videos/{id}/stream`（大小写均可） | `POST /v/api/v1/stream`（云盘直链）或 `GET /v/api/v1/media/range/{media_guid}?direct_link_quality_index=1`（本地透传，**必须带 authx**） |
| `GET /Items/{id}/Images/Primary` | `GET /v/api/v1/sys/img{poster}?w=400`（只需 `mode=relay` cookie；失败回 1x1 PNG 兜底） |
| `POST /Sessions/Playing[/Progress|/Stopped]`、`POST /Items/{id}/Progress` | `POST /v/api/v1/play/record` |

> 所有端点同时兼容 **`/emby` 前缀**（Emby 官方 Android/iOS/TV/Web 客户端全部请求带 `/emby/...`）。

## 飞牛 0.9.8 接口契约要点（逆向实测）

- **authx 签名**（所有 `/v/api/v1` 请求必须）：
  `sign = MD5(apiKey + "_" + path(不含query) + "_" + nonce + "_" + ts毫秒 + "_" + MD5(body) + "_" + apiSecret)`
  header：`authx: nonce=<6位>&timestamp=<毫秒>&sign=<md5>`；GET 请求 bodyHash=`MD5("")`
  固定密钥：`apiKey=NDzZTVxnRKP8Z0jXg1VAMonaG8akvh`，`apiSecret=16CCEB3D-AB42-077D-36A1-F355324E4237`
- **nonce 不能以 0 开头**：前导零 nonce 服务端返回 `code=5000 invalid sign`（间歇性失败陷阱，已修复：nonce 取值 100000-999999）
- **无 token 的请求也要带 Cookie 头**（值任意，`mode=relay` 即可），否则 FN 反代把 `/v/api/*` 兜底成 SPA HTML
- **登录**：`POST /v/api/v2/user/loginByPassword` `{app_name:"trimemedia-web", username, password: sha256hex}`；token 双通道传递：`Authorization: <token>` + `Cookie: Trim-MC-token=<token>; mode=relay`
- **item/list 两种互斥模式**：库 guid 只认 `ancestor_guid`，剧/季 guid 只认 `parent_guid`（桥接先试 ancestor，空则回退 parent）
- **夸克云盘**（cloud_storage_type=4）：`stream` 返回 `direct_link_qualities[0].url` + `header.Cookie`/`User-Agent`，直链必须带该 Cookie（否则 412），支持任意 Range

## 播放路径（云盘 / 本地自动分流）

`GET /Videos/{id}/stream` 先调 `POST /v/api/v1/stream` 判断 `cloud_storage_info`：

- **本地 NAS**（null）：`media/range` 透传，支持 Range 断点续传；上游忽略 Range 回 200 全量时桥接自行裁剪补 206
- **云盘直链**：直连 CDN，行为与飞牛官方 fntv-electron 代理一致——
  - 带上响应里的 `header.Cookie` / `header.User-Agent`
  - **115**（type=3）：全局限速 1 req/s
  - **夸克**（type=4）：10MiB 固定分块 + 串行"边下边播"防风控；探测失败自动退化透明透传；上游 200（无视 Range）按偏移裁剪
  - **阿里/百度/123**（type=1/2/5）：透明代理 + 302 跟随（最多 3 跳）

## 构建

```bash
cd fnos-emby-bridge
go build -o fnos-emby-bridge ./cmd/fnos-emby-bridge

# Docker（多架构含 ARM，飞牛常见 RK3588 可跑）
docker buildx build --platform linux/amd64,linux/arm64 -t fnos-emby-bridge:latest .
```

## 运行

```bash
FNOS_BASE=http://192.168.1.10:5666 \
FNOS_USER=admin FNOS_PASS=xxx \
PORT=8096 HOST=192.168.1.10:8096 \
./fnos-emby-bridge
```

## 配置项（环境变量）

| 变量 | 必填 | 说明 |
|---|---|---|
| `FNOS_BASE` | 是 | 飞牛影视服务端 base URL（含端口） |
| `FNOS_USER` | 是 | 飞牛影视账号 |
| `FNOS_PASS` | 否 | 飞牛影视密码（空则走 cookie 态） |
| `PORT` | 否 | 桥接监听端口，默认 8096 |
| `HOST` | 否 | 客户端访问桥接的 `host:port`，拼绝对 URL 用 |
| `SERVER_NAME` | 否 | 显示在 Emby 客户端的服务端名 |
| `ADMIN_USER` | 否 | `/admin` 后台账号，默认 `admin` |
| `ADMIN_PASS` | 否 | `/admin` 后台密码，默认 `admin123`，生产环境请修改 |

> 0.9.8 起媒体库通过 `mediadb/list` 自动列出，**无需 SEED_GUIDS**。

Docker Compose 可先不填飞牛地址和账号，启动后打开 `/admin` 在后台填写并保存。

## Docker / GHCR

推送 `main` 或 `v*` 标签后，GitHub Actions 会自动构建并发布多架构镜像到：

```text
ghcr.io/<你的 GitHub 用户名>/<仓库名>:latest
```

## 测试

```bash
go test ./...
```

mock 飞牛（完整模拟 0.9.8 契约：v2 登录 / mediadb / item/list 双模式 / sys/img 海报）验证桥接端点全链路，含 4 组测试：基础端点、云盘直链、夸克 200 裁剪回归、0.9.8 新契约（Views/下钻/海报/详情）。

## 多版本播放

飞牛通过 `GET /v/api/v1/stream/list/{episodeGuid}` 返回同一集的全部文件版本。桥接把这些版本映射成 Emby 标准的 `MediaSources[]`，客户端可在版本菜单中选择。每个版本携带 `Id=media_guid`、真实文件名/大小/码率/视频轨信息，播放链接带 `Version=<media_guid>`。同一版本的 `audio_streams` 和 `subtitle_streams` 也会映射成 Emby `MediaStreams`，客户端可正常显示和切换音频、字幕。

`GET /Videos/{id}/stream` 会优先读取 `Version` 或 `MediaSourceId`；未带版本参数时继续使用 `play/info` 返回的默认版本，保证旧客户端和继续观看路径不变。

另有本地联调启动器：

```bash
go run ./cmd/verify   # mock 飞牛 + 桥接同起，桥接 :8096
```

## 已知限制

- **画质选择**：云盘直链固定取 `direct_link_qualities[0]`，多画质档位未实现选择。
- **夸克网盘分块代理**：10MiB 固定分块 + 串行（1 并发），防风控。网盘断连/限速表现为播放卡顿，属预期。
- **`stream` 接口 ip 参数**：固定 32 位 hex 设备指纹（真机抓包格式），网盘直链未绑定请求 IP，暂无影响。
- **Emby 协议只实现最小集**：登录/列库/详情/播放/进度/海报够用；直播、DVR、转码不支持（PlaybackInfo 已声明禁转码、仅直连）。

## 后台

访问 `http://<host>:8096/admin`，后台账号默认 `admin/admin123`。可修改飞牛连接、媒体库海报，以及后台安全路径；路径保存后立即生效。

## 文件结构

```text
cmd/fnos-emby-bridge/main.go      入口 + 配置 + CORS
cmd/verify/main.go                本地联调启动器（mock + 桥接）
internal/fn/client.go             飞牛客户端（v2 登录 + authx 签名 + token/cookie 双通道）
internal/fn/api.go                飞牛 typed 方法（mediadb/item/play/stream/record）
internal/emby/handler.go          Emby 端点 → 飞牛映射
internal/emby/cloudDirect.go      云盘直链分流（115 限速/夸克分块/透明代理）
internal/emby/util.go             工具（限速器/json/png/stream）
internal/emby/bridge_test.go      端到端测试（mock 飞牛）
internal/mock/fnos.go             mock 飞牛 0.9.8 服务
```


