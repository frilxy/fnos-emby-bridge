# 飞牛 Emby 桥接中间件

> 本项目不再维护。欢迎自行 Fork、提交 PR，或基于它开新项目。

把飞牛影视伪装成 Emby 服务端，让 Emby 播放器直接连接飞牛影视媒体库。

## 镜像

```text
ghcr.io/ssabv/fnos-emby-bridge:latest
```

## Docker 部署

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

启动：

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

飞牛地址、飞牛账号、飞牛密码、媒体库海报和后台安全路径，都在后台填写。

## 后台

访问：

```text
http://你的NAS_IP:8096/admin
```

默认账号密码：

```text
admin
admin123
```

## Emby 客户端连接

服务器地址：

```text
http://你的NAS_IP:8096
```

账号密码使用飞牛影视的账号密码。

## 常见问题

### 后台路径改错怎么办？

修改 `data/bridge-config.json`：

```json
{
  "admin_path": "/admin"
}
```

然后重启容器。

### 播放器连不上？

不要使用 `127.0.0.1`，建议使用 NAS 局域网 IP。
