# 更新日志

本项目（**飞牛影视 Emby 桥接**）的版本历史。每个版本的改动都附带**实机验证依据**
（客户端日志 / 官方 API 文档）。

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
