# 设计

## 目标

在同一台 Linux 主机上，用**一个普通用户进程**完成两种 TCP 桥接：

1. **publish**：把本机 `127.0.0.1:target` 发布为 tailnet 的 `node:port`；
2. **map**：把 tailnet 的 `ip:port` 映射为本机的 `127.0.0.1:port`。

约束：

- 不创建 TUN，不修改路由表，不需要 root；
- 不安装系统级 Tailscale 客户端；
- 仍然使用 Headscale 做控制面，节点以进程级身份出现；
- 只处理 TCP，不做 TLS 终止。

## 架构

```
publish 模式

  tailnet peer
      │  TCP -> 100.64.0.x:9000
      ▼
  tsnet netstack (userspace)
      │  srv.Listen(":9000")
      ▼
  tsnet-bridge
      │  net.Dial("127.0.0.1:22")
      ▼
  本机服务

map 模式

  本机应用
      │  TCP -> 127.0.0.1:2222
      ▼
  tsnet-bridge (net.Listen)
      │  srv.Dial("100.64.0.y:22")
      ▼
  tsnet netstack (userspace)
      │
      ▼
  tailnet peer
```

一个进程只创建一个 `tsnet.Server`，所有 `--publish` / `--map` 共享这个节点。
不同节点需要不同的 `--state-dir` 和 `--hostname`。

## 启动流程

1. 解析 flags，校验每个 `LEFT=RIGHT`；
2. 检查 `TS_AUTHKEY`；
3. 如果 `--derp-only` 且环境里没有 `TS_DEBUG_ALWAYS_USE_DERP=1`，`exec` 自己一次，
   把环境变量带上（tsnet 在包初始化时读取它）；
4. 创建 `tsnet.Server`，调用 `Up(ctx)` 等待节点 Running；
5. 为每个 `--publish` 创建 tailnet listener，为每个 `--map` 创建本机 listener；
6. 每个 listener 一个 accept 循环，每个连接一个 goroutine，双向 `io.Copy`；
7. 收到 SIGINT/SIGTERM 后关闭 listener 和 tsnet，进程退出。

## 参数

| 参数 | 说明 |
|---|---|
| `--hostname` | Headscale 里显示的节点名 |
| `--control-url` | Headscale URL，默认 `https://blackhole.netsnail.net` |
| `--state-dir` | tsnet 状态目录，必须每个节点唯一 |
| `--publish PORT=TARGET` | 可重复；tailnet 端口 → 本机目标 |
| `--map LOCAL=TAILNET` | 可重复；本机监听地址 → tailnet 目标 |
| `--derp-only` | 强制 DERP（VPS 中转） |
| `--dial-timeout` | 出站连接超时，默认 10s |
| `TS_AUTHKEY` | 环境变量，preauth key |

## 并发与生命周期

- accept 循环阻塞在 `Accept()`；ctx 取消时关闭 listener，循环返回；
- 连接 goroutine 在任一方向 `io.Copy` 结束后关闭两端；
- 没有连接数上限；需要限流时由 systemd/ulimit 或目标服务负责；
- `tsnet.Server.Close()` 在进程退出时关闭 netstack。

## 错误处理

- 启动阶段错误（key 无效、端口被占、state 目录冲突）直接 `Fatal`；
- 运行阶段单个连接失败只记日志，不影响 listener；
- listener 异常退出会通过 `errCh` 返回，进程退出并由 systemd 重启。

## 安全模型

- 传输层：WireGuard（tsnet 内部），或 DERP 中转；
- 身份：Headscale 节点认证 + preauth key；
- 授权：Headscale policy（本项目不内置 ACL）；
- 目标认证：SSH key、应用登录等仍由目标服务负责；
- 本机暴露面：`--map` 默认只建议 `127.0.0.1`，`--publish` 只开明确端口。

## 已知限制

- 只支持 TCP；
- 不支持 UDP、TLS 终止、PROXY protocol；
- `--map` 需要一个进程级 tsnet 节点，不是“完全不出现在 tailnet”的方案；
- 如果客户端本身是 subnet router（例如 Pi 广播了 `192.168.123.0/24`），
  它到 tsnet 节点的 TCP 可能失败；这是 tsnet/客户端侧的问题，不是本工具；
- `--derp-only` 依赖 tailscale 的调试环境变量，未来版本可能变化，需锁版本。

## 后续可做

- UDP 映射；
- 配置文件（YAML/TOML）替代重复 flag；
- Prometheus 指标（连接数、字节数）；
- `LocalClient.WhoIs` 做 per-peer ACL；
- 连接空闲超时和最大连接数；
- 和 systemd socket activation 集成。
