# 更新日志

本项目（**飞牛影视 Emby 桥接**）的版本历史。每个版本的改动都附带**实机验证依据**
（客户端日志 / 官方 API 文档）。

## b008（预发布，构建号）

针对「剧集每季的图片没有」：

- **图片 parent 链改用 `itemDetailAnywhere`**：季没有自己的 poster/backdrop 时，要沿
  parent 找剧的海报；之前这条链仍用普通 `itemDetail`，剧详情 404 时季图就变成 1×1。
- **季详情兜底**：`item/{seasonGuid}` 404 时，遍历 TV 剧的 `season/list` 找回该季，
  季详情和季封面都能正常返回。
- 包含 b007 的 TV Latest/封面/详情修复，b006 的宽高比/季海报，b005 的播放修复。

## b007（预发布，构建号）

针对「电视库封面是横版艺术图、点进 TV 库无封面、点剧集无内容」：

- **TV 库 Latest 默认返回剧容器**：之前为符合 Emby「TV 库返回最新集」的语义，默认返回
  Episode；Rodel 的 TV 行因此显示每集的横版剧照。现在默认返回 Series（竖版封面），
  客户端显式带 `IncludeItemTypes=Episode` 时才返回最新集。
- **ImageTags.Primary 始终声明**：TV 剧条目经常只有 backdrop、没有 poster；之前 Primary
  只在有 poster 时声明，客户端根本不发封面请求 → 电视库空白。现在始终给 Primary tag，
  handler 再依次用季海报/backdrop 兜底。
- **TV 详情兜底**：`item/{guid}` 对部分 TV 剧返回 404 时，改从 `item/list` 的库列表里
  按 guid 找条目，详情页不再 404、点剧集不再「没有内容」。
- 包含 b006 的主图宽高比/季海报修复与 b005 的播放 token 修复。

## b006（预发布，构建号）

针对「电影/AV 封面正常，电视节目封面不对」：

- **`PrimaryImageAspectRatio` 按实际尺寸输出**：飞牛 TV/集的 poster 常是 16:9 横图，
  之前固定 0.6667 会让客户端按竖版卡片裁剪，显示成错误构图。现在用
  `poster_width/poster_height` 计算；无尺寸才回退 0.6667。
- **TV 优先用季海报**：剧条目 poster 为空或为横图时，先找该剧第一季的**竖版**海报
  （同一作品），再退回自己的 backdrop；不借别的剧海报。
- 包含 b005 的播放 401 修复。

## b005（预发布，构建号）

- **修复播放 401**：`streamURL` 之前只读 `X-Emby-Token`/query，漏掉
  `X-Emby-Authorization: MediaBrowser ..., Token="..."`；现在统一走 `h.apiKey()`，
  生成的流 URL 会带 `api_key`，MPV/播放器直连不再 401。
- **修复 PlaybackInfo 崩溃**：`PlaySessionId` 用 `id[:12]`，短 id 会 slice 越界；已加保护。
- **电视图片兜底调整**：Primary 先找同剧/季的 parent 图，再退回自己的 backdrop；
  不再用“库内第一张图”给 TV 兜底，避免电视节目显示成别的剧海报。
- 包含 b004 的图片公开/会话 client/playinfo 兜底。

## b004（预发布，构建号）

针对「演员图正常、内容图全空」的修复（演员图走 tag 直连，不经过 item 详情，说明问题在
内容图的条目解析/鉴权链路）：

- **图片端点公开**：`/Items/{id}/Images/...`、`/Users/{uid}/Items/{id}/Images/...` 不再要求
  token（很多客户端的图片组件不会带 token）；图片 handler 会自动选一个**有效登录会话**的
  飞牛 client，而不是回落到可能已过期的 admin token。
- **item 详情兜底**：图片解析 `item/{guid}` 失败时用 `play/info` 的 poster/backdrop；
  还没有就沿 parent 链（集→季→剧）找图。
- **竖版位兜底**：Primary 没有海报时用横版 backdrop；Backdrop 没有横图时用海报。
- 无图/上游失败会打印 `[poster] item=<id> type=<type> img=<type> ...`，便于定位。
- 版本：`1.1.4-b004`（tag/镜像 `b004`）。

