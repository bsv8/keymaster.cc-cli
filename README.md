# keymaster.cc-cli

Keymaster CLI（命令 `km`）：单二进制 Go 程序，下载与售卖 BitFS 文件。

## 当前状态

买方与卖方主链路已完成，并在 **testnet 真实链路**上跑通（发现 → 报价 → 开池 → Seed/块采购
→ 关池 → 文件校验；小额测试网资金）。包含：KeyHold 钱包与网络绑定、P2PKH 钱包/WOC、
调度器（Go 与 TS 语义交叉一致）、exact-bytes 存储与资金账本、买方/卖方协议执行层、
需求发现（SatSubscription）、BitFS 传输（Go↔Go）、浏览器 WebRTC 信令、文件组装与改名、
到期退款、仲裁证据层、断线续接、TUI 下载界面、Web Admin。
剩余（阻断项）：正式下载入口接调度器（价格杆/席位/推荐名 Top5、多卖方采购）、
浏览器互通的上游报价刷新竞态、仲裁服务可用分支、部分验收矩阵项。

## 构建

```sh
go build -o km ./cmd/km

# 单文件部署（静态链接，不依赖 libc）：
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o km ./cmd/km
```

构建需要 `github.com/bsv8/go-bitfs` 包含买方本地付款证据 API
（`BuyerPoolEvidence.latestBuyerPayment`、`buyer.InspectPool`）与
`bitcoin-libp2p` 包含 `webrtcinterconnect`/会话 ID API 的版本。这两个版本尚未发布，
因此 `go.mod` 临时 `replace` 指向同级本地仓库（`../go-bitfs`、`../bitcoin-libp2p`）；
发版后删除 replace 并升级 require。

## 使用

```sh
km <seed-hash>                    # 下载一个文件（发布需求，等待卖方；默认输出到当前目录）
km -out <dir> <seed>              # 指定下载输出目录
km -listen /ip4/0.0.0.0/tcp/0 <seed>  # 向卖方公布的监听地址
km -server <path>                 # 售卖目录：扫描 + 定价维护
km -server <path> -discover       # 接入供应商，监听 Hash 需求并售卖
km -server <path> -admin 127.0.0.1:8790  # Web Admin（缺省开启；空字符串禁用）
km -config <file> ...             # 指定配置文件路径
km -pending-conclusions          # 只读列出未决 allocation/txid，不广播
km -query-conclusion <allocationId> -query-interval 5s  # 显式启动原 exact 查询/重交
```

结论查询只重交已经持久化的原交易，不重新选币、签名或变更手续费。重启及只读列表不会自动重交；Ctrl-C 停止循环但不释放未决输入。查询与下载/售卖模式独立使用，间隔至少1秒。同一实例不并行启动同一申请的多个循环，跨进程仍由钱包目录锁保护。

匹配 txid 或同次 exact 提交的精确 already-in-mempool 回执作为成功；未知/冲突诊断保留责任。冲突经单个/批量 spent API 逐 input 核实：消费方是本 tx 则成功；其他 tx 消费使原申请失败收尾，仅确认未花且不受其它责任保护的输入恢复可用。未知输入继续保护，查询结果带 `conclusion`，可查看已释放/已花/仍保护的 outpoint 与历史释放金额。已确定冲突后只继续核实，不再重交无效 exact。全部收尾返回 `delivery=failed`，命令退出码1；新付款须由调用者新建申请，原请求不会自动重选币。

CLI 的 `manager.json` 使用 `km-manager-store/1` 宿主存储封装，将原 canonical manager blob 与提交结果/尝试证据一起原子写入。新版能读取旧版裸 manager blob 并保留责任迁移；旧版不认识新封装，勿不备份直接降级或手工删除状态文件。该封装仅是文件适配，内部仍保存 TS/Go 共同 canonical manager状态；逐输入结论与failed状态需要新版阅读器。普通付款、买方出资、退款与卖方关池使用同一提交/结论路径。退款/关池重放核对并复用原 allocation、冻结事实和 exact，不重新签名；已缓存 Kind 13 不代表广播成功。未知结果保留业务 pending，可信冲突结论记 failed，只有可靠成功才推进对应业务台账。注意：上游 SDK 的交易签名回调尚未提供完整交易/prevout 授权上下文，退款/关池仍有 SDK 先签、manager 后绑定的路径；上述恢复能力不代表所有 SDK 签名均已通过 manager 签前授权。

systemd 部署模板与凭据流程见 `deploy/README.md`；运维注意事项见
`BSV8_Engineering/docs/keymaster.cc-cli/运维与部署.md`。

