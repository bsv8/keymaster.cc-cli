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

## 使用（新模型）

```sh
km install [--data-dir DIR]   # 安装程序、检查初始化状态、生成 systemd 服务（可重复，保留钱包与业务数据）
km init [--data-dir DIR]      # 独立初始化钱包和设置（网络最后选择），不支持覆盖已有钱包
km daemon [--data-dir DIR]    # 运行后台，已有钱包时以锁定状态启动，仅监听 127.0.0.1
km unlock [--api URL]          # 隐藏输入密码，提交给后台解锁（与 Web 共用流程）
km info [--api URL]            # 网络、锁定、地址/二维码、余额（可用/占用口径、时间/缓存/错误）
km get <seedhashhex> [--api URL]  # 提交任务并显示进度；断开只停显示，后台继续；取消用 km get --cancel <seed>
```

旧入口（`km <seed>`、`-server`、`-out`、`-admin`、`-config` 等）已退役，统一走后台 HTTP API（`--api`，默认读本机 `settings.json` 端口连 `127.0.0.1`）。后台不可连接时明确退出，不隐启第二个后台。远程访问由用户反向代理与认证负责，远程解锁请用 HTTPS。本轮不内置额外远程认证协议。

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

首次运行（数据目录无 `key.json`/`info.json`/`settings.json`）用 `km init` 在交互式终端完成：新建 key 或导入纯 KeyHold → 两次密码 → 其他设置 → 最后选择网络（`test`/`main`）→ 完整校验后持久化；管道安装交互从 `/dev/tty` 读取，无终端明确结束。systemd 只启动 `km daemon`（锁定状态），密码只经 `km unlock`（终端隐藏输入）或 Web 解锁页提交，不存参数/环境/配置/systemd 凭据。

## 配置（新布局）

| 文件 | 内容 | 恢复出厂 |
|---|---|---|
| `key.json` | 官方纯 KeyHold 文档（0600） | 保留 |
| `info.json` | 网络、公钥、网络绑定、售卖根列表、默认下载根 ID（`version`+`revision`，网络身份禁普通改） | 保留 |
| `settings.json` | 端口、主题、语言、缺省价格、调度等普通参数（`version`+`revision`） | 重置 |
| 业务数据 | 任务、账本、会话、恢复证据、索引 | 不受影响 |

- 目录默认 `~/.km`（0600/0700），系统服务建议 `/var/lib/km`，支持 `--data-dir` 显式指定。
- `info.json`/`settings.json` 统一经模块读写：读取最新→改字段→补缺省→完整校验→原子持久化→发布；后台串行更新，版本号拒绝旧页覆盖；损坏/未知版本/半套文件不作首次安装；写失败不报成功。
- 端口可重置，恢复出厂回默认，后台重启生效。`admin.apiKey`/token 已删除，无登录页。
- `key.json` 只在初始化/迁移写入；密码不持久化、不进日志/浏览器存储；解锁串行防重复 KDF；重启需重解锁。
- 网络（`bsvNetwork`）最后选择并绑定；WOC 端点由网络推导。
- 旧 `config.json` + 网络绑定外壳迁移：验证密码/公钥/HMAC 后，内部 KeyHold 密文原样存 `key.json`（不重加密），绑定移入 `info.json`，保留原文件备份。

### dev 服务覆盖

`./scripts/dev <km 参数>` 读取本仓 `.env.dev.local` 并设置 `KEYMASTER_ENV=dev`；普通 `km` 不自动读取 dotenv。也可由启动器或集成测试直接注入进程环境：

- `KEYMASTER_ENV=dev`：启用临时覆盖，不改变钱包 main/test 链网络。
- `KEYMASTER_DEV_ARBITRATION_SERVICES`：JSON 数组，元素为 `{name?, publicKeyHex, address}`，替换完整默认仲裁目录；`[]` 表示无可信仲裁。
- `KEYMASTER_DEV_SAT_SUPPLIER`：JSON 对象 `{name?, publicKeyHex, multiaddrs}`，替换默认网关。未设置的服务仍用当前钱包网络内置值。

临时目录不写回 `config.json`；公钥与完整地址中的 PeerID 必须一致。费率不写入 env，从服务查询。完整默认身份和两端配置说明见 `../BSV8_Engineering/docs/keymaster.cc/服务缺省与开发覆盖.md`。

## 目录布局（新布局）

| 路径 | 用途 |
|---|---|
| `key.json` | 纯 KeyHold（0600） |
| `info.json` | 网络身份 + 售卖根 + 默认下载根 |
| `settings.json` | 普通参数（端口/主题/语言/价格/调度） |
| `client/<seed-hash>/` | 下载/账本/付款日志/staging/组装状态/调度器快照 |
| `client/unresolved/` | 未决资金历史 |
| `server/roots/<rootID>/` | 每根 `catalog.sqlite` + `pricing.json`（多根，`rootID+相对路径` 身份） |
| `server/sessions/` | 售卖会话证据（绑定原根，删根不删历史责任，不改用他根同名文件） |

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
| `internal/daemon` | 后台唯一管理者：状态机、HTTP API（无 token）、解锁、余额、售卖根、文件、定价、下载任务、采集上报、Web 嵌入 |
| `internal/apiclient` | CLI HTTP 客户端：`--api` 优先，否则读本机 `settings.json` 端口 |
| `web/` | Vite+React 管理界面（概览/售卖/下载/文件/采集/设置，中英日三语，主题跟随系统或设置）；发布 `dist/` 经 `web/embed.go` 进二进制 |

后台解锁后自动开始多根售卖（需求发现→报价→交付，`rootID+相对路径` 身份，会话绑定原目录）；`km get` 提交任务默认落入默认下载根。关池由卖方广播，买方只按 txid 观察；付款不逐笔上链。

## 依赖

- [`github.com/bsv8/KeyHold/go/v2`](https://github.com/bsv8/KeyHold)：KeyHold v1 标准 SDK。
- [`github.com/bsv8/BitFSProtocol`](https://github.com/bsv8/BitFSProtocol)：BitFS 协议 SDK（Kind 1–13）。
- `github.com/bsv-blockchain/go-sdk`：BSV 原语（地址、交易、脚本）。
- `golang.org/x/term`：隐藏输入密码。

R&D 过程中的需求、施工单与测试位于工程仓库
`BSV8_Engineering/todo/keymaster.cc-cli/`、`test/keymaster.cc-cli/` 与
`e2e/keymaster.cc-cli/`；设计事实见 `BSV8_Engineering/docs/keymaster.cc-cli/`。
调度器与 UTXO manager 的组件测试、文档和历史资料统一归入工程仓 `test/shared/{download-scheduler,utxo-manager}/` 和 `docs/{download-scheduler,utxo-manager}/`；现有调度器交叉运行器位于 `test/keymaster.cc-cli/scheduler/`，不复制套件。宿主专属测试仍归 CLI；入口见工程仓 `test/shared/README.md`。