## b003（预发布，构建号）

- **修正预发布版本策略**：实测 fnOS 应用中心对 `1.1.2-b001` 与 `1.1.2-b002` 仍判定为
  同一版本（忽略 `-b###` 后缀），导致 b002 无法覆盖安装。从本版起**补丁号与构建号一起递增**：
  `1.1.3-b003`，下一版 `1.1.4-b004`。
- 内容包含 b002 的图片修复（图片路径哈希 tag、去掉 304、Content-Length/Content-Type 兜底）。

## b002（预发布，构建号）

针对「所有图片不显示」的修复：

- **图片 tag 改为路径哈希**：`ImageTags.Primary/Logo/Backdrop/...` 与
  `BackdropImageTags` 不再用固定的 `"Primary"`/`"0"`，而是图片路径的短哈希。
  海报/背景图变化或旧缓存是 1×1 占位图时，客户端 URL 会变，从而重新拉取。
- **去掉图片 304**：部分客户端（含小幻影视）对图片条件请求处理不一致，304 可能被当成
  空响应显示为空白；现在条件请求也返回 200 + 图片。
- **补 Content-Length / Content-Type 兜底**：上游没给图片类型时按扩展名推断；
  非图片响应会打日志，便于定位。
- 图片失败时仍回 1×1 PNG，但带 `Cache-Control: no-store`，避免被长期缓存。

## b001（预发布，构建号）

预发布版从本版起使用 **`b` + 三位构建号**（b001、b002…），每次修改递增；正式版仍用
`1.x.y`。

- fnOS/fnpack 要求 manifest 版本为 `x.y.z[-r]`，所以 manifest 写成
  **`1.1.2-b001`**，Release/镜像 tag 用 **`b001`**（不带 v）。
- 工作流支持 `b*` tag；Docker 镜像打 `b001` 标签（不打 latest）。
- 内容包含 v1.1.1 的小幻影视 token 刷新与详情 404 修复。

## v1.1.1（预发布）

针对小幻影视 v2.2610.32 实测日志的兼容修复：

- **修复 token 刷新 401 循环**：新增 `POST /Users/{id}/Authenticate`（Emby 的按 id
  刷新入口），并列入免鉴权路径；body/query 里的 `Pw` 都支持。此前该路径被
  `requireAuth` 拦成 401，客户端会反复重试登录。
- **修复部分条目详情 404**：`item/{guid}` 失败时用 `play/info` 兜底（仅当返回的
  `Guid` 与请求 id 一致，避免把剧 guid 误当下一集）；404 时打印 item id 与飞牛错误，
  便于继续排查。
- **play/list 优先 `item_guid`**：少数版本把媒体文件 guid 放在 `guid`，直接当条目
  id 会导致详情 404；现在优先用 `item_guid`/`itemGuid`。
- 新增回归：`TestAuthenticateByIDAndDetailFallback`（刷新入口免 token、play/info 兜底）。

## v1.1.0（预发布）

这一版把「能看」补齐到 Emby 客户端常用的查询/筛选/字幕/多用户语义。12 项：

- **搜索**：`/Items?SearchTerm=` 与 `/Users/{uid}/Items?SearchTerm=` 接飞牛
  `GET /v/api/v1/search/list?q=`；按 IncludeItemTypes 过滤、桥接侧分页；兼容只认
  POST body 的版本。
- **真实入库时间**：解析 `item/list` / `item/{guid}` 的 `create_time`（秒/毫秒/日期串），
  写入 `DateCreated`；缺失才回退首播日期，不再用 `time.Now()`。
- **排序语义**：`SortBy` 支持多键（如 `ProductionYear,PremiereDate`），
  `DateCreated` 与 `PremiereDate` 分离，新增 `PlayCount`；`Random` 首页随机、
  翻页复用同一 seed（避免分页重复/漏项）；`/Shows/{id}/Seasons|Episodes` 也支持 SortBy。
- **元数据**：解析并输出 `Genres`/`GenreItems`、`OriginalTitle`、`OfficialRating`
  （content_ratings）、`ProductionLocations`（production_countries）、`EndDate`
  （last_air_date）、`ProviderIds`（IMDb/TMDB/豆瓣）与外部链接。
