# codew

codew 是一个面向跨境业务网络节点管理的控制台，用于统一管理专线模式、中转入口、落地出口、固定出口客户、SOCKS5 路由中转、Clash/Mihomo 订阅和网络核心状态。

## 当前版本

- 源码开发版本：`0.7.8-stable-engineering`
- 已发布版本：`0.7.8-stable-engineering`（Codew 首版），见 [GitHub Release v0.7.8](https://github.com/toolbbs/codew/releases/tag/v0.7.8)。
- 首版仅支持 **Ubuntu 22.04 LTS、x86_64（amd64）、fast/systemd 全新安装**。本地 ZIP 安装和重启，以及使用预发布测试清单的在线安装和客户端联网已通过真机验证。
- 在线升级、旧 ZXY Panel 原地迁移、Debian、Docker、其他架构和跨模式切换不在本次验收范围内。下方旧品牌发布记录仅供历史参考。

## 功能列表

- 跨境业务网络节点管理
- 专线模式客户管理
- 中转入口与落地出口配置
- 固定出口客户保持一客户一入口一出口
- 直连客户与固定出口客户分流管理
- SOCKS5 路由中转
- 客户专属入口与固定出口能力
- VLESS Reality 入站与客户绑定
- Clash Verge / Mihomo 远程订阅：`/sub/<token>?format=clash`
- Clash YAML 下载：`/sub/<token>?format=clash&download=1`
- 端口、UUID、SOCKS5 账号绑定一致性检测
- Agent 下发、写入、重启后的配置一致性校验
- 网络核心版本展示，优先读取 Agent 上报版本
- 系统升级页与远程版本清单配置
- 生成升级命令与更新检查入口

## 一键安装

只在未安装过 Codew 或 ZXY Panel 的 Ubuntu 22.04 x86_64 服务器上执行。先创建整机快照，再以 root 身份在 SSH 终端整段粘贴。命令会校验固定版本的安装脚本，通过正式清单下载并校验 ZIP，安装缺少的依赖并启动服务。

```bash
f=$(mktemp /tmp/codew-install.XXXXXX) &&
curl -fsSL --proto '=https' --proto-redir '=https' -o "$f" \
  'https://raw.githubusercontent.com/toolbbs/codew/ae693d4df4fd5e1dcd5bb3acc8769070dfcd41fd/install.sh' &&
printf '%s  %s\n' '7350b7029e903dad326ffdb52caee0e2ee624e18cae59c90f07756d299b15606' "$f" | sha256sum -c - &&
CODEW_INSTALL_MODE=fast FRESH_INSTALL=false \
  CODEW_UPDATE_MANIFEST_URL='https://raw.githubusercontent.com/toolbbs/codew/main/version.json' \
  bash "$f"
```

安装完成后请更换管理员密码。不要公开包含密码、Token、完整面板入口或节点链接的安装日志。如果安装报错，先保留脱敏日志，不要直接重复执行。

## 升级与迁移

首版未验收在线升级或旧品牌迁移。以下命令仅检查更新并在存在可升级版本时输出供审核的命令，不会直接执行安装：

```bash
codew update
```

不要用本版安装器覆盖旧 ZXY Panel，也不要把找到更新视为升级已通过验收。

## 已知问题

安装日志偶尔会在 BBR 实际开启时误报未开启。请以 `codew-netopt bbr-status` 的实际状态为准；显示 `Enabled: true` 时不需要重复开启或重装。

## 常用命令

```bash
codew info
codew status
codew restart
codew logs
codew doctor
codew backup
codew backup-list
codew update
codew uninstall
```

## 旧品牌 V0.7.8 发布说明

- 保留 BBR、版本治理及 R1/R1.5 Server、Node、Relay、Client 绑定护栏和保存失败保护。
- 完善 Node/Relay 错误提示及保存状态，避免重复提交和草稿丢失。
- 修正 systemd 服务路径和严格 Xray drop-in 归属检查，支持已验收的重复安装和配置/数据恢复。
- Ubuntu 22.04 amd64 fast/systemd 的 V0.7.7.5 手动升级、后台托管升级、旧数据保留、原客户端联网、重启恢复及 300 秒空闲观察通过。
- 原样使用已验收 ZIP，不重建二进制、前端或安装包。当前源码的发布文档更新不在该 ZIP 内，完整差异在包外说明中列明。
- BBR 安装日志可能误提示未开启；网络波动原因未确认。完整限制与证据边界见 [发布说明](docs/releases/v0.7.8.md)。

## V0.7.7.5 历史发布说明

V0.7.7.5 是基于 V0.7.7.4 的稳定性收尾版本，不增加高风险新功能，重点处理测试过程中反复出现的发布一致性、客户端导入误判和安装自检提示问题：

- 统一 README、CHANGELOG、构建脚本、前端文案、后端版本、Agent 版本和安装脚本版本。
- `scripts/build-fast-release.sh` 支持动态 VERSION / CODENAME，默认打包 V0.7.7.5 stability-polish。
- Release 包不再内置带 SHA256 的 `version.json`，避免发布包内部 manifest 与外部 release manifest 互相引用导致 hash 不一致。
- Fresh install 没有备份、尚未创建客户绑定时，`zxy-panel doctor` 改为信息提示，不再误报 warning。
- 客户分享弹窗补充 Clash Verge / Mihomo 使用提示，明确订阅源端口不是节点端口、导入后需在代理页选节点、内核通信错误时应重启内核或客户端进程。
- Clash YAML 顶部增加客户端使用说明，减少“导入成功但当前节点为空”的误判。
- 保留 V0.7.7.3 Clash/Mihomo 订阅绑定修复，多客户多端口订阅不再混淆。
- 保留 V0.7.7.2 端口 UUID BindingCheck、Agent apply 校验、Doctor 深度检测和客户 merge update。

## 安装后自检

安装或升级完成后执行：

```bash
codew doctor
```

doctor 检查实际部署模式、端口、可选组件、数据库及 API/Nginx 健康 JSON 和版本。无备份或未安装可选组件显示 INFO；PASS 不等于客户端联网、绑定深度核验或真实部署验收通过。基本健康检查通过时包含：

```text
Doctor result: PASS
```

## 配置与数据恢复

配置/数据备份不包含程序。恢复只接受本 CLI 新格式、同安装目录/配置目录/数据库映射及同模式的可信备份，保留当前程序、服务定义和 Nginx 路由。旧式系统根目录归档不自动解压。整版回滚须使用预先验证的完整虚拟机和数据盘快照，不要用旧包重新安装代替回滚。

## Clash Verge / Mihomo 使用提醒

Clash Verge 首页显示的“来源：服务器IP:面板端口”是订阅源地址，不是节点转发端口。真正的节点端口需要到“代理”页面或订阅 YAML 的 `port` 字段查看。

导入 Clash 订阅后，如首页显示“暂无激活的代理节点”或“内核通信错误”，请先：

1. 进入“代理”页面手动选择 `PROXY` 分组里的节点。
2. 重启 Mihomo / Clash 内核。
3. Windows 环境可尝试以管理员身份启动 Clash Verge。
4. 如仍无流量，先关闭 TUN，只开启系统代理测试；确认可用后再开启 TUN。

## 发布一致性说明

`VERSION` 是源码版本基准。根目录 [`version.json`](version.json) 是 Codew 的正式清单，指向 `v0.7.8` 的已验收原版 ZIP。下载后可用 Release 中的 [`SHA256SUMS`](https://github.com/toolbbs/codew/releases/download/v0.7.8/SHA256SUMS) 核对完整性。

预发布的 `version-test.json` 只供测试，不是正式入口。本地候选使用独立的 `version.fast.json` 和 `SHA256SUMS`；ZIP 不内置正式清单。

开发状态检查（默认模式）验证源码版本和示例清单，未发布时无需根目录 `version.json`。正式清单存在时仍会检查其格式、包名、下载地址和 SHA256 字段：

```bash
node scripts/check-version-consistency.mjs --mode dev
```

在隔离的源码副本中，使用 Go、Node/npm、Python 3 和 Bash 构建本地候选。输出目录必须不存在，已有候选不会被覆盖：

```bash
bash scripts/build-fast-release.sh
```

默认输出到 `dist-release/`，也可通过 `CODEW_RELEASE_OUT_DIR` 指定一个新目录。构建重新生成 Linux amd64 二进制及前端，文件留在该目录下的隔离副本中，不覆盖源码树的 `bin/` 或 `frontend/dist/`。

复核候选时必须显式指定候选清单和 ZIP，即使正式清单尚未发布也会检查实际 ZIP 的 SHA256：

```bash
node scripts/check-version-consistency.mjs --mode release --manifest dist-release/version.fast.json --package dist-release/codew-v0.7.8-stable-engineering.zip
node scripts/test-fast-release-package.mjs dist-release/codew-v0.7.8-stable-engineering.zip
```

构建脚本默认读取 `VERSION`，显式版本参数必须与源码相符。历史 CHANGELOG、旧发布目录、持久化版本元数据和未重新构建的产物不属于源码版本检查范围。候选检查通过不代表完成服务器安装、Agent 或 Xray 真机验收。
