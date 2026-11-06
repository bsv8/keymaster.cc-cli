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

协议 SDK 使用正式 `github.com/bsv8/BitFSProtocol v0.6.0`，
包含买方本地付款证据 API（`BuyerPoolEvidence.latestBuyerPayment`、`buyer.InspectPool`）。
网络层使用正式 `github.com/bsv8/bitcoin-libp2p v0.3.2`。
两者均通过远端 module 下载及 go.sum 校验，不使用相邻 SDK replace；构建不要求相邻 SDK 源码。

## 使用

```sh
km <seed-hash>                    # 下载一个文件（发布需求，等待卖方；默认输出到当前目录）
km                               # 纯买方 Web Admin（下载执行管理：列表/重新下载/切回旧执行）
km -out <dir> <seed>              # 指定下载输出目录
km -listen /ip4/0.0.0.0/tcp/0 <seed>  # 向卖方公布的监听地址
km -server <path>                 # 售卖目录：扫描 + 定价维护
km -server <path> -discover       # 接入供应商，监听 Hash 需求并售卖
km -server <path> -admin 127.0.0.1:8790  # Web Admin（缺省开启；空字符串禁用）
km -config <file> ...             # 指定配置文件路径
```

纯买方访问 Web Admin（无 `-server` 目录时）：先在 `config.json` 设置
`admin.apiKey`（任意非空口令），解锁钱包后运行一次 `km`（无参数）保持常驻，
打开终端打印的 `http://127.0.0.1:8790/`，用该 API Key 登录。
该模式只服务 `~/.km/client` 下的下载执行管理（`/api/downloads/*`），不要求
卖方目录；目录与定价接口返回 404。下载过程中 `km <seed>` 也会按同一
`-admin` 地址在后台启动买方 Admin，退出即关闭；需要常驻管理页时用无参数
`km` 模式。监听地址只允许回环（127.0.0.1/::1/localhost），API Key 经
Authorization Bearer 或 X-API-Key 校验并附 Host/Origin 检查。

## 下载执行与重新下载

- 同一 Seed 的每次购买称为一个执行（`e1`、`e2`…），宿主侧池/账本/ journal 键
  统一加前缀（`e1/<seed>/pool/1`…），内核事件仍用原始池 ID；无裸 ID 兼容分支。
- 每个执行独立保存快照与 outbox（`scheduler/tasks/<seed>.<exec>.json`），当前
  执行由指针文件（`scheduler/tasks/<seed>.current`）指向。创建先写新快照再原子
  切换指针：崩溃时指针仍指向旧执行，中断产生的新鲜孤儿（revision 0）会被下次
  创建复用，不堆积。
- 正常 `km <seed>` 只解析指针并续跑当前执行，从不自动重建；新执行只能经
  Admin Web 登记、切换指针并在后台启动（同一进程内 headless 运行，无 TUI、
  自动取首个推荐名、日志走 stderr；后台任务状态见 `GET /api/downloads/{seed}/job`）。
  后台任务绑定创建时的执行编号：启动器传入 pin，下载器持锁后复核，指针已移动
  则明确拒绝，绝不悄悄启动其他执行。
- 同一进程的并发下载（含卖方与买方）共享同一 Owner/网络/费率的钱包分配器，
  不再为同一 UTXO 重复分配；Admin 关闭时取消后台任务并有界等待其收尾落盘，
  之后才退出，进程退出不会截断收尾。
- 重新下载门禁：仅当执行已停止（失败或正常停止均可，已完成除外）、无
  pending/unresolved 购买授权、无 opening/open/problem/closing 池、资金非
  problem 时允许；广播后的 `closing` 仍算未决（不代表链上确认），一律拒绝。
- 旧责任保留实际处理入口：旧快照原地保留、账本与 journal 键不改名不搬移；
  新旧池命名空间不同，迟到旧结果仍路由到旧执行。Admin 可将指针切回任一旧执行
  并直接启动（switch 传 `start: true`），后台驱动该执行的交付、关池与退款，
  而不只是审计查看或“再运行命令”的提示。
- 同一 Seed 同时只允许一个下载持有者（`scheduler/.active.lock` 文件锁）；
  重新下载与切换在持有锁时返回 409，必须先停掉在跑的 `km <seed>`。
- 点击“重新下载”登记新执行、切换指针并在后台启动（返回 `execId` 与 job 状态），
  而不是只归档返回编号；新执行的内核配置沿用当前执行，复用清单按
  已验证 staging 重建（有序哈希全列，复用只收哈希验过的块）。

钱包使用运行期内存 UTXO 分配池。分配即冻结；调用者成功提交交易（含 already-in-mempool）后立即注入自有找零；明确的节点交易拒绝主动释放，未知网络错误不释放。付款、买方出资、退款及卖方关池都经过同一成功注入出口。

