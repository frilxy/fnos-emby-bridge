# fnos-emby-bridge（社区 fork）

把 **飞牛影视（fnOS 影视）** 包装成 **Emby / Jellyfin 兼容服务端**，让 Infuse、Yamby（小幻影视）、爆米花、Findroid 等第三方播放器直接播放飞牛影视媒体库。

上游 [ssabv/fnos-emby-bridge](https://github.com/ssabv/fnos-emby-bridge) 已声明不再维护。本仓库是社区 fork，**只修 bug、不改架构**，并额外提供 **飞牛 fnOS 的 .fpk 安装包**。

> ⚠️ **关于转码**：本桥接**不做转码**，只做直连/直封装。播放能否成功取决于客户端能否硬解该文件。
> `4K HEVC Main10 + TrueHD Atmos`、`DTS-HD MA`、`FLAC` 这类音视频，手机/电视端普遍无法直接解码——
> 那是设计边界，不是 bug。需要任何格式都能播，请改用真正的 Jellyfin/Emby，或让客户端走软解（VLC / Infuse / Just Player）。

---

## 修了什么

### 1. 点开视频立即失败，且日志里看不出错（最关键）

**现象**

```
[fallback] GET /embyhttp:/192.168.10.229:8096/Videos/{id}/Stream
[http] 200 GET /embyhttp:/192.168.10.229:8096/Videos/{id}/Stream?MediaSourceId=...&Static=true (0s) []
```

**成因链**

1. 桥接在 `MediaSource.Path` / `DirectStreamUrl` 返回**绝对播放地址**：`http://<nas>:8096/Videos/{id}/stream?...`
2. 部分客户端把它与自身 base（`http://<nas>:8096/emby`）做**字符串拼接** →
   `http://<nas>:8096/embyhttp://<nas>:8096/Videos/{id}/stream?...`
3. Go 解析请求行时把 `//` 归一为 `/`，服务端实际收到 `/embyhttp:/<nas>:8096/Videos/{id}/stream?...`
4. 该路径不匹配任何路由 → 落到兜底 handler → **返回 `200` + 空 QueryResult JSON（49 字节）**
5. 播放器拿到 JSON 而不是视频字节，**而且状态码是 200，客户端连报错都没有**

**修复**：入站侧新增 `repairMangledURLPath` 中间件，识别路径里的 `http://` / `https://` /（Go 归一后的）`http:/`，摘掉 scheme+host 还原成真实路径。

**为什么不是把 `streamURL()` 改成相对路径**：`handlePlayback` 处有上游作者的明确注释——

```go
// Path 必须真实 URL：客户端直接交给播放器内核（fn:// 伪协议零请求失败）
```

**存在一类客户端会把 `Path` 原样交给播放器，必须要绝对 URL**。改成相对会修好一类、弄坏另一类。
入站修复让两类客户端同时工作。另外 `streamURL()` 也不是唯一给 `Path` 赋 URL 的地方（另有 6 处）。

**实测**（同一实例，`--path-as-is`）：

| 请求 | 修复前 | 修复后 |
|---|---|---|
| `/embyhttp://<nas>:8096/Videos/{id}/Stream` | `200 application/json` **49 字节** | `200 video/mp4` **4096 字节** |

### 2. 点进「其他视频」类媒体库一片空白（AV 库等）

**现象**：首页能正常看到这些视频，但点进该媒体库是空白。

**成因**：飞牛库类别只有 `TV / Movie / Other / Live`（`Other` 在飞牛界面里叫「其他视频」），
而这一类库的条目被桥接标成 `Type=Video`。

Emby 里没有与「其他视频」对应的库类型，按 Emby 官方定义应归为**混合内容**：

> If **CollectionType** is null, it indicates a mixed movie/tv folder that should be
> displayed generically. —— [Emby REST API: Browsing the Library](https://dev.emby.media/doc/restapi/Browsing-the-Library.html)

混合库不声明 `CollectionType`，客户端只能自行猜 `IncludeItemTypes`（多为 `Movie,Series`），
于是 `Type=Video` 的条目被**整个过滤掉**，返回 0 条。首页不带库级类型过滤，所以显示正常。

```
GET /Users/{uid}/Views                                    → 该库 CollectionType=null（混合内容）
GET .../Items?ParentId=<库>&IncludeItemTypes=Movie,Series  → 0 条（空白）
GET .../Items?ParentId=<库>&IncludeItemTypes=Video         → 95 条
```

> ⚠️ 注意**不要**把它写成 `"mixed"`：Emby 文档列出的可用值
> （`movies/tvshows/music/games/books/musicvideos/homevideos/livetv/channels`）里没有 `mixed`；
> Jellyfin 的 `CollectionType` 响应枚举同样没有（`mixed` 只存在于**建库选项**
> `CollectionTypeOptions` 中）。上报未知值会让部分客户端直接加载不了该库。

**修复**：库级浏览的**空结果兜底** —— 带 `ParentId` 时若类型过滤把结果清空、但库本身有内容，
则忽略类型过滤返回库的真实内容，并打日志。不带 `ParentId` 的全局筛选保持严格不变。

**实测**（同一 mock 夹具）：

| | 修复前 | 修复后 |
|---|---|---|
| 该库 `CollectionType` | `null`（混合内容，符合 Emby 定义） | `null`（不变） |
| 点进库（客户端猜的 `Movie,Series`） | **0 条 → 空白** | **1 条** ✅ |

日志：`[items] IncludeItemTypes="Movie,Series" 过滤后为空，库 lib_other 回退为不过滤（1 条）`

### 3. 小幻影视打不开影视详情（**UserData 字段不全** ← 真正的根因）

**现象**：Yamby 一切正常，小幻影视（Rodel Player，Windows）能登录、能列出媒体库，
但**点开任意条目详情页一直转圈**。

**定位**：让它走一个落日志的反向代理，再导出小幻影视自己的日志，异常写得很清楚：

```
System.InvalidOperationException: Emby item '73b9597b...' returned incomplete required user state.
   at RodelPlayer.SproutHost.Metadata.ProductMetadataUserStateProjection
        .EnsureRequiredUserState(String, String, Nullable, Nullable, Nullable, Nullable)
   at RodelPlayer.SproutHost.Metadata.ProductMetadataUserStateProjection.FromEmby(EmbyMediaItem)
```

**根因**：桥接的 `UserData`（Emby 官方 `UserItemDataDto`）**只在「已看完」时才写 `PlayCount`**，
没看过的条目直接缺这个字段 → 客户端校验必需字段时判为 "incomplete" 抛异常。

修复前 / 后对比（同一个未看过的条目）：

```jsonc
// 修复前：缺 PlayCount
{"Key":"fv_001","Played":false,"IsFavorite":false,"PlaybackPositionTicks":0}
// 修复后：必需字段齐全
{"Key":"fv_001","ItemId":"fv_001","Played":false,"PlayCount":0,
 "IsFavorite":false,"PlaybackPositionTicks":0}
```

`PlaybackPositionTicks` / `PlayCount` / `IsFavorite` / `Played` 四个字段**必须始终存在**，
值为 0/false 也要写出来——**"字段缺失"和"值为 0"对客户端不是一回事**。
另补上官方同样有的 `ItemId`。四处 `UserData` 构造点（列表/详情、play-infoo、人物、收藏响应）已统一。

### 4. 小幻影视等客户端打不开媒体库

**现象**：Yamby 一切正常，但小幻影视（Rodel Player）连上后打不开媒体库或详情页。

**排查方法**：拿飞牛**原生 Jellyfin 面（`http://<NAS_IP>:8005`）**做参照——飞牛自己实现
了哪些端点给第三方客户端用，就说明真实客户端确实会调用。逐个对照后找出的缺口：

| 端点 | 飞牛原生 8005 | 桥接（修复前） |
|---|---|---|
| `/UserViews` | ✅ 实现 | ⚠️ 落兜底 → **返回空列表**（客户端看不到任何媒体库） |
| `/Items/Filters` | ✅ 实现 | ❌ **404** —— 被 `/Items/{id}` 通配吃掉，拿 `"Filters"` 当影片 ID 去查飞牛 |
| `/Users/{uid}/Items/Filters` | ✅ 实现 | ❌ **404**（同上） |
| `/Plugins` | ✅ 实现 | ⚠️ 返回 QueryResult 对象（应为数组） |
| `/Library/VirtualFolders` | ✅ 实现 | ⚠️ 返回 QueryResult 对象（应为数组） |
| `/Items/Counts` | ✅ 实现 | ⚠️ 返回 QueryResult 对象（应为 `ItemCounts` 对象） |
| `/Users/{uid}/GroupingOptions` | ✅ 实现 | ⚠️ 返回对象（应为数组） |
| `/Items/{id}/Images` | ✅ 实现 | ⚠️ 返回 QueryResult 对象（应为 `ImageInfo[]` 数组） |
| `/Localization/Options` 等 | — | ⚠️ 返回对象（应为数组） |

**形状不符比 404 更隐蔽**：状态码是 `200`，客户端连报错都没有，只是把响应塞进
"数组"类型去反序列化然后失败——表现就是**界面一直转圈**。

> 定位手段：用一个落日志的反向代理把小幻影视的流量录下来。实测它请求
> `/Items/{id}/Images` 拿到的却是 `{"Items":[],...}`（49 字节），而 Emby 该端点
> 返回的是 `ImageInfo[]`。

`/Items/Filters` 是 Emby 标准端点，**客户端打开媒体库时会调用它**，404 直接导致媒体库页打不开。

另外 `/System/Info/Public` 也缺字段、`Id` 格式不合法：

| | 修复前 | 真 Jellyfin / Emby |
|---|---|---|
| `Id` | `"fnos-emby-bridge"` | `"9da9a321945b4e60a7488ea2be37cb9f"`（32 位 hex GUID 形态） |
| `LocalAddress` | 缺 | ✅ |
| `OperatingSystem` | 缺 | ✅ |
| `StartupWizardCompleted` | 缺 | ✅ |

（另：`CanEnableAutoSignIn` 属于需鉴权的 `SystemInfo`，不属于 `Public`，已移出。）

**修复**：补齐上述端点（并给它们加上小写路径的大小写归一，客户端常发 `/userviews`），
`ServerId` 改为由**飞牛地址 + 服务器名**派生的稳定 32 位 hex——同一套部署重启不变、
不同部署不撞车（不能随机，否则客户端每次重启都会丢失服务器关联）。

### 5. 「标记已看 / 未看 / 收藏」提示操作失败（写操作响应形状不对）

**现象**：详情页点「标记为未看」，客户端提示**操作失败**。

**排查**：在服务端直接做往返测试，其实**功能是好的**——

```
操作前 Played = False
POST   /Users/{uid}/PlayedItems/{id} → 204 ；POST 后 Played = True
DELETE /Users/{uid}/PlayedItems/{id} → 204 ；DELETE 后 Played = False
```

问题不在功能，在**响应形状**。查官方契约
（[Emby PlaystateService](https://betadev.emby.media/reference/RestAPI/PlaystateService/postUsersByUseridPlayeditemsById.html)）：

> `post /Users/{UserId}/PlayedItems/{Id}` → **`200 | UserItemDataDto`**
> "Operation successful. **Returning a UserItemDataDto object.**"

桥接返回的是 **`204` 空响应**。.NET 客户端按 `UserItemDataDto` 反序列化并校验必需字段，
拿到空 body 就判成操作失败。`FavoriteItems` 那边更离谱——返回了自造的
`{"Id":..,"UserData":{..}}` 包装对象，也不是官方模型。

顺带一个坑：`playedItemsRe` 原来只匹配纯 hex id（`[0-9a-fA-F]+`），**id 不含纯 hex 时
正则不匹配，会静默落到「写操作 → 204 兜底」**——表面成功、实际什么都没做。已放宽为 `[^/]+`。

**修复**：三个写端点统一返回 `200 + UserItemDataDto` 本体：

| 端点 | 修复前 | 修复后 |
|---|---|---|
| `POST/DELETE /Users/{uid}/PlayedItems/{id}` | `204` 空 | **`200` + DTO** ✅ |
| `POST/DELETE /Users/{uid}/FavoriteItems/{id}` | 包装对象 | **`200` + DTO** ✅ |
| `POST /Users/{uid}/Items/{id}/UserData` | `204`（未实现） | **`200` + DTO**（真正生效）✅ |

DTO 含官方全字段，其中 `ServerId` 官方注明 **"Used only by our Windows app"**——
小幻影视正是 Windows 应用，会读这个字段。

### 6. 修正上一版自己的错误：`AdditionalParts` / `CriticReviews` 是**对象**不是数组

1.0.5 里我**基于猜测**把一批端点改成了纯数组，其中两个改错了。实机日志（小幻影视）：

```
Could not load Additional Parts for "4c5ea3e4..."
System.Text.Json.JsonException: The JSON value could not be converted to
  Richasy.RodelPlayer.Sources.Emby.Models.EmbyQueryResult`1[EmbyMediaItem].
  Path: $ | LineNumber: 0 | BytePositionInLine: 1
```

`BytePositionInLine: 1` —— 第 1 个字符就解析失败，说明客户端要的是**对象** `{"Items":[...]}`，
而 1.0.5 给了数组 `[]`。查官方文档确认：

> `get /Items/{Id}/CriticReviews` → **`200 | QueryResult_BaseItemDto`**
> "Operation successful. **Returning a QueryResult object.**"

`/Videos/{Id}/AdditionalParts` 同理（Jellyfin 对应类型 `BaseItemDtoQueryResult`）。两个已改回
`QueryResult` 对象。

**教训**：这一批"应为数组"的判断里，只有 `/Items/{id}/Images` 有实机证据支持（日志显示
"Loaded 4 art images" 成功）。其余靠推理，就出了这个错。回归测试现在对每个端点**分别断言**
期望形状，而不是笼统地要求"都是数组"。

### 7. HEAD 取流请求挂死

Go 的 `GET` 路由会一并匹配 `HEAD`，HEAD 不带 `Range` 时走完整 GET，把**整个文件**（实测 6.7GB）当响应体往外推，而 HEAD 不发送响应体 → 请求永不返回，部分播放器探测失败后判定「无法播放」。

**修复**：HEAD 改为向飞牛只请求 `Range: bytes=0-0` 拿头部与总长度，不传字节。

### 8. 后台连接页 panic

`main.go` 里 `client.Token[:8]` 在飞牛未登录（token 为空）时切片越界 panic。触发路径是 `GET /admin/api/connection`——**恰好是刚部署、还没配好连接时最需要打开的那一页**，结果是面板打不开、地址填不进去，形成死循环。

**修复**：新增 `shortToken()`，三处 `[:8]` 全部加固。

### 9. 改进：取流 `Content-Type`

上游 `/v/api/v1/media/range` 一律返回 `application/octet-stream`。现按扩展名给真实 MIME（`video/mp4`、`video/x-matroska` 等），仅在通用类型时覆盖。

### 10. 改进：音轨兼容性提示

`DTS / TrueHD / FLAC / ALAC / PCM` 音轨的 `DisplayTitle` 追加 `· 需软解`，便于挑选。
用 `AUDIO_TRACK_HINT=0` 关闭。**刻意不做自动切轨**——备选轨常是导演评论轨，自动切换会让人听到错误音轨。

---

## 安装

### 方式 A：fnOS 应用包（推荐）

1. 下载 [`dist/fnos-emby-bridge.fpk`](dist/fnos-emby-bridge.fpk)
2. 飞牛「应用中心」→ 左下角「手动安装」→ 上传 fpk
3. 按向导填写飞牛影视地址（形如 `http://192.168.1.10:8005`，**必须用局域网 IP**）、账号、密码
4. 安装完成后从桌面「Emby 桥接」进入后台，确认连接状态

后台密码：向导里设的，或首次启动自动生成，可在
`<应用数据目录>/admin.pass` 与 `service.log` 中查看。

### 方式 B：Docker（官方镜像 + 替换二进制）

```yaml
services:
  fnos-emby-bridge:
    image: ghcr.io/ssabv/fnos-emby-bridge:latest
    container_name: fnos-emby-bridge
    restart: unless-stopped
    entrypoint: ["/data/fnos-emby-bridge-patched"]
    ports: ["8096:8096"]
    environment:
      - PORT=8096
      - ADMIN_USER=admin
      - ADMIN_PASS=<强密码>
      - CONFIG_PATH=/data/bridge-config.json
      - FNOS_BASE=http://<NAS_IP>:8005
      - FNOS_USER=<飞牛影视账号>
      - FNOS_PASS=<飞牛影视密码>
    volumes:
      - ./data:/data   # 把 dist/fnos-emby-bridge-patched 放进这个目录
```

---

## 客户端连接

服务器地址 `http://<NAS_IP>:8096`，账号密码用**飞牛影视的账号**。

| 客户端 | 建议 |
|---|---|
| Infuse / VidHub | 类型选 **Emby**，能直通 HEVC/DTS/TrueHD 等格式 |
| Yamby（小幻影视） | 已修复路径拼接问题；编解码受限于设备 |
| 爆米花 / Findroid | 可用 |

---

## 从源码构建

```bash
# 1) 编译桥接
cd src
CGO_ENABLED=0 go build -ldflags="-s -w" -o ../dist/fnos-emby-bridge-patched ./cmd/fnos-emby-bridge

# 2) 打 fpk（需要飞牛官方 fnpack）
cp ../dist/fnos-emby-bridge-patched ../fpk/fnos-emby-bridge/app/server/fnos-emby-bridge
cd .. && fnpack build --directory ./fpk/fnos-emby-bridge
```

或直接跑 `./build.sh`。

---

## 环境变量

| 变量 | 说明 |
|---|---|
| `FNOS_BASE` | 飞牛影视地址，**必须带 `http://`**，如 `http://192.168.10.229:8005` |
| `FNOS_USER` / `FNOS_PASS` | 飞牛影视账号密码 |
| `PORT` | 监听端口，默认 `8096` |
| `CONFIG_PATH` | 配置文件路径 |
| `ADMIN_USER` / `ADMIN_PASS` | 后台账号密码 |
| `HOST` | 绝对 URL 兜底主机（正常走请求 Host，一般不用设） |
| `AUDIO_TRACK_HINT` | 本 fork 新增，`0` 关闭音轨兼容性提示 |

> 上游 `main.go` 注释里的示例端口 `:5666` 是错的，飞牛影视实际是 **8005**。
> 容器/应用内不要用 `127.0.0.1` 访问飞牛影视。

---

## 与上游的差异

见 [`fixes.patch`](fixes.patch)：6 个文件、**+1199 / −52 行**，`patch -p1` 可干净应用到上游 `main`。

---

## 已知限制

- **不转码**（见文首说明）。
- 上游契约基于**飞牛影视 0.9.8 / mediasrv 0.8.42**。飞牛影视升级后接口可能变化。
- 仅提供 **x86** 包（二进制为 amd64 静态编译）。
- 后台「连接」配置以明文保存飞牛密码（上游设计如此），请勿把 8096 暴露到公网。

---

## 许可与致谢

- 本 fork 遵循上游 **GPL-3.0**，保留 `LICENSE`。
- 上游项目：[ssabv/fnos-emby-bridge](https://github.com/ssabv/fnos-emby-bridge)