- **筛选**：`Genres`、`Years`、`OfficialRatings`、`Countries`、`Is4K`、`IsHD`、
  `Filters`（IsPlayed/IsUnplayed/IsResumable/IsFavorite/Likes）、`ExcludeItemTypes`、
  `Ids`、`NameStartsWith`、`MinCommunityRating`；`/Genres`、`/Years`、
  `/OfficialRatings`、`/Items/Filters` 返回真实字典。
- **Latest / NextUp**：TV 库的 `Latest` 返回最新**集**（带 SeriesId）；
  `/Shows/NextUp` 无 SeriesId 时也能跨剧返回"下一集"。
- **继续观看**：优先用 `GET /v/api/v1/play/list`，失败回退全库扫描；缓存按用户隔离，
  不再共用 admin 客户端；支持 ParentId/StartIndex/Limit。
- **外挂字幕**：`/Videos/{id}/{msId}/Subtitles/{index}/Stream.{fmt}` 从
  `stream/list.subtitle_streams` 找外挂轨，代理 `/v/api/v1/subtitle/dl/{guid}`；
  内嵌字幕仍由客户端直读容器（返回 404）。
- **图片**：支持官方完整路径
  `/Items/{id}/Images/{type}/{index}/{tag}/{format}/{maxWidth}/{maxHeight}/...`、
  多背景图（BackdropImageTags/索引）、ETag/Cache-Control/304。
- **评分 / 隐藏继续观看**：`POST/DELETE /Users/{uid}/Items/{id}/Rating?Likes=`、
  `POST /Users/{uid}/Items/{id}/HideFromResume?Hide=` 落本地用户状态，并在
  UserData.Likes / Resume 中生效。
- **多用户与鉴权**：User.Id/Name 按飞牛账号派生，`/Users/Me`、`/Users/Public` 反映真实
  登录用户；除握手/登录/品牌/本地化外，媒体端点默认要求有效 token（无 token/错误 token
  返回 401）；兼容 `X-Emby-Authorization` 头。排障可用 `ALLOW_ANON=1` 临时放开。
- **缓存与请求合并**：媒体库/详情/play-info/stream-list/search/tag 字典加 TTL 缓存与
  singleflight；写进度/已看/收藏/评分后失效相关缓存。

> 测试：`go test ./...` 覆盖上述行为（`internal/emby/v11_features_test.go`）；发布前请按
> Release 说明里的"测试方法"在真实客户端上再验一遍。

## v1.0.17

- **桌面入口改为在飞牛桌面内嵌打开**。`app/ui/config` 里入口的 `type` 由 `url`（跳转新标签页）
  改为 `iframe`（在飞牛桌面窗口内打开）——这是官方对两种入口的定义：

  > 桌面窗口：`type=iframe`；需要完整浏览器能力：`type=url`

  其余字段不变（`protocol`/`port`/`url` 照旧）。桥接后台**没有**发送 `X-Frame-Options`
  或 CSP `frame-ancestors`，所以不会被浏览器拦掉；页面里也没有 `window.open` /
  `target="_blank"`，原本就会就地完成登录、保存、上传等所有操作。

  > 注意：若你是用 **HTTPS** 访问飞牛桌面，而桥接服务是 HTTP（8096），浏览器会按混合内容
  > 拦掉 iframe。这种情况下需要让 8096 也走 HTTPS（或反向代理），否则请继续用 `type: url`。

## v1.0.16

- **修复「播放完了还留在继续观看、状态也不变已播放」**。

  线上实测那 3 部刚看完的片子长这样：

  ```
  Video SNOS-112 …  位置=8954s 时长=8954s = 100.0%  Played=False
  ```

  **进度上报是成功的**（位置已经等于时长），但「标记已看」没执行。原因是判定条件：

  ```go
  if stopped && dur > 0 && sec >= dur*92/100 {   // 只在 stopped 时
  ```

  有的客户端（实测小幻影视）**只发 `Sessions/Playing/Progress` 把位置推到片尾，从不发
  `Sessions/Playing/Stopped`**，于是永远进不了这个分支。

  修复：**任何一次**位置上报达到阈值即标记已看，不再要求 `stopped`
  （Emby 自身也是按约 90% 的进度判定已播放）。阈值提为常量 `watchedThresholdPercent = 92`。

