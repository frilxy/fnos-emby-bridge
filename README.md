<div align="center">
  <img src="docs/icon.png" width="128" alt="飞牛影视 Emby 桥接">
  <h1>飞牛影视 Emby 桥接</h1>
  <p><b>fnOS Emby Bridge</b> —— 把飞牛影视的媒体库以 Emby 协议暴露给第三方客户端</p>
</div>

把**飞牛影视（fnOS）**的媒体库以 **Emby 协议**暴露出来，让 Yamby、小幻影视、Infuse、VidHub、
爆米花等客户端直接连——飞牛影视本身只提供私有协议，第三方客户端支持有限。

本仓库 fork 自 [ssabv/fnos-emby-bridge](https://github.com/ssabv/fnos-emby-bridge)（原项目已停止维护），
在其基础上修复了一批客户端兼容问题，并额外提供飞牛 `.fpk` 安装包。改动集中在 `internal/emby`，
相对上游的**源码与 CI 改动**见 [`fixes.patch`](fixes.patch)（`patch -p1` 可应用），
逐版本修复记录见 [`CHANGELOG.md`](CHANGELOG.md)。

> ⚠️ **不做转码**：桥接只做直连/直封装，能否播放取决于客户端能否解码原文件。
> `4K HEVC Main10 + TrueHD Atmos`、`DTS-HD MA` 这类格式手机端普遍解不动——那是设计边界，
> 不是 bug。需要任何格式都能播，请用真正的 Jellyfin/Emby，或让客户端走软解。

## 元数据支持

| 字段 | 支持情况 |
|---|---|
| 类型 / Genres | ✅ 飞牛 `genres` id 经 `/tag/genres` 映射为名称，详情/筛选可用 |
| 标签 / Tags | ❌ 飞牛 item 接口没有用户标签字段；`/Tags`、`Tags`、`TagItems` 只能返回空 |
| 官方分级 / OfficialRating | ✅ 来自 `content_ratings` |
| 年份 / 国家 / 工作室 | 年份按飞牛习惯合并为**年代**（`2000s`，同时兼容单年筛选）；国家支持；工作室飞牛无数据 |
| 人物 / Persons | ✅ 演员/导演等 |

## 版本号约定

- 只发**正式版**：`1.x.y`，tag `v1.x.y`，全部为普通 Release，不再使用预发布/构建号后缀。
- 每次修改递增 patch：`1.1.13` → `1.1.14`；安装器按数字段比较，不会报「已安装相同版本」。
- Docker 镜像同步使用 `1.1.x` 标签，`latest` 跟随最新 `v*`。

## v1.1 新增

- **搜索**：`SearchTerm` 接飞牛 `/search/list`，不再"搜什么都返回整库"。
- **排序/筛选**：多键排序、真实 `DateCreated`（入库时间）、`Genres`/`Years`/`OfficialRatings`/
  `Is4K`/`IsHD`/已看/未看/可续播/喜欢筛选；`/Genres`、`/Years`、`/Items/Filters` 返回真实字典。
- **首页**：TV 库 `Latest` 返回最新集；`/Shows/NextUp` 支持跨剧"下一集"；`Resume` 改用
  `/play/list` 并按用户隔离。
- **字幕**：外挂字幕（`subtitle_streams.is_external=1`）可通过 Emby 字幕端点加载。
- **图片**：支持官方完整图片路径、多背景图、ETag/Cache-Control。
- **用户**：真实用户 Id/Name；`Rating`（喜欢/不喜欢）与 `HideFromResume` 落本地状态。
- **鉴权**：媒体端点默认要求有效 token（登录后自动带）；`ALLOW_ANON=1` 可临时放开排障。
- **性能**：媒体库/详情/播放信息/流列表/搜索/标签字典加 TTL 缓存与请求合并。

---

## 安装

### 方式 A：fnOS 应用包（推荐）

1. 从 [Releases](https://github.com/frilxy/fnos-emby-bridge/releases) 下载最新的
   `fnos-emby-bridge-<版本>.fpk`
2. 飞牛「应用中心」→ 左下角「手动安装」→ 上传 fpk
3. 向导第一步填飞牛影视地址（形如 `http://192.168.1.10:8005`，**必须用局域网 IP**）、账号、密码
4. 向导第二步**设定桥接后台账号与密码**（这个只用于打开桥接自己的后台，与飞牛影视账号无关）
5. 装完从桌面「飞牛影视 Emby 桥接」进后台，确认连接状态

#### 后台账号密码

后台地址 `http://<NAS_IP>:8096/admin`，账号密码就是向导第二步设定的那组。

| 事项 | 做法 |
|---|---|
| 修改 | 应用中心 → 飞牛影视 Emby 桥接 → **配置** → 「修改桥接后台账号」，保存后自动重启 |
| 查看当前生效值 | `<应用配置目录>/admin.pass`，每次启动按实际生效的账号密码刷新 |
| 配置源文件 | 同目录下的 `bridge.env`（含飞牛连接信息与后台账号密码，权限 600） |

`<应用配置目录>` 即飞牛的 `TRIM_PKGETC`，应用包安装的情况下形如
`/vol6/@appconf/fnos-emby-bridge/`。

> 本 fork **去掉了旧版本内置的 `admin123` 默认密码**（那等于后台没有鉴权），也不再自动生成
> 随机密码（随机值只写在文件里、没人会看到，反而与真正生效的凭据不一致）。现在没设密码、
> 或仍沿用 `admin123` 时服务会**拒绝启动**，并在应用中心给出提示，引导你显式设置。

### 方式 B：Docker

```yaml
services:
  fnos-emby-bridge:
    image: ghcr.io/frilxy/fnos-emby-bridge:1.1.13
    container_name: fnos-emby-bridge
    restart: unless-stopped
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
      - ./data:/data
```

镜像由本仓库的 Docker 工作流构建（amd64/arm64），也可用 `:latest`（指向最新 Release）。

---

## 客户端连接

服务器地址 `http://<NAS_IP>:8096`，账号密码用**飞牛影视的账号**。

| 客户端 | 说明 |
|---|---|
| Infuse / VidHub | 类型选 **Emby**；能直通 HEVC / DTS / TrueHD |
| 全部客户端 | v1.1 起媒体端点要求登录 token（客户端正常登录即可）；图片/流地址也会带 `api_key` |
| Yamby / 小幻影视 | 已验证可用；能否播放取决于设备解码能力 |
| 爆米花 / Findroid | 可用 |

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
| `ALLOW_ANON` | 排障用：`1` 时允许无 token 访问媒体端点（默认严格鉴权，不建议长期开启） |

> 飞牛影视实际端口是 **8005**（上游 `main.go` 注释里的示例端口是错的）。
> 容器/应用内不要用 `127.0.0.1` 访问飞牛影视。

---

## 构建与发布

```bash
./build.sh      # vet + test + 编译 + 打 fpk，产物在 dist/
```

打包**不依赖飞牛官方 fnpack**：`fpk/pack.sh` 优先用它，没装则用标准 `tar` 复刻同样的结构
（fpk 本质就是一个 ustar tar.gz），因此 CI 无需额外下载任何打包工具。

GitHub Actions：

| 工作流 | 触发 | 作用 |
|---|---|---|
| `Build` | push / PR | 跑 `go vet` + `go test`，构建 fpk 并作为 artifact 上传 |
| `Release` | push `v*` tag（或手动触发） | 构建 fpk → 发 Release → 附上 fpk |

**发新版只需三步**：改 `fpk/fnos-emby-bridge/manifest` 里的 `version` → 提交推送 →
`git tag v<版本> && git push origin v<版本>`。tag 与 manifest 版本不一致时工作流会直接失败，
避免发错版本。构建产物（`dist/`）不入库。

---

## 目录

| 路径 | 说明 |
|---|---|
| `cmd/fnos-emby-bridge/` | 桥接入口（后台服务、配置） |
| `internal/emby/` | Emby 协议实现，本 fork 的改动集中在这里 |
| `internal/fn/` | 飞牛影视私有接口客户端 |
| `internal/admin/` | 后台界面（图标内嵌，无外部静态资源依赖） |
| `fpk/` | 飞牛应用包工程：`manifest`、生命周期脚本、wizard、图标源 |
| `fpk/fnos-emby-bridge/cmd/` | 生命周期脚本：配置在 `TRIM_PKGETC`，运行数据在 `TRIM_PKGVAR` |
| `fpk/图标.jpg` | **唯一图标源**；`fpk/make-icons.sh` 由它派生各尺寸 PNG |
| `docs/` | 由图标源派生的图片，供本说明使用 |
| `fixes.patch` | 相对上游的全部源码改动，可 `patch -p1` 应用 |

---

## 已知限制

- **不转码**（见文首说明）。
- 上游契约基于**飞牛影视 0.9.8 / mediasrv 0.8.42**，飞牛影视升级后接口可能变化。
- 仅提供 **x86_64** 包（静态编译）。
- 后台配置以**明文**保存飞牛密码（上游设计如此），请勿把 8096 暴露到公网。
- 媒体端点默认要求有效 token；如果客户端无法登录，可临时设 `ALLOW_ANON=1` 排障，但不要长期开启。

---

## 许可

遵循上游 **GPL-3.0**，保留 `LICENSE`。