manager 无持久化、恢复、交易审批或冲突取证。实例销毁丢弃冻结及本地找零，不读旧 manager 文件或 unresolved 日志重建资金；跨重启 WoC 延迟风险已明确接受。业务台账仍记录自身交易结果，不作为 manager 补偿账本。

正式依赖精确锁定已发布的 Go 根 module `github.com/bsv8/keymaster.cc-utxo-manager v0.7.0`，使用可用 P2PKH UTXO 注入接口，无组件本地替换。构建与业务 E2E 验收分别报告。

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
| `~/.km/server/` | 服务端数据（`catalog.sqlite` 内容映射、`pricing.json`、`sessions/` 售卖会话） |

## dev 调试日志

`KEYMASTER_ENV=dev` 自动开启 CLI 与 Go BitFSProtocol 的详细诊断，向 stderr 输出
`[bitfs-debug] ` 前缀的 JSON 事件；默认不创建任何日志文件，非 dev 不输出。
可由终端重定向保留，例如 `KEYMASTER_ENV=dev km … 2>>/data/home/km-debug.log`。
输出含需求/会话/调度/传输/资金/广播及 SDK 解析签名错误，使用运行编号、序号、UTC
时间与耗时串联；二进制只显示长度/哈希，禁止把秘密写入普通文本。日志不是恢复真值。

## 售卖目录与内容索引

售卖目录始终以磁盘为准：Admin 每次直接列文件系统，再关联 SQLite 中已计算的内容映射；不保存另一份文件树。数据库位于配置文件同目录的 `server/catalog.sqlite`，必须位于售卖目录之外。Admin、需求发现与卖方执行共用一个 Catalog，不保留各自的目录快照。

启动、每 5 秒和 SIGHUP 都执行普通核对：只以相对路径和文件长度判断是否需要计算 MasterSeed；同路径、同长度直接复用 SQLite，不比较修改时间、不重算全文件哈希。因此同路径同长度的内容替换不会被自动识别。Admin 的“强制重新索引”可显式完整重算；普通“刷新目录与索引”只处理新增和长度变化。实际交付的数据块仍验证请求的块哈希，不把索引复用当作交付正确性的证明。

SQLite 使用 WAL、最多 4 个只读连接与单写连接；所有写入经一个队列串行执行，哈希计算不占写事务。同一售卖根目录、同一数据库均有进程锁，拒绝多个管理进程；数据库绑定原根目录，不能拿到另一目录直接复用。不可读目录报错，不当作空目录清理；不可索引文件在 Admin 显示失败，新文件尚未完成映射时显示“尚未索引”。

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 完成 |
| 1 | 错误 |
| 2 | 参数/用法错误 |
| 3 | 文件完成但存在未决资金 |
| 4 | 用户取消（Ctrl-C / SIGTERM） |

## 钱包与链端口

UTXO manager 唯一 Go 源码位于 `../keymaster.cc-utxo-manager/utxo/`，导入路径为 `github.com/bsv8/keymaster.cc-utxo-manager/utxo`，只依赖标准库，不保留内部副本。
`internal/wallet` 负责 WoC 初始建池、真实构建/签名/提交及三类回执判断。成功后筛选当前钱包非零 P2PKH 输出，有输出才调用 `InjectP2pkhUtxos([{TxID,Vout,Value}])`；manager 不检查脚本归属，不上报已花费输入；原分配继续锁定，成功或广播未知不得释放，可连续使用找零，不隐式刷新 WoC；显式 `Refresh` 获取新观察。

正式依赖锁定 manager v0.7.0，使用产品原 go.mod 构建，不设置组件本地 replace。工程仓 `npm run parity:utxo-manager` 对照组件 TS／Go 源码，发布依赖验证与源码验证分开。无持久化或恢复测试契约。
`internal/woc` 提供 `listunspent`、`tx/raw`、`tx/<txid>/hex` 与 `chain/info`，Base URL 由
`bsvNetwork` 推导。

## 调度器

调度器 TS 与 Go 统一在独立项目维护，Go 按最新 TS 规则同步，不允许业务分叉：

- `../keymaster.cc-download-scheduler/src/`：唯一 TS 源码，包名 `keymaster-trade-scheduler`，钱包当前使用 npm `0.2.0`（0.3.0 待 npm 发布后跟进）。
- `../keymaster.cc-download-scheduler/scheduler/`：唯一 Go 源码，CLI 导入 `github.com/bsv8/keymaster.cc-download-scheduler/scheduler`；精确使用根 module `v0.3.0`，无组件本地 `replace`，构建不需要 Node.js。