- **自愈已有残留**：拉取「继续观看」时，位置已到片尾（≥92%）却仍未标的条目会被
  **排除出列表**，并**异步补标已看**。所以已存在的那些「记到 100% 仍未看」的条目
  装上后会自动消失并变成已播放，不用手动处理。

- mock 补齐 `POST/DELETE /v/api/v1/item/watched`（此前只实现了 `play/record`，
  没法验证已看标记），并加 `SetItemField` 供测试构造残留场景。

- 回归测试两条：
  - `TestPlaybackProgressMarksWatched`：用 **Progress**（非 Stopped）上报到片尾，
    断言条目变为已播放。还原旧条件（加回 `stopped &&`）即失败。
  - `TestResumeExcludesFinishedItems`：构造「位置=时长、is_watched=0」的残留，
    断言它不出现在 Resume。去掉自愈过滤即失败（条目带着 100% 进度留在列表里）。

## v1.0.15

- **中文标题排序改为按拼音序**。v1.0.14 的标题排序用的是字节比较，得到的是 **Unicode 码点序**：
  实测「按标题升序」出来是 一一 → 七宗罪 → 东京物语 → 乱 → 信条 → 北极百货店…（码点严格递增），
  而中文用户要的是拼音序（**北 bei** → **东 dong** → … → **一 yi**）。英文标题因为 ASCII 码点序
  恰好等于字母序，所以看起来是对的，中文就露馅了。

  改用 `golang.org/x/text/collate`（CLDR 里 `zh` 的默认排序规则就是拼音序），
  中英文统一走本地化比较，也与 .NET/ICU 客户端的预期一致。
  `collate.Collator` 并发不安全，用 `sync.Pool` 每 goroutine 借一个。

  代价：二进制 +1.6 MB（7.4 MB → 9.0 MB，主要是 CLDR 排序表），fpk 约 4.5 MB。

- 回归测试 `TestItemsSortByAndOrder` 增加拼音序断言：mock 里「电影A(电 dian)」应排在
  「剧集B(剧 ju)」之前。实测把实现临时改回字节比较会失败并报
  `[fv_tv fv_001]`（即剧集在前，正是码点序），改回 collate 即通过。

- 新增依赖 `golang.org/x/text v0.28.0`（钉在兼容 go 1.24 的版本；`@latest` 会要求 go ≥ 1.26）。

## v1.0.14

- **实现完整的 `SortBy` / `SortOrder`**。此前这两个参数被**完全忽略**，所以在客户端里
  「按标题 / 按时间」等排序点了没有任何反应——列表顺序永远等于飞牛库内顺序
  （这也是 v1.0.13「推荐影片」问题的同一个根因）。

  现在支持客户端常用的排序键（大小写不敏感，支持逗号并列如 `Random,SortName`；
  **无法识别的键保持飞牛原顺序**，不改坏现有行为）：

  | SortBy | 依据 |
  |---|---|
  | `SortName` / `Name` | 标题（忽略大小写） |
  | `PremiereDate` / `DateCreated` / `AirDate` | 首播 → 发行 → 首映日期；日期相同退化为按标题 |
  | `DatePlayed` | 最后播放时间（桥接记录优先，回退飞牛 `watched_ts`） |
  | `ProductionYear` | 年份 |
  | `CommunityRating` / `CriticRating` | 评分 |
  | `Runtime` | 时长 |
  | `Random` | 随机打乱 |

  `SortOrder=Descending` 为降序，缺省为升序。

  注：飞牛没有「入库时间」字段，桥接给客户端的 `DateCreated` 本来也是用首播日期兜底的
  （见 `toEmbyItem`），所以按时间排序与界面显示的日期一致。

- 回归测试 `TestItemsSortByAndOrder`：断言升序/降序互为逆序、按时间排序生效、
  未知排序键保持原顺序。实测上游原版报「SortOrder 没生效：Ascending 与 Descending 返回同一顺序」。

