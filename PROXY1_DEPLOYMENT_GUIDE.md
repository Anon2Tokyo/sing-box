# AWS Lightsail (`proxy-1`) × `sing-box` 全栈部署、协议原理与高延迟内核调优指南

本文档沉淀了基于 **AWS Lightsail (`us-east-2` 美东俄亥俄)** 实例部署 **`sing-box`** 的完整架构设计、协议选型科普、防主动探测原理、Go 编译标签解析、Linux 跨洋高延迟内核调优，以及 **GitHub Actions 自动编译部署** 与 **Metacubexd 实时监控台** 运维方案。

---

## 目录

1. [基础设施与 CI/CD 架构概览](#1-基础设施与-cicd-架构概览)
2. [主流代理协议对比与双协议共存方案](#2-主流代理协议对比与双协议共存方案)
3. [核心安全机制：什么是「防主动探测」与全网盲扫？](#3-核心安全机制什么是防主动探测与全网盲扫)
4. [`sing-box` Go Build Tags（编译标签）详解](#4-sing-box-go-build-tags编译标签详解)
5. [跨洋高延迟（High-BDP）Linux 内核与协议层性能拉满指南](#5-跨洋高延迟high-bdplinux-内核与协议层性能拉满指南)
6. [内置 `Metacubexd` 实时 Web 监控控制台](#6-内置-metacubexd-实时-web-监控控制台)
7. [服务端与 Clash Verge 客户端配置参考](#7-服务端与-clash-verge-客户端配置参考)
8. [日常运维、测速与 AWS 成本注意事项](#8-日常运维测速与-aws-成本注意事项)

---

## 1. 基础设施与 CI/CD 架构概览

### 1.1 实例规格（`proxy-1`）
* **云厂商 / 区域**：AWS Lightsail — `us-east-2`（美东俄亥俄，`us-east-2a`）
* **硬件配置**：2 vCPU (Intel Xeon Platinum 8259CL @ 2.50GHz，支持 AES-NI) / 1 GB RAM / 40 GB NVMe SSD / 2 TB 月流量
* **操作系统**：Debian 12 (`Linux 6.1 cloud-amd64`)
* **网络与防火墙开放端口**：
  * `TCP 22`：SSH 远程管理与 GitHub Actions 自动部署
  * `TCP 443`：`VLESS + XTLS-Vision + Reality` 主力入站
  * `UDP 443`：`Hysteria 2` 跨洋高速入站（与 Reality 共用 443 端口号，协议层 TCP/UDP 分离）
  * `TCP 9090`：`sing-box` 内置 `Clash API` + `Metacubexd` Web 实时监控台

### 1.2 为什么采用 GitHub Actions 编译而非实例本地编译？
* **运行时轻量，编译期吃内存**：`sing-box` 运行时仅占用 **20 MB ~ 40 MB** 内存，2 核 1 GB 机器跑满千兆加密转发绰绰有余；但 Go 编译器在链接完整特性树时峰值内存较高，在 1 GB 内存实例上直接 `go build` 易触发 OOM。
* **自动化流水线（[`.github/workflows/deploy-proxy1.yml`](.github/workflows/deploy-proxy1.yml)）**：
  1. 本地修改源码并 `git push origin testing`（或在网页端手动触发 `workflow_dispatch`）；
  2. GitHub 云端 Runner (`ubuntu-latest`) 自动拉取代码，按指定 Build Tags 交叉编译 `linux/amd64` 静态二进制文件；
  3. 通过仓库 Secrets (`PROXY1_HOST`、`PROXY1_USER`、`PROXY1_SSH_KEY`) 自动 SCP 上传至 `/usr/local/bin/sing-box`；
  4. 自动执行 `sing-box check -c /etc/sing-box/config.json` 校验配置合法性，并通过 `systemctl restart sing-box` 完成平滑热更新。

---

## 2. 主流代理协议对比与双协议共存方案

针对美东 `us-east-2` 到国内直连物理延迟约 **180ms ~ 230ms**、晚高峰可能伴随跨洋丢包的网络特征，各主流协议表现对比如下：

| 协议方案 | 底层传输 | 抗封锁 / 隐蔽性 | 速度与弱网抗丢包 | 配置复杂度 | 适用定位 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **VLESS + XTLS-Vision + Reality** | TCP (伪装 TLS 1.3) | ⭐⭐⭐⭐⭐（极高） | ⭐⭐⭐⭐（稳定，配合 BBR） | ⭐⭐（免域名/免证书） | **全天候主力首选**，不怕运营商 UDP 限速 |
| **Hysteria 2 (歇斯底里 2)** | UDP (基于 QUIC) | ⭐⭐⭐⭐（伪装 HTTP/3） | ⭐⭐⭐⭐⭐（极速，抗丢包极强） | ⭐⭐⭐（支持自签证书） | **跨洋提速 / 晚高峰救星**，4K/8K 视频秒开 |
| **TUIC v5** | UDP (基于 QUIC) | ⭐⭐⭐⭐ | ⭐⭐⭐⭐（0-RTT 握手快） | ⭐⭐⭐（需证书） | 适合移动端频繁切换网络 |
| **Trojan / AnyTLS** | TCP + TLS | ⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐⭐（需自有域名+证书） | 适合已有个人域名并建站伪装的场景 |
| **Shadowsocks-2022 (SS2022)** | TCP / UDP | ⭐⭐（无 TLS 伪装） | ⭐⭐⭐⭐⭐（协议开销最小） | ⭐（仅需对称密钥） | 仅适合专线中转落地，不建议公网直连 |

### 最佳实践：`VLESS-Reality (TCP 443)` + `Hysteria 2 (UDP 443)` 双协议共存
* **日常主力走 `VLESS + Vision + Reality`**：利用 `XTLS-Vision` 消除内部 TLS 握手长度特征，不惧部分地区运营商针对 UDP 的 QoS 限流。
* **大带宽/晚高峰走 `Hysteria 2`**：基于 QUIC 协议，不受传统 TCP 队头阻塞（Head-of-Line Blocking）影响，在 200ms+ 高延迟与丢包环境下依然能跑满本地宽带。

---

## 3. 核心安全机制：什么是「防主动探测」与全网盲扫？

### 3.1 为什么新开的服务器不到 20 分钟就有陌生 IP 扫描 443 端口？
全球公网 IPv4 地址仅约 43 亿个（$2^{32}$）。借助 `ZMap`、`Masscan` 等高速异步扫描工具，一台高带宽服务器只需几分钟即可遍历全球所有 IPv4 的 `443` 端口。
互联网上 24 小时存在以下几类常驻扫描流量：
1. **网络安全测绘平台**（如 Censys、Shodan、Fofa）与大型 CDN 探测节点（如日志中出现的 `184.32.x.x`）；
2. **自动化漏洞扫描爬虫与僵尸网络**；
3. **防火墙的主动探测爬虫（Active Prober）**。

### 3.2 防火墙的「主动探测」是如何工作的？
当国内客户端向海外某个陌生 IP 的 `443` 端口发送大量加密数据时，防火墙仅凭被动流量分析（DPI）难以 100% 区分这是“正常访问海外小众 HTTPS 网站”还是“连接代理服务器”。为了避免误封正常网站，防火墙会派出探测爬虫**主动回访连接该服务器的 443 端口**：
* **传统无伪装/自签证书协议的暴露过程**：
  探测爬虫发起标准 TLS/HTTPS 请求 -> 代理服务器返回协议错误、乱码或不受信任的自签名证书 -> 探测爬虫实锤该 IP:443 为代理节点 -> 封禁 IP 或阻断端口。
* **`Reality` 的防主动探测原理**：
  * **自己人连接时**：客户端利用服务端公钥与 `ShortId`，在 TLS 1.3 `ClientHello` 握手的 `SessionId` 等字段中嵌入经密码学认证的临时凭据，服务端验证通过后建立 VLESS 代理隧道。
  * **扫描器/探测爬虫来敲门时**：对方没有私钥签名凭据。此时 `sing-box` **不会返回任何报错**，而是直接化身为透明 TCP/TLS 转发器，将探测者的握手和请求原封不动转发给配置好的大厂目标网站（如 `www.microsoft.com`），并将微软官方服务器返回的**真实证书链、真实网页内容、真实 TLS 行为**原样回传给探测者。
  * **日志特征**：在 `sing-box` 日志中看到如下记录时，即代表 Reality 成功拦截并欺骗了一次外部扫描探测：
    ```text
    ERROR inbound/vless[vless-reality-in]: process connection from 184.32.16.105:21042: TLS handshake: REALITY: processed invalid connection
    ```

---

## 4. `sing-box` Go Build Tags（编译标签）详解

`sing-box` 采用 Go 语言的条件编译标签（`-tags "..."`，参见 [`Makefile`](Makefile) 与 [`release/DEFAULT_BUILD_TAGS_OTHERS`](release/DEFAULT_BUILD_TAGS_OTHERS)）来按需裁剪功能模块：

| Build Tag | 核心作用 | 本项目是否启用 |
| :--- | :--- | :--- |
| **`with_quic`** | 启用 QUIC 协议栈支持。**`Hysteria 2`、`TUIC` 及 HTTP/3 DNS 必须依赖此标签** | ✅ 已启用 |
| **`with_utls`** | 启用 `uTLS` 库，支持模拟 Chrome / Firefox / Safari 的真实 TLS 客户端指纹 | ✅ 已启用 |
| **`with_clash_api`** | 启用内置 Clash API 服务端，支持挂载 **Metacubexd / Yacd** 实时流量与日志监控面板 | ✅ 已启用 |
| **`with_acme`** | 内置自动向 Let's Encrypt / ZeroSSL 申请与续期域名证书的能力 | ✅ 已启用 |
| **`with_wireguard`** | 内置用户态 WireGuard 协议支持 | ✅ 已启用 |
| **`with_dhcp`** | 内置 DHCP 服务端/客户端支持 | ✅ 已启用 |
| **`badlinkname,tfogo_checklinkname0`** | Go 1.23+ 编译器兼容标志（允许底层网络库如 `tfo-go` 与 `utls` 使用 `//go:linkname`） | ✅ 已启用（必选） |
| `with_tailscale` / `with_cloudflared` | 内置 Tailscale 虚拟组网与 Cloudflare Tunnel 支持（服务端单机代理可省去以加快编译） | 按需可选 |

> **注**：新版 `sing-box` 已将 `Reality` 服务端能力内置为默认支持，无需再额外指定旧版的 `with_reality_server` 标签。

---

## 5. 跨洋高延迟（High-BDP）Linux 内核与协议层性能拉满指南

在美东 `us-east-2`（RTT $\approx 200\text{ ms}$）场景下，制约单连接峰值速度的往往不是服务器 CPU 或带宽上限，而是 **TCP 窗口大小** 与 **握手往返耗时**。

### 5.1 核心原理：带宽时延积（BDP）与 TCP 窗口瓶颈
TCP 协议的最大理论吞吐量受限于公式：

$$\text{Max Throughput} = \frac{\text{TCP Window Size}}{\text{RTT}}$$

* 当跨洋延迟 $\text{RTT} = 0.2\text{ s}$ 时，如果系统默认的 TCP 缓冲区上限只有 `4 MB`，那么单条 TCP 连接的理论极限速度就会被锁死在 $\frac{4\text{ MB}}{0.2\text{ s}} = 20\text{ MB/s}$（约 `160 Mbps`），一旦丢包还会断崖式下跌。
* 将内核最大读写窗口扩容至 **`64 MB`** 后，理论单流吞吐上限即可解锁至千兆以上。

### 5.2 已部署的内核调优参数（`/etc/sysctl.d/99-sing-box.conf`）

```ini
# 1. 开启 Google BBR 拥塞控制算法与 fq 公平队列调度
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr

# 2. 将 TCP / UDP 最大读写缓冲区扩容至 64 MB（突破跨洋高延迟 BDP 瓶颈，同时喂饱 QUIC/Hysteria2）
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
net.ipv4.tcp_rmem = 4096 87380 67108864
net.ipv4.tcp_wmem = 4096 65536 67108864

# 3. 增大网卡收包队列与 Socket 监听队列上限
net.core.netdev_max_backlog = 16384
net.core.somaxconn = 8192

# 4. 关闭空闲后慢启动重置（极关键：防止阅读网页停顿几秒后 TCP 窗口被打回原形）
net.ipv4.tcp_slow_start_after_idle = 0

# 5. 开启自动 MTU 黑洞探测（防止跨洋路由分片丢包导致连接假死）
net.ipv4.tcp_mtu_probing = 1

# 6. 开启内核级 TCP Fast Open（值为 3 表示客户端与服务端双向同时启用 0-RTT 握手）
net.ipv4.tcp_fastopen = 3

# 7. Swap 积极度调至 10（90% 物理内存用满前绝不动用磁盘 Swap，兼顾零损耗与防 OOM）
vm.swappiness = 10
```

### 5.3 `sing-box` 协议层加速开关
在 `vless` 入站配置中同步开启了以下两项特性：
* **`"tcp_fast_open": true`（TFO 快速打开）**：允许在首次 TCP 三次握手的 `SYN` 包中直接捎带第一批加密载荷（0-RTT），省去一次跨太平洋往返等待（约 `200ms`），显著提升网页首屏响应速度。
* **`"tcp_multi_path": true`（MPTCP 多路径传输）**：利用 Go 标准库与现代 Linux 内核的多路径 TCP 能力，提升长连接在网络轻微抖动时的抗断流能力。

### 5.4 1 GB Swap 虚拟内存兜底
通过 `/swapfile` 挂载了 `1 GiB` Swap（写入 `/etc/fstab` 持久化），配合 `vm.swappiness = 10`：日常仅使用高速物理内存，遇到突发高并发或系统后台更新时提供 1 GB 缓冲空间，彻底杜绝 Linux OOM Killer 强杀 `sing-box`。

---

## 6. 内置 `Metacubexd` 实时 Web 监控控制台

依托 `with_clash_api` 编译标签，`sing-box` 在 `/etc/sing-box/config.json` 中通过 `experimental.clash_api` 原生集成了 **Metacubexd** 监控面板（零额外进程、内存开销 `< 5 MB`）：

```json
  "experimental": {
    "cache_file": {
      "enabled": true
    },
    "clash_api": {
      "external_controller": "0.0.0.0:9090",
      "external_ui": "ui",
      "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
      "external_ui_download_detour": "direct",
      "secret": "<CLASH_API_SECRET>",
      "access_control_allow_private_network": true
    }
  }
```

### 面板核心功能
* **概览 (Overview)**：实时上下行速率折线图、累计上传/下载总流量、`sing-box` 进程实时内存占用。
* **连接 (Connections)**：实时查看当前所有活跃代理连接的目标域名/IP、入站标签（`vless-reality-in` 或 `hy2-in`）、单连接流量消耗及持续时长，支持一键断开指定连接。
* **日志 (Logs)**：网页端实时滚动查看服务端 `info` / `warn` / `error` / `debug` 日志，无需每次 SSH 登录服务器查 `journalctl`。

---

## 7. 服务端与 Clash Verge 客户端配置参考

### 7.1 服务端配置文件结构（`/etc/sing-box/config.json`）

```json
{
  "log": {
    "level": "info",
    "timestamp": true
  },
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-reality-in",
      "listen": "::",
      "listen_port": 443,
      "tcp_fast_open": true,
      "tcp_multi_path": true,
      "users": [
        {
          "uuid": "<UUID>",
          "flow": "xtls-rprx-vision"
        }
      ],
      "tls": {
        "enabled": true,
        "server_name": "www.microsoft.com",
        "reality": {
          "enabled": true,
          "handshake": {
            "server": "www.microsoft.com",
            "server_port": 443
          },
          "private_key": "<REALITY_PRIVATE_KEY>",
          "short_id": [
            "<REALITY_SHORT_ID>"
          ]
        }
      }
    },
    {
      "type": "hysteria2",
      "tag": "hy2-in",
      "listen": "::",
      "listen_port": 443,
      "users": [
        {
          "password": "<HY2_PASSWORD>"
        }
      ],
      "tls": {
        "enabled": true,
        "alpn": [
          "h3"
        ],
        "certificate_path": "/etc/sing-box/hy2.crt",
        "key_path": "/etc/sing-box/hy2.key"
      }
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ],
  "experimental": {
    "cache_file": {
      "enabled": true
    },
    "clash_api": {
      "external_controller": "0.0.0.0:9090",
      "external_ui": "ui",
      "external_ui_download_url": "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
      "external_ui_download_detour": "direct",
      "secret": "<CLASH_API_SECRET>",
      "access_control_allow_private_network": true
    }
  }
}
```
> **注**：服务器上的真实生成的密钥统一保存在 `/etc/sing-box/credentials.txt`（权限 `600`），重装或通过 GitHub Actions 更新二进制文件均不会丢失。

### 7.2 Clash Verge (Mihomo 内核) 扩展配置 (`prepend-proxies`)

```yaml
prepend-proxies:
  - name: "AWS-proxy1-Reality"
    type: vless
    server: <SERVER_STATIC_IP>
    port: 443
    uuid: <UUID>
    network: tcp
    udp: true
    tls: true
    flow: xtls-rprx-vision
    servername: www.microsoft.com
    client-fingerprint: chrome
    reality-opts:
      public-key: <REALITY_PUBLIC_KEY>
      short-id: <REALITY_SHORT_ID>

  - name: "AWS-proxy1-Hysteria2"
    type: hysteria2
    server: <SERVER_STATIC_IP>
    port: 443
    password: <HY2_PASSWORD>
    sni: www.bing.com
    skip-cert-verify: true
    alpn:
      - h3
```

---

## 8. 日常运维、测速与 AWS 成本注意事项

### 8.1 常用服务端运维命令
```bash
# 查看 sing-box 运行状态
sudo systemctl status sing-box

# 实时查看服务端日志
sudo journalctl -u sing-box -f

# 查看已保存的节点密钥凭据
sudo cat /etc/sing-box/credentials.txt

# 校验配置文件语法并重启服务
sudo /usr/local/bin/sing-box check -c /etc/sing-box/config.json && sudo systemctl restart sing-box
```

### 8.2 推荐测速方式
1. **连通性延迟测试**：在 Clash Verge「代理」页点击测速图标（美东物理往返 + TLS 握手正常范围：`180ms ~ 320ms`）。
2. **真实流媒体与多线程带宽测速**：
   * **Netflix 测速**：https://fast.com
   * **Cloudflare 综合链路质量（含丢包率与抖动）**：https://speed.cloudflare.com
   * **YouTube 4K 实测**：播放 2160p60 视频 -> 右键画面打开「详细统计信息 (Stats for nerds)」观察 `Connection Speed`。

### 8.3 AWS Lightsail 静态 IP (`proxy-1-ip`) 计费与换 IP 技巧
* **绑定状态完全免费**：只要静态 IP 绑定在运行中的 `proxy-1` 实例上，AWS **不收取任何额外静态 IP 费用（\$0.00）**，且重启实例 IP 永不改变。
* **免费秒换 IP 技巧**：若未来某天公网 IP 意外受阻，无需重装实例，只需在 Lightsail 控制台解绑并删除旧静态 IP，重新申请一个新静态 IP 绑定即可。
* **删机提醒**：若未来彻底删除 `proxy-1` 实例，请务必同步释放静态 IP `proxy-1-ip`，因为**未绑定任何实例的闲置静态 IP** 会产生约 \$0.005/小时（约 \$3.6/月）的占用费。
