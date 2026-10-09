# fnos-emby-bridge

把**飞牛影视（fnOS）**的媒体库以 **Emby 协议**暴露出来，让 Yamby、小幻影视、Infuse、VidHub
等客户端直接连——飞牛影视本身只提供私有协议，第三方客户端支持有限。

本仓库 fork 自 [ssabv/fnos-emby-bridge](https://github.com/ssabv/fnos-emby-bridge)（原项目已停止维护），
在其基础上修复了一批客户端兼容问题，并额外提供飞牛 `.fpk` 安装包；改动集中在 `internal/emby`，
完整 diff 见 [`fixes.patch`](fixes.patch)，逐版本修复记录见 [`CHANGELOG.md`](CHANGELOG.md)。

> ⚠️ **不做转码**：桥接只做直连/直封装，能否播放取决于客户端能否解码原文件。
> `4K HEVC Main10 + TrueHD Atmos`、`DTS-HD MA` 这类格式手机端普遍解不动——那是设计边界，
> 不是 bug。需要任何格式都能播，请用真正的 Jellyfin/Emby，或让客户端走软解。

---

## 安装

### 方式 A：fnOS 应用包（推荐）

1. 从 [Releases](https://github.com/frilxy/fnos-emby-bridge/releases) 下载最新的
   `fnos-emby-bridge-<版本>.fpk`
2. 飞牛「应用中心」→ 左下角「手动安装」→ 上传 fpk
3. 向导里填飞牛影视地址（形如 `http://192.168.1.10:8005`，**必须用局域网 IP**）、账号、密码
4. 装完从桌面「Emby 桥接」进后台，确认连接状态

后台密码在向导里设置，或首次启动自动生成，见 `<应用数据目录>/admin.pass`。

### 方式 B：Docker

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

| 客户端 | 说明 |
|---|---|
| Infuse / VidHub | 类型选 **Emby**；能直通 HEVC / DTS / TrueHD |
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

> 飞牛影视实际端口是 **8005**（上游 `main.go` 注释里的示例端口是错的）。
> 容器/应用内不要用 `127.0.0.1` 访问飞牛影视。

---

## 从源码构建

```bash
./build.sh          # vet + test + 编译 + fnpack 打包，产物在 dist/
```

或手动：

```bash
cd src
CGO_ENABLED=0 go build -ldflags="-s -w" -o ../dist/fnos-emby-bridge-patched ./cmd/fnos-emby-bridge
cp ../dist/fnos-emby-bridge-patched ../fpk/fnos-emby-bridge/app/server/fnos-emby-bridge
cd .. && fnpack build --directory ./fpk/fnos-emby-bridge
```

打 fpk 需要飞牛官方 `fnpack`。

---

## 已知限制

- **不转码**（见文首说明）。
- 上游契约基于**飞牛影视 0.9.8 / mediasrv 0.8.42**，飞牛影视升级后接口可能变化。
- 仅提供 **x86_64** 包（静态编译）。
- 后台配置以**明文**保存飞牛密码（上游设计如此），请勿把 8096 暴露到公网。

---

## 许可

遵循上游 **GPL-3.0**，保留 `LICENSE`。
