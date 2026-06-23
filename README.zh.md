# GM Suite — EVM 自动化桌面应用

**语言：** [English](README.md) · [Tiếng Việt](README.vi.md) · **中文**

一款独立的桌面应用，用于大规模 EVM 自动化：多钱包 NFT 铸造（SeaDrop + OpenSea 预售凭证）、Flashbots 私有打包、白名单资格检查、RPC/代理管理，以及 Telegram 远程控制——全部在单个原生窗口中操作。

使用 Go 编写（单一可执行文件、纯 Go SQLite、无 CGO），桌面界面基于 WebView2。**仅支持 EVM。**

> ⚠️ **免责声明。** 本工具会处理私钥并自动发送链上交易。请仅用于你自己掌控的钱包和资金。不提供任何担保——你需对自己签名并广播的内容负责。本工具不构成任何财务建议。

---

## 功能

- **钱包（Wallets）** — 批量生成或导入，按分组管理。私钥使用本地随机密钥（`vault.key`）**静态加密**，启动时自动解锁；私钥显示功能默认关闭。
- **任务（铸造引擎）**
  - SeaDrop **公售铸造** 与 OpenSea **签名/凭证预售**（白名单）铸造。
  - **Flashbots** 私有打包（防抢跑），可调打包窗口与 priority/max gas。
  - **先模拟再执行**：对每笔交易先 `eth_call` 试运行，只有确认会成功才广播。
  - **铸造后链式动作**：成功铸造后自动 转账 / 挂单 / 接受报价 / 清空。
  - **定时启动**、按任务多 RPC 选择、分配代理分组。
- **白名单检查器** — 让每个钱包登录 OpenSea（SIWE），读取**各阶段资格**（WL / 预售 / 公售）及累计上限。结果**逐行实时显示**；可在标签页内配置**线程数** + **代理分组**，避免单 IP 限流。
- **NFT 管理** — 查看持仓，在 OpenSea（Seaport）上挂单/取消挂单。
- **RPC** — 多链节点管理，支持延迟测试与按链回退。
- **代理（Proxies）** — 分组、轮换以及实时可用性测试。
- **合约工具** — 为任意合约粘贴/拉取 ABI 并选择函数；通过交易哈希或浏览器链接重放交易。
- **设置（Settings）** — OpenSea API 密钥（**支持多个密钥并自动轮换**以规避限流）、Etherscan 密钥、Flashbots 调优、**自定义链**（无需改代码即可添加全新链）、Discord webhook 通知、界面缩放、检查更新。
- **Telegram 远程控制** — 在手机上启动/停止任务并接收通知。

## 环境要求

- **Go 1.21+** 用于构建。
- **Windows** 用于原生桌面窗口（使用 WebView2 运行时，Windows 11 已预装）。在其他系统上——或设置 `ZYPER_HEADLESS=1` 时——会在浏览器中提供相同的面板。

## 构建与运行

```bash
# Windows 桌面构建——不弹出控制台窗口，只显示 UI（纯 Go，无 CGO）。
# -H windowsgui 设置 GUI 子系统；日志仍写入 logs/ 文件夹。
CGO_ENABLED=0 go build -ldflags="-H windowsgui" -o zyper-bot.exe ./cmd/server
# （或直接运行 ./build.ps1）

# 运行——打开原生桌面窗口
./zyper-bot.exe
```

> 不加 `-ldflags="-H windowsgui"` 构建会生成附带控制台的 exe（把日志打印到终端）——
> 便于调试，但会显示一个控制台窗口。

应用会将数据保存在可执行文件旁边：`zyperbot.db`（SQLite）、`vault.key`、`logs/`，并从同一文件夹读取 `.env`。请将 `web/` 目录与二进制文件放在一起。

## 配置

所有机密都保存在本地 **`.env`**（已 gitignore）和/或应用内的 **设置** 标签页中——绝不写入源码。复制模板：

```bash
cp .env.example .env
```

| 变量 | 用途 |
| --- | --- |
| `ZYPER_ETH_RPC` | 默认的以太坊主网 RPC 地址。 |
| `OPENSEA_API_KEY` | 一个或多个 OpenSea API 密钥（用换行/逗号分隔）——自动轮换。 |
| `ETHERSCAN_API_KEY` | Etherscan V2（多链）密钥，用于拉取 ABI 与重放交易。 |
| `ZYPER_AUTH_TOKEN` | **远程** 访问 API 所需的共享密钥（通过 `X-Auth-Token` 头发送）。 |
| `ZYPER_HEADLESS` | `1` = 仅运行服务端并在浏览器中打开（VPS 模式）。 |
| `ZYPER_ADDR` | 绑定地址（默认 `127.0.0.1:0`，随机回环端口）。 |

在设置界面中填写的密钥会保存到数据库，并在下次启动时覆盖 `.env`。

## 安全模型

- 私钥在落盘前先由 vault 封装；明文仅在任务运行期间存在于内存中。
- 面板默认绑定到**随机回环端口**——它是内部窗口的传输通道，而非公开网站。
- 未设置 `ZYPER_AUTH_TOKEN` 时**拒绝远程访问**（且应置于带 TLS 的反向代理之后）。
- 私钥显示接口默认关闭（`ZYPER_ALLOW_REVEAL`）且仅限回环访问。
- **切勿提交 `.env`、`vault.key` 或 `*.db`。** 它们均已 gitignore。

## 技术栈

Go · [go-ethereum](https://github.com/ethereum/go-ethereum) · [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)（纯 Go）· [chi](https://github.com/go-chi/chi) 路由 · [go-webview2](https://github.com/jchv/go-webview2) · 原生 HTML/CSS/JS 前端。

## 许可

仅供个人与学习用途。按现状提供，不附带任何形式的担保。
