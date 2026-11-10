# tsnet-bridge

一个不建 TUN、不改路由、不需要 root 的 TCP 桥。它用 `tsnet` 在用户态加入
tailnet，支持两个方向：

| 模式 | 方向 | 典型用途 |
|---|---|---|
| `--publish` | 本机服务 → tailnet | 把本机 SSH/Web/RDP 发布成 tailnet 上的一台主机 |
| `--map` | tailnet 服务 → 本机端口 | 在本机开一个 `127.0.0.1:port`，访问 tailnet 里的 `IP:port` |

两种模式可以在同一个进程里混用，共享一个 tsnet 节点。

## 不做什么

- 不创建 TUN，不修改系统路由表，不需要 root；
- 不做 TLS 终止，不处理 UDP；
- 不替代完整 Tailscale 客户端：`--map` 模式仍然会以一个**进程级节点**出现在
  tailnet 里，这是 `s.Dial` 的前提；
- 不内置 ACL；访问控制仍然由 Headscale policy 和目标服务自己的认证负责。

## 构建

需要 Go 1.27.3（`tailscale.com v1.104.5` 的依赖要求）。

```bash
cd pi/tsnet-bridge
go build -trimpath -ldflags="-s -w" -o dist/tsnet-bridge-linux-amd64 .
```

交叉编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/tsnet-bridge-linux-arm64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -o dist/tsnet-bridge-linux-armv7 .
```

也可以直接安装：

```bash
go install github.com/sean-lin/tsnet-bridge@latest
```

## 认证

```bash
export TS_AUTHKEY='hskey-auth-...'
```

key 只在第一次注册时使用，之后节点状态保存在 `--state-dir` 里。state 目录要
按节点分开，不能两个进程共用。

## 参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--hostname` | `tsnet-bridge` | 节点在 Headscale 里显示的名字 |
| `--control-url` | `https://blackhole.netsnail.net` | Headscale 控制面地址 |
| `--state-dir` | `./tsnet-bridge-state` | 节点身份目录，**每个节点独立**；进程独占加锁（`<dir>/lock`） |
| `--publish` | 无 | `TAILNET_PORT=LOCAL_HOST:PORT`，可重复 |
| `--map` | 无 | `LOCAL_ADDR=TAILNET_HOST:PORT`，可重复；本地监听显式设 `SO_REUSEADDR` |
| `--derp-only` | false | 强制全部流量走 DERP 中继（通过 `TS_DEBUG_ALWAYS_USE_DERP` 自我 re-exec 实现） |
| `--dial-timeout` | `10s` | 出站连接超时 |
| `--shutdown-grace` | `5s` | 关停时等 tsnet 收尾的上限，超时直接退出 |
| `--verbose` | false | 逐条打印每条被代理的连接；默认只记起停与错误 |
| `--version` | | 打印版本（构建时用 `-X main.version=...` 注入） |

退出信号：第一个 `SIGINT`/`SIGTERM` 排空退出，第二个直接退出。

`state-dir` 里的节点身份可跨重启复用，因此 `TS_AUTHKEY` **只在首次注册时必需**；
已有 state 时可直接启动。

## 用法

### 把本机 SSH 发布到 tailnet

```bash
tsnet-bridge \
  --hostname home-ssh \
  --state-dir /var/lib/tsnet-bridge/home-ssh \
  --publish 9000=127.0.0.1:22
```

tailnet 里的设备访问 `100.64.0.x:9000`，流量转到本机 `127.0.0.1:22`。

### 把 tailnet 服务映射到本机端口

```bash
tsnet-bridge \
  --hostname local-map \
  --state-dir /var/lib/tsnet-bridge/local-map \
  --map 127.0.0.1:2222=100.64.0.21:22
```

本机 `ssh -p 2222 user@127.0.0.1` 就会到 tailnet 的 `100.64.0.21:22`。

### 同时使用

```bash
tsnet-bridge \
  --hostname bridge \
  --state-dir /var/lib/tsnet-bridge/bridge \
  --publish 9000=127.0.0.1:22 \
  --map 127.0.0.1:8080=100.64.0.30:80
```

### 强制走 VPS 的 DERP

```bash
tsnet-bridge --derp-only ...
```

`TS_DEBUG_ALWAYS_USE_DERP` 是 tailscale 包初始化时读取的调试开关，所以程序会
自动 `exec` 一次自己，把环境变量带上。这是调试开关，不是稳定 API；需要固定
版本时请锁住 tsnet 版本。

## systemd（用户级）

见 `examples/`：

```bash
mkdir -p ~/.config/systemd/user ~/.config/tsnet-bridge
cp examples/tsnet-bridge-publish.service ~/.config/systemd/user/
cp examples/tsnet-bridge.env.example ~/.config/tsnet-bridge/env
chmod 600 ~/.config/tsnet-bridge/env
# 编辑 env 和 service 里的 hostname/state-dir/--publish
systemctl --user daemon-reload
systemctl --user enable --now tsnet-bridge-publish
```

需要开机即跑时，普通用户还要 `loginctl enable-linger $USER`（可能需要管理员）。

## 已验证

- tailnet 设备 → `--publish` 端口：手机浏览器访问返回预期页面；
- 本机 `--map` → `--publish` 节点：`curl 127.0.0.1:18084` 返回 `bridge works`。

## 已知问题

- **subnet router 客户端**：Pi 广播了 `192.168.123.0/24`，它作为客户端访问
  tsnet 节点时 TCP 建连会失败（ping 正常）。手机/普通节点没有问题。
  如果目标客户端是 subnet router，请先用普通节点验证。
- **MagicDNS**：建议直接用 `100.64.0.x`，不依赖 MagicDNS。
- **Headscale 证书**：本项目不依赖 Tailscale Serve 的 HTTPS 证书，因此不受
  Headscale 不签发 `*.ts.net` 证书的影响。
- **UDP**：当前只桥 TCP。

## 安全建议

- `--map` 默认只绑定 `127.0.0.1`，不要随手绑 `0.0.0.0`；
- `--publish` 只发布明确需要的端口；
- 目标服务自己仍要有认证（SSH key、密码、应用鉴权）；
- Headscale policy 限制 src/dst，不要依赖“tailnet 内就安全”；
- auth key 放环境文件，权限 600，不要提交到仓库。