首次运行（`~/.km/config.json` 不存在）需要在交互式终端中选择网络
（`main` / `test`）并两次输入钱包密码；随后生成 secp256k1 私钥、加密
KeyHold 文档写入同目录的 `~/.km/key.json`、按网络区分的缺省配置写入
`~/.km/config.json`。

无人值守运行使用 systemd credentials：

```ini
LoadCredentialEncrypted=wallet-password:/etc/km/wallet-password.cred
```

进程从 `$CREDENTIALS_DIRECTORY/wallet-password` 读取密码；凭据缺失或非交互
环境无凭据时明确报错退出，绝不等待终端输入。

## 配置

- 路径：`~/.km/config.json`（设置，0600）与同目录 `~/.km/key.json`（纯 KeyHold 钱包文档，0600），目录 0700。
- config.json 可覆盖保存（如 Web Admin 改缺省价会写回）；key.json 只在初始化/迁移时写入，启动时输密码解锁。
- 不保存明文私钥或密码。
- 网络（`bsvNetwork`）在初始化时固定；WOC 端点由网络推导，不写入配置。
- `wocApiKey` 缺省为空，可填写自己的 WhatsonChain API Key。
- `feeRateSatoshisPerKb` 缺省 1000，用于钱包交易手续费估算（v1 无最高费率阈值）。
- `server.arbiterPublicKeys`：额外接受的仲裁公钥（压缩 hex）；代码内置/当前 dev 目录公钥自动加入卖方报价。
- `services.arbitrationServices`：可追加的自定义可信完整端点 `{name?, publicKeyHex, address}`。买方只选择报价与可信目录的交集，并从认证仲裁服务的 info v2 获取条目费率；查询失败不以假定费用开池。
- main/test 的仲裁和默认 SatSubscription 网关由代码内置；运行时重新注入 `bsv8` 的代码值，同时保留原配置首个启用自定义网关的发布选择，不能用持久配置改写内置身份。调度器和售价初始值随网络写入。

### dev 服务覆盖

`./scripts/dev <km 参数>` 读取本仓 `.env.dev.local` 并设置 `KEYMASTER_ENV=dev`；普通 `km` 不自动读取 dotenv。也可由启动器或集成测试直接注入进程环境：

- `KEYMASTER_ENV=dev`：启用临时覆盖，不改变钱包 main/test 链网络。
- `KEYMASTER_DEV_ARBITRATION_SERVICES`：JSON 数组，元素为 `{name?, publicKeyHex, address}`，替换完整默认仲裁目录；`[]` 表示无可信仲裁。
- `KEYMASTER_DEV_SAT_SUPPLIER`：JSON 对象 `{name?, publicKeyHex, multiaddrs}`，替换默认网关。未设置的服务仍用当前钱包网络内置值。

临时目录不写回 `config.json`；公钥与完整地址中的 PeerID 必须一致。费率不写入 env，从服务查询。完整默认身份和两端配置说明见 `../BSV8_Engineering/docs/keymaster.cc/服务缺省与开发覆盖.md`。

## 目录布局

| 路径 | 用途 |
| --- | --- |
| `~/.km/config.json` | 设置（0600，可覆盖保存） |
| `~/.km/key.json` | 钱包 KeyHold 文档（0600，启动输密码解锁） |
| `~/.km/client/<seed-hash>/` | 下载/账本/付款日志/staging/组装状态/调度器快照 |
| `~/.km/client/unresolved/` | 未决资金历史（append-only NDJSON；广播未知的交易记录） |
| `~/.km/server/` | 服务端数据（`pricing.json`、`sessions/` 售卖会话） |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成 |
| 1 | 错误 |
| 2 | 参数/用法错误 |
| 3 | 文件完成但存在未决资金 |
| 4 | 用户取消（Ctrl-C / SIGTERM） |

## 钱包与链端口

`utxo/` 是 CLI 内维护的原生 Go UTXO manager（`utm/1`），属于根 Go module，
导入路径为 `github.com/bsv8/keymaster.cc-cli/utxo`，仅依赖 Go 标准库。
`internal/wallet` 提供持久化、链上观察及提交端口；不再依赖独立 manager 仓库。
工程仓 `test/keymaster.cc-cli/utxo-manager/` 保留共享向量、Go 测试与 TS/Go 交叉恢复测试。

`internal/wallet` 实现 P2PKH 选币与签名：新鲜 UTXO 快照、逐个加输入重估费、找零、
`bsv-blockchain/go-sdk` 签名、广播后核对 txid；广播失败或结果未知写入未决历史。
`internal/woc` 提供 `listunspent`、`tx/raw`、`tx/<txid>/hex` 与 `chain/info`，Base URL 由
`bsvNetwork` 推导。

## 调度器

