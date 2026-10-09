# 更新日志

本项目（**飞牛影视 Emby 桥接**）的版本历史。每个版本的改动都附带**实机验证依据**
（客户端日志 / 官方 API 文档）。

## v1.0.9

- **品牌统一**为「飞牛影视 Emby 桥接」（英文 **fnOS Emby Bridge**），仓库名 `fnos-emby-bridge` 不变。
- 更换应用图标：以 `fpk/图标.jpg` 为唯一图标源，派生 fpk 图标、应用中心入口图标、
  后台界面图标与说明文档配图（`fpk/make-icons.sh`）。
- 后台界面标题/品牌区改用真图标（内嵌 data URI，不依赖静态资源路由）。
- **打包迁到 GitHub Actions**：`Build` 工作流跑测试并产出 fpk，`Release` 工作流在
  `v*` tag 上自动构建并发版；`dist/` 不再入库。
- `fpk/pack.sh`：没装飞牛官方 `fnpack` 时用标准 tar 复刻 fpk 结构（已校验两者的成员表、权限与文件哈希完全一致），CI 因此无需下载打包工具。

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