## v1.0.13

- **修复「生成推荐影片失败」**（真正的原因）。用代理抓包看到客户端「推荐影片」实际请求的是：

  ```
  GET /Users/{uid}/Items?IncludeItemTypes=Movie,Series&...&SortBy=Random&Limit=30&Recursive=true
  ```

  它**并不调用** `/Movies/Recommendations`（v1.0.12 修的那个端点，抓包里只有我自己的自检请求）。
  而桥接**完全忽略 `SortBy`**：返回的是飞牛库内固定顺序，这个查询稳定只给出前 30 个**剧集**、
  一部电影都没有，而且每次刷新返回**完全一样**的列表 → 客户端认为「生成推荐」失败。

  现已实现 `SortBy=Random`（在类型过滤之后、分页之前打乱候选集），
  于是同一查询每页都是电影/剧集混合且每次刷新都不同。
  回归测试 `TestSortByRandomShuffles` 连续 24 次取样断言不全相同；修复前实测 24 次全为同一条。

- 顺带说明：v1.0.12 的 `/Movies/Recommendations` 仍保留（官方契约就是
  `RecommendationDto[]`，其它客户端会用），只是不是本客户端这条路径。

## v1.0.12

- **修复「生成推荐影片失败」**。`/Movies/Recommendations` 官方契约是
  **`RecommendationDto[]`（数组）**，而桥接从未注册该路由，落到兜底返回了 QueryResult
  对象 `{"Items":[],...}`，客户端按数组反序列化直接抛异常。
  现在按官方契约返回数组，并给出真实内容：以用户**最近播放过的条目**为基线
  （`RecommendationType = SimilarToRecentlyPlayed`），推荐它所属媒体库里的其它条目
  （`BaselineItemName` / `CategoryId` / `Items` 均为官方 `RecommendationDto` 字段）。
  没有观看历史时返回空数组——真实 Emby 对无历史用户同样如此。
- 同一批检查确认 `/Users/{uid}/Suggestions` 官方就是 `QueryResult` 对象，兜底形状正确，
  无需修改。
- `routeSegmentCase` 补充 `movies` / `recommendations`，客户端发小写路径也能命中。

## v1.0.11

- **后台账号密码改为安装时必须设定**。此前 Go 端内置默认密码 `admin123`（等于后台没有鉴权），
  而生命周期脚本又会自动生成一个随机密码写进 `admin.pass`——**两个值不一致，随机那个从来没生效过**，
  真正能用的一直是 `admin123`。现在：
  - 安装向导新增「设置桥接后台账号」一步，**账号与密码都必填**。
  - 去掉 Go 端 `admin123` 默认值；未设密码、或仍沿用 `admin123` 时**拒绝启动**并给出可执行指引。
  - 不再自动生成随机密码。
  - `admin.pass` 改为**每次启动按当前生效的账号密码刷新**，看到的就是真的。
- **配置改放飞牛规范目录**：`bridge.env`、`admin.pass`、`bridge-config.json` 移到
  `TRIM_PKGETC`（应用配置目录，形如 `/vol6/@appconf/fnos-emby-bridge/`），运行数据
  （`service.log`、`app.pid`、会话与进度缓存）留在 `TRIM_PKGVAR`。升级时自动迁移，
  且启动时固定工作目录，缓存不再落到不确定的 CWD。
- Docker 示例里的 `ADMIN_PASS` 改为占位符并注明必填。

## v1.0.10

- 与 v1.0.9 源码相同，**重新发布**：v1.0.9 的 Release 在整理 tag 时丢失了附件，
  本版本是内容一致的完整发布包。（v1.0.9 那个空 Release 可以删掉。）

## v1.0.9

- **品牌统一**为「飞牛影视 Emby 桥接」（英文 **fnOS Emby Bridge**），仓库名 `fnos-emby-bridge` 不变。
- 更换应用图标：以 `fpk/图标.jpg` 为唯一图标源，派生 fpk 图标、应用中心入口图标、
  后台界面图标与说明文档配图（`fpk/make-icons.sh`）。