调度器的 TS 与 Go 实现分别由钱包和 CLI 维护：

- `../keymaster.cc/packages/download-scheduler/`：原生 TypeScript 包的唯一源码，包名仍为 `keymaster.cc-download-scheduler`，属于钱包 workspace；运行时零依赖。
- `internal/scheduler/`：CLI 使用的 Go 实现，属于根 Go module，构建 Go CLI 不需要 Node.js。

两端均为纯状态机（createTask/step/restoreTask），无时钟、网络、钱包或存储副作用。
交叉测试在工程仓 `test/keymaster.cc-cli/scheduler/`（状态、命令、视图、唤醒、恢复及资金不变量）；
TS 单元测试和外围模拟位于 `test/keymaster.cc-cli/download-scheduler/`。
TS 包构建见 `../keymaster.cc/packages/download-scheduler/README.md`；CLI 不保留 TS 副本，原独立调度器仓库不再提供源码。

## 存储与卖方基础

| 包 | 用途 |
| --- | --- |
| `internal/store` | 单任务工作目录：`SaveExact` 首次写入即绑定 exact bytes，`Save` 原子替换快照 |
| `internal/ledger` | 资金账本与交易 outbox：txid 绑定原始字节、状态机与累计付款上限、未决查询 |
| `internal/catalog` | 卖方目录索引：MasterSeed 逐文件计算 Seed，跳过隐藏/符号链接/零字节/临时文件 |
| `internal/pricing` | 文件定价：缺省回退、文件独立价格、批量「覆盖全部/仅未定价」、无目录继承 |
| `internal/bitfs` | 买方协议执行层：开池（先验后付）→ 采购（Kind 5/6/7，本地付款日志）→ 关池（Kind 12/13 + 按 txid 观察） |
| `internal/woc` | WhatsonChain 客户端：UTXO 查询、广播、按 txid 取回原文 |
| `internal/host` | 调度器宿主：内核状态 + 命令 outbox 持久化、命令幂等执行、`open/download/drain/close` 与买方执行层接线、事实回灌 |
| `internal/output` | 下载文件组装：以已验证 seed 为清单逐块校验、`<seed-hash>.km` 临时文件、安全唯一名原子改名与改名崩溃恢复 |
| `internal/sell` | 卖方协议执行层：目录+定价报价、预签/验资/交付/收款/关池（卖方广播），exact 复用与先落盘后发送 |
| `internal/arbitration` | 可信端点身份校验与仲裁 info v2 查询；新池预算使用实际条目费率，不提供完整提交/取回应用工作流 |
| `internal/ssp` | SatSubscription 客户端：libp2p `/ssp/1.0.0` 长连接、uvarint 分帧、订阅/发布/投递 |
| `internal/discovery` | 需求发现：签名 Hash 请求发布（`bsv8.hash.request.v1`）与卖方验签监听 |
| `internal/transport` | BitFS 传输：`/bitfs/wire/1.0.0` + uvarint 分帧（与浏览器同一协议），卖方拨号/买方接流 |
| `internal/signal` | 浏览器 WebRTC 信令：`bsv8.webrtc.signal.v1` 收件箱消息 ↔ interconnect Envelope，会话 ID 与 PeerId 派生 |
| `internal/admin` | Web Admin：解锁后启动的本地 HTTP API + 内嵌单页界面，API Key / Host / Origin 保护，文件与定价管理 |
| `internal/ui` | 固定下载界面：阶段状态、进度条、地址二维码、TTY 原地刷新/非 TTY 普通日志 |

`km -server <dir> -discover` 监听需求并按 locator 拨号买方；`-admin` 启动本地管理界面。
关池由卖方广播，买方只按 txid 观察；付款不逐笔上链。

## 依赖

- [`github.com/bsv8/KeyHold/go/v2`](https://github.com/bsv8/KeyHold)：KeyHold v1 标准 SDK。
- [`github.com/bsv8/go-bitfs`](https://github.com/bsv8/go-bitfs)：BitFS 协议 SDK（Kind 1–13）。
- `github.com/bsv-blockchain/go-sdk`：BSV 原语（地址、交易、脚本）。
- `golang.org/x/term`：隐藏输入密码。

R&D 过程中的需求、施工单与测试位于工程仓库
`BSV8_Engineering/todo/keymaster.cc-cli/`、`test/keymaster.cc-cli/` 与
`e2e/keymaster.cc-cli/`；设计事实见 `BSV8_Engineering/docs/keymaster.cc-cli/`。
调度器与 UTXO manager 的测试、文档和历史资料分别归入以上 CLI 分类下的
`download-scheduler/`、`utxo-manager/`；运行入口见工程仓 `test/keymaster.cc-cli/README.md`。