两端均为纯状态机（createTask/step/restoreTask），无时钟、网络、钱包或存储副作用。
交叉测试在工程仓 `test/keymaster.cc-cli/scheduler/`（状态、命令、视图、唤醒、恢复及资金不变量）；
TS 单元测试和外围模拟位于 `test/shared/download-scheduler/`。
构建见 `../keymaster.cc-download-scheduler/README.md`；CLI 不保留 TS/Go 组件副本。统一双语言门禁为工程仓 `npm run test:components:parity`，包括双方快照交换后的续跑比较。发布包验证设 `COMPONENTS_RELEASED=1`；产品精确依赖与真实 CLI 编译运行 `npm run test:components:released`。

## 存储与卖方基础

| 包 | 用途 |
| --- | --- |
| `internal/store` | 单任务工作目录：`SaveExact` 首次写入即绑定 exact bytes，`Save` 原子替换快照 |
| `internal/ledger` | 资金账本与交易 outbox：txid 绑定原始字节、状态机与累计付款上限、未决查询 |
| `internal/catalog` | 磁盘直列 + SQLite 内容映射；按路径/长度增量计算 MasterSeed，单写队列；跳过隐藏/符号链接/零字节/临时文件 |
| `internal/pricing` | 文件定价：缺省回退、文件独立价格、批量「覆盖全部/仅未定价」、无目录继承 |
| `internal/bitfs` | 买方协议执行层：开池（先验后付）→ 采购（Kind 5/6/7，本地付款日志）→ 关池（Kind 12/13 + 按 txid 观察） |
| `internal/woc` | WhatsonChain 客户端：UTXO 查询、广播、按 txid 取回原文 |
| `internal/host` | 调度器宿主：按执行持久化内核状态 + 命令 outbox（`scheduler/tasks/<seed>.<exec>.json` + `.current` 指针）、命令幂等执行、`open/download/drain/close` 与买方执行层接线（宿主池键统一 `eN/` 前缀）、事实回灌 |
| `internal/output` | 下载文件组装：以已验证 seed 为清单逐块校验、`<seed-hash>.km` 临时文件、安全唯一名原子改名与改名崩溃恢复 |
| `internal/sell` | 卖方协议执行层：目录+定价报价、预签/验资/交付/收款/关池（卖方广播），exact 复用与先落盘后发送 |
| `internal/arbitration` | 可信端点身份校验与仲裁 info v2 查询；新池预算使用实际条目费率，不提供完整提交/取回应用工作流 |
| `internal/ssp` | SatSubscription 客户端：libp2p `/ssp/1.0.0` 长连接、uvarint 分帧、订阅/发布/投递 |
| `internal/discovery` | 需求发现：签名 Hash 请求发布（`bsv8.hash.request.v1`）与卖方验签监听 |
| `internal/transport` | BitFS 传输：`/bitfs/wire/1.0.0` + uvarint 分帧（与浏览器同一协议），卖方拨号/买方接流 |
| `internal/signal` | 浏览器 WebRTC 信令：`bsv8.webrtc.signal.v1` 收件箱消息 ↔ interconnect Envelope，会话 ID 与 PeerId 派生 |
| `internal/admin` | Web Admin：解锁后启动的本地 HTTP API + 内嵌单页界面，API Key / Host / Origin 保护；卖方模式管文件与定价，常驻买方模式（`km` 无参数）与下载内嵌模式管下载执行（列表/重新下载/切回旧执行） |
| `internal/ui` | 固定下载界面：阶段状态、进度条、地址二维码、TTY 原地刷新/非 TTY 普通日志 |

`km -server <dir> -discover` 监听需求并按 locator 拨号买方；`-admin` 启动本地管理界面。
关池由卖方广播，买方只按 txid 观察；付款不逐笔上链。

## 依赖

- [`github.com/bsv8/KeyHold/go/v2`](https://github.com/bsv8/KeyHold)：KeyHold v1 标准 SDK。
- [`github.com/bsv8/BitFSProtocol`](https://github.com/bsv8/BitFSProtocol)：BitFS 协议 SDK（Kind 1–13）。
- `github.com/bsv-blockchain/go-sdk`：BSV 原语（地址、交易、脚本）。
- `golang.org/x/term`：隐藏输入密码。

R&D 过程中的需求、施工单与测试位于工程仓库
`BSV8_Engineering/todo/keymaster.cc-cli/`、`test/keymaster.cc-cli/` 与
`e2e/keymaster.cc-cli/`；设计事实见 `BSV8_Engineering/docs/keymaster.cc-cli/`。
调度器与 UTXO manager 的组件测试、文档和历史资料统一归入工程仓 `test/shared/{download-scheduler,utxo-manager}/` 和 `docs/{download-scheduler,utxo-manager}/`；现有调度器交叉运行器位于 `test/keymaster.cc-cli/scheduler/`，不复制套件。宿主专属测试仍归 CLI；入口见工程仓 `test/shared/README.md`。
