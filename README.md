# 飞牛 Emby 桥接

一个面向 NAS / Docker 的飞牛影视 Emby 协议桥接工具。

把飞牛影视伪装成 Emby 服务端，让 Emby 客户端可以直接连接、浏览和播放飞牛影视媒体库。

## 项目特点

- Emby 客户端直连飞牛影视
- 支持多版本播放
- 支持多音轨和字幕信息
- 支持本地文件和云盘直链
- 支持进度、收藏和已看状态
- 支持后台账号密码登录
- 支持自定义后台安全路径
- 支持 Docker 一键部署

## 镜像

GHCR：

```text
ghcr.io/ssabv/fnos-emby-bridge:latest
```

## 部署

### Docker Run

```bash
docker run -d \
  --name fnos-emby-bridge \
  --restart unless-stopped \
  -p 8096:8096 \
  -e PORT=8096 \
  -e ADMIN_USER=admin \
  -e ADMIN_PASS=admin123 \
  -e CONFIG_PATH=/data/bridge-config.json \
  -v /你的路径/data:/data \
  ghcr.io/ssabv/fnos-emby-bridge:latest
```

### Docker Compose

```yaml
services:
  fnos-emby-bridge:
    image: ghcr.io/ssabv/fnos-emby-bridge:latest
    container_name: fnos-emby-bridge
    restart: unless-stopped
    ports:
      - "8096:8096"
    environment:
      - PORT=8096
      - ADMIN_USER=admin
      - ADMIN_PASS=admin123
      - CONFIG_PATH=/data/bridge-config.json
    volumes:
      - ./data:/data
```

```bash
docker compose up -d
```

## 配置说明

| 配置 | 说明 |
|---|---|
| `PORT` | 服务端口，默认 `8096` |
| `ADMIN_USER` | 后台账号，默认 `admin` |
| `ADMIN_PASS` | 后台密码，默认 `admin123` |
| `CONFIG_PATH` | 配置文件路径 |
| `/data` | 持久化配置目录 |

## 后台使用

访问：

```text
http://你的NAS_IP:8096/admin
```

默认账号密码：

```text
admin
admin123
```

登录后可以填写：

- 飞牛影视地址
- 飞牛影视账号
- 飞牛影视密码
- 媒体库海报
- 后台安全路径

保存后立即生效。

## Emby 客户端连接

在 Emby 客户端添加服务器：

```text
http://你的NAS_IP:8096
```

账号密码使用飞牛影视的账号密码。

## 常见问题

### 后台路径改错怎么办？

修改容器挂载目录里的 `bridge-config.json`，把 `admin_path` 改回：

```json
{
  "admin_path": "/admin"
}
```

然后重启容器。

### 播放器连不上？

如果播放器和服务不在同一台机器，不要使用 `127.0.0.1`。  
建议使用 NAS 局域网 IP。

### 支持转码吗？

不支持。桥接使用直连播放，不提供 Emby 转码。

## 版本

使用指定版本镜像：

```text
ghcr.io/ssabv/fnos-emby-bridge:v1.0.0
```