- 后台界面标题/品牌区改用真图标（内嵌 data URI，不依赖静态资源路由）。
- **打包迁到 GitHub Actions**：`Build` 工作流跑测试并产出 fpk，`Release` 工作流在
  `v*` tag 上自动构建并发版；`dist/` 不再入库。
- 新增 `fpk/pack.sh`：没装飞牛官方 `fnpack` 时用标准 `tar` 复刻 fpk 结构，CI 因此无需
  下载打包工具。已逐项校验两条路径的产物等价：成员表（名称/类型/权限）与 `app.tgz`
  内容哈希完全一致，`manifest` 除 `checksum` 外逐行相同。
- 复刻时补上 fnpack 会写入的 `checksum = md5(app.tgz)`（实测确认就是这个值；
  安装器可能校验它，缺了有被拒的风险），并给 `tar -T` 列表加 `--no-recursion`，
  避免目录被递归展开导致成员重复。

## v1.0.8

- **修正 1.0.5 引入的错误**：`/Videos/{id}/AdditionalParts`、`/Items/{id}/CriticReviews`
  官方返回 `QueryResult` **对象**，1.0.5 误改为数组，导致客户端在响应第 1 个字符就解析失败
  （`The JSON value could not be converted to EmbyQueryResult`）。已改回对象。
- 仅 `/Items/{id}/Images`、`LocalTrailers`、`Ancestors`、`Localization/*` 保持数组。
- 回归测试改为**逐端点分别断言**期望形状。

## v1.0.7

- 写操作（标记已看 / 未看 / 收藏）改为返回 `200 + UserItemDataDto`。
  此前 `PlayedItems` 返回 `204` 空响应、`FavoriteItems` 返回自造的包装对象，
  .NET 客户端反序列化空 body 后报「操作失败」。
- 新增 `POST /Users/{uid}/Items/{id}/UserData`（此前未实现，落到 204 兜底＝表面成功实际无操作）。
- `UserItemDataDto` 补 `ServerId`（官方注明 "Used only by our Windows app"）。
- 放宽 `playedItemsRe` 的 id 段为 `[^/]+`，避免非纯 hex id 静默落兜底。

## v1.0.6

- **关键修复**：`UserData`（`UserItemDataDto`）必需字段补齐。
  `PlayCount` 此前只在「已看完」时才写，未看过的条目缺该字段，客户端校验后抛
  `Emby item '...' returned incomplete required user state`，详情页一直加载。
  现 `PlaybackPositionTicks` / `PlayCount` / `IsFavorite` / `Played` 始终输出，
  并补 `ItemId`。

## v1.0.5

- 补齐数组形状端点：`/Items/{id}/Images`（返回真实 `ImageInfo[]`）、`/Plugins`、
  `/Library/VirtualFolders`、`/Users/{uid}/GroupingOptions`、`/Localization/*`。
- `/System/Info/Public` 补 `LocalAddress` / `OperatingSystem` / `StartupWizardCompleted`，
  `Id` 改为合法的 32 位 hex。

## v1.0.4

- 补齐客户端打开媒体库/详情的必经端点：`/UserViews`、`/Items/Filters`、
  `/Users/{uid}/Items/Filters`、`/Plugins`、`/Library/VirtualFolders`、`/Items/Counts`、
  `/Users/{uid}/GroupingOptions`。
- `ServerId` 改为由飞牛地址 + 服务器名派生的稳定 32 位 hex（重启不变、不同部署不撞车）。

## v1.0.3 及更早

- 修复 `[fallback]` 路径拼接问题：客户端发出的绝对 URL 被拼成 `/embyhttp:/host/...`
  导致「点开视频立即失败」。新增 `repairMangledURLPath`。
- 「其他视频」类媒体库（AV 等）一片空白：这类库 `CollectionType` 为空，
  按 Emby 语义作为**混合内容**处理，并加空结果回退。
- `HEAD /Videos/{id}/stream` 挂死；后台连接页 `panic`（`shortToken`）。
- 取流 `Content-Type` 按扩展名推断；新增音轨兼容性提示（如 `· 需软解`）。
- 提供飞牛 `.fpk` 安装包与构建脚本。
