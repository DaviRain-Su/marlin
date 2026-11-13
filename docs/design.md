# Marlin 桌面客户端技术设计文档

> 基于 `marlin-agent-app-server`（`marlin.app.v1` gRPC 协议）+ [mygo](https://github.com/egoist/mygo)（web 前端模式）
> 版本：v0.2 · 2026-11-13 · 范式：Go 桌面 App（gRPC 客户端）+ Rust sidecar host

---

## 1. 目标与非目标

### 目标
- 一个 mygo 桌面 App（macOS 优先，天然可移植到 Windows/Linux），提供与 Marlin Session 的聊天界面。
- 通过 gRPC（HTTP/2）与 Marlin App Server 通信，接口完全由上游 `marlin.app.v1` schema 定义，客户端不自造协议。
- 完整落实协议的可靠性语义：submission 幂等重试、event cursor 断线续传、取消与不确定状态展示。
- App 退出/重启后可恢复会话视图（持久化投影 + cursor）。

### 非目标（v1）
- 多 Session 创建/切换（上游明确 "Dynamic Session creation and selection need a separate integration contract"，当前 host 绑定单 Session）。
- 工具调用恢复（上游 text-turn 契约不含 tool-call recovery；`TOOL_CONTINUATION_PENDING` 状态先只做展示）。
- gRPC-Web / 浏览器部署（schema 已为此预留，后续可复用）。
- App 内配置模型路由：provider 凭证与路由属 host 边界，v1 通过配置文件传给 sidecar。

---

## 2. 系统架构

```
┌───────────────────────────────────────────────┐
│ MyGo 桌面 App（单 Go 进程，无 cgo）             │
│                                               │
│  ┌───────────┐   typed IPC    ┌────────────┐  │      ┌──────────────────────────────┐
│  │ 前端 (TS) │ ◀─Channel/Event─│ bridge 层   │  │gRPC  │ marlin-agent-app-server      │
│  │ 消息列表/  │   mygo.Bind    │ ChatService │──┼─HTTP/2─▶ (Rust host, Tonic)          │
│  │ markdown  │                │ + events    │  │loop- │  ├ SessionBinding             │
│  └───────────┘                └─────┬──────┘  │back │  │  ├ ConversationJournal     │
│                              ┌─────┴──────┐  │+auth│  │  ├ SessionEventStore       │
│                              │ client 层   │◀─┘     │  │  └ TokioAgentRuntime       │
│                              │ conn+状态机 │        │  └ WeaverModelGateway        │
│                              │ +持久化     │        └──────────────────────────────┘
│                              └─────┬──────┘              ▲
│                              ┌─────┴──────┐              │ 拉起/监控/关停
│                              │ sidecar 管理│──────────────┘
│                              └────────────┘   （cargo 产出的 host 二进制）
└───────────────────────────────────────────────┘
```

职责边界（与上游 README 对齐）：

| 组件 | 拥有 | 不拥有 |
| --- | --- | --- |
| 前端 (web) | 消息视图、markdown 渲染、输入框、Stop 按钮交互 | 任何协议状态 |
| bridge 层 | Bind 服务、Channel 桥接、typed events | 协议重试逻辑 |
| client 层 | 连接、Session 状态机、submission/cursor 持久化、错误映射 | UI、渲染 |
| sidecar 管理 | host 进程生命周期、端口/token 分配、就绪探测 | Session 语义 |
| Rust host | Session/Runtime/Journal、模型路由、provider 凭证、鉴权 | 任何 UI 状态 |

---

## 3. 版本锁定与依赖

| 依赖 | 版本/来源 | 说明 |
| --- | --- | --- |
| marlin-agent-core | 分支 `update/gerbil-boolean-inspection-gate`，**锁定 commit**（写本文时最新为 `c33fffa`） | 协议仍在高频演进（97 commits ahead of main），必须 vendored + 脚本化重取 |
| `app_server.proto` | 从上述 commit vendor 到 `proto/` | 上游同时产出 `marlin-app-v1.bin`（descriptor set），两种方式等价；vendoring 源码更利于 diff 审查 |
| mygo | `v0.9.0`（2026-11-11） | MIT |
| Go | ≥ 1.27 | |
| Bun | 前端工具链（web 模板要求） | |
| `google.golang.org/grpc` + `protoc-gen-go` + `protoc-gen-go-grpc` | 最新稳定 | grpc-go 纯 Go，无 cgo |
| Rust host 二进制 | `cargo build --release -p <app-server host>` 产物 | 打包为 App 资源；构建需一台有 Rust 工具链的 CI 机器 |

上游许可为 `Apache-2.0 AND LGPL-2.1-or-later`：vendored proto 保留其 SPDX 头即可，无传染问题（App 自身代码不链接 Rust 库）。

---

## 4. 接口层：proto → Go stubs

`app_server.proto` 定义的 service（原文摘录）：

```proto
service AppServer {
  rpc GetCapabilities(GetCapabilitiesRequest) returns (GetCapabilitiesResponse);
  rpc StreamTurn(StreamTurnRequest) returns (stream TurnEvent);          // 无 session 的单轮流式，v1 不用
  rpc StartSessionTurn(StartSessionTurnRequest) returns (StartSessionTurnResponse);
  rpc WatchSessionTurn(WatchSessionTurnRequest) returns (stream SessionTurnEvent);
  rpc CancelSessionTurn(CancelSessionTurnRequest) returns (CancelSessionTurnResponse);
  rpc GetSessionSummary(GetSessionSummaryRequest) returns (GetSessionSummaryResponse);
  rpc GetSessionTurnCancellation(GetSessionTurnCancellationRequest) returns (GetSessionTurnCancellationResponse);
}
```

生成流程（脚本 `scripts/gen-stubs.sh`，CI 校验产物无 drift）：

```sh
protoc \
  --proto_path=proto \
  --go_out=gen --go_opt=paths=source_relative \
  --go-grpc_out=gen --go-grpc_opt=paths=source_relative \
  proto/marlin/app/v1/app_server.proto
```

要点：
- `uint64` 字段（ordinal、sequence、text_bytes）在 TS 侧超 2⁵³ 会失精，桥接层一律以 string 透传（Go 侧 json tag `json:"ordinal,string"`）。
- `event_id`、`submission_id` 是 opaque 字符串，不得解析、不得重排。
- 每个 proto 变更 = 重跑脚本 + 更新本文件的语义对照表；CI 里对比 vendored proto 与上游 commit 的 hash。

---

## 5. Sidecar：Rust host 进程管理

### 5.1 host 的组装（host-bin/，已落地）

上游 `marlin-agent-app-server` 是**库**（`AppServer` + `SessionBinding`），不产独立二进制；`host-bin/` 是本仓库的薄包装 crate（git 依赖 pin 同 vendored proto 的 `c33fffa`，wire 契约零漂移）：

```
sidecar 传参/env
  → AppServer::new(ModelEndpoint, WeaverModelGateway, authorize)   // 真实模型路由
  → .with_session(SessionBinding::new(TursoAgentStorage, Project, Agent, TokioAgentRuntime))
  → tonic::transport::Server                                       // marlin.app.v1
```

要点（2026-11-13 实测打通）：
- **URL 即完整请求目标**：weaver 的 Endpoint 直接 POST，`resolve_direct_target` 只在 env 分支做归一化——配置的 base_url 由 host-bin 自行展开（Anthropic → `/v1/messages`，OpenAI → `/chat/completions`）。
- **认证**：Anthropic wire 默认 `x-api-key`（仅 `sk-ant-oat` 走 Bearer）；Kimi coding 端点需要 Bearer → kimi 路由显式 `Authentication::Bearer`（config `auth` 键可覆盖）。
- **凭证缺失快速失败**：host 启动即报错退出（否则每个 turn 都是 REMOTE_UNCERTAIN），App 横幅透出错误。
- journal 是 libsql 文件（默认 `<config>/marlin-desktop/sessions.db`）：跨进程重放、多轮上下文、unresolved 恢复均实测工作。

### 5.2 启动握手
```
App 启动
  → 选空闲端口 P（net.Listen 探测后关闭）
  → 生成一次性 token T（crypto/rand, 32B）
  → spawn host 二进制：
      --bind-addr 127.0.0.1:P
      --session-id S
      --config app-sidecar.toml （模型 endpoint + provider 凭证 + 存储路径）
      env MARLIN_HOST_AUTH_TOKEN=T （token 只进环境；argv 的 ps 可见性是 M0 遗留风险，已改）
  → 轮询 GetCapabilities（指数退避，10s 超时判定启动失败）
  → 成功：读 session_id（无值 = host 未绑定 Session，报配置错误）
```

- 通信全程 **127.0.0.1 明文**（不出环回，靠 token 鉴权；grpc-go `insecure` 凭证 + `127.0.0.1` 限定）。
- token 只进子进程环境，不落盘、不写日志、不进 argv。
- host 崩溃：client 层连接失败可见 → sidecar 管理器自动重启（上限 N 次/分钟）→ App 顶部条显示"重新连接中"。重启后 journal 持久 store（host 侧 Turso/本地库）保证 committed 记录还在，App 走 §7 的重放路径。

### 5.3 退出
App 退出时先 `CancelSessionTurn` 不需要；只需给 host 发 SIGTERM → 等待 graceful shutdown（上游 `serve_with_shutdown`）→ 超时 SIGKILL。进行中的 turn 由 Runtime 独立于连接执行，host 关停会留下 unresolved 记录，下次启动由 `GetSessionSummary` 呈现。

---

## 6. client 层（`client/` 包，无 UI 依赖）

### 6.1 连接
- `grpc.NewClient("127.0.0.1:P")` + 一元/流式 interceptor 注入 `authorization: T` metadata。
- keepalive：客户端 `KeepAliveParams`（如 30s/10s），让连接断开快速可见。
-grpc-go 自带透明重连；**流必须 App 层恢复**（见 6.4）。

### 6.2 SessionManager 状态机

```
            Send(text)                 resp OK
  EMPTY ─────────────▶ SENDING ─────────────────▶ STREAMING
    ▲                    │ resp 丢失/ABORTED/        │ Outcome 事件
    │ GetSessionSummary  │ FAILED_PRECONDITION       ▼
    │ 校准后回到 EMPTY   ▼                          FINISHED
  UNRESOLVED ◀── summary.recovery_state ── Cancel 调用中
      （UNRESOLVED / TOOL_CONTINUATION_PENDING）
```

每个状态的行为细则：

| 状态 | UI 呈现 | 允许动作 |
| --- | --- | --- |
| EMPTY / FINISHED | 输入框可用 | 发送（新 submission_id，`after_turn` = 当前已知最大 ordinal） |
| SENDING | 草稿已锁定，显示"发送中" | 不可再发 |
| STREAMING | 打字机式追加文本，Stop 可用 | Stop → CancelSessionTurn |
| Cancel 调用中 | "正在停止…" | 等待 receipt |
| UNRESOLVED | 明确横幅："上一次结果不确定，等待恢复"；输入框禁用（上游：Session 在 unresolved 时阻塞新工作） | 等待 summary 变化 / `GetSessionTurnCancellation` |

### 6.3 发送（严格照上游 walkthrough）
1. 生成 `submission_id`（ULID 或 `send-<unix>-<rand>`，1–128 ASCII）；**发送前**把 `{submission_id, user_text, after_turn}` 写入本地持久层（JSON 文件或 SQLite，见 6.6）。
2. `StartSessionTurn`。
3. 响应分类：

| 结果 | 动作 |
| --- | --- |
| OK，返回 turn | 关联草稿↔turn，本地 `after_turn` 推进到 ordinal，删 pending 记录 |
| 响应丢失（网络/进程） | 保持"发送中"，自动重试**完全相同**的请求（同 ID 同 text 同 after_turn）→ 幂等返回原 turn |
| `ALREADY_EXISTS` | submission_id 复用但输入不同：本地 bug 或持久化记录冲突；保留草稿，换新 ID 重发 |
| `ABORTED` | 并发准入竞态：退避后重试同请求 |
| `FAILED_PRECONDITION` | 活动/未决 turn 或 stale ordinal：调 `GetSessionSummary`，按 recovery_state 校准 `after_turn`，重试或呈现状态 |
| 资源超限（typed reason） | 保留草稿，展示限额原因 |

### 6.4 观察（WatchSessionTurn）与重连
- 每个 turn 一条流；事件按到达顺序 apply：`Started` → 复用发送响应的用户行；`TextCheckpoint` → **追加**其 UTF-8 文本到助手行；`Outcome` → 终态，停表。
- **cursor 规则（易错点，照抄上游约束）**：cursor 只在事件被 **apply 到视图之后**才可推进；网络收到但未 apply 不得推进。持久化时"投影 + last applied cursor"必须原子写在一起。`event_id` 当 opaque 处理。
- 断线恢复循环：
  ```
  for {
    stream := WatchSessionTurn(turn, after_event_id=<last applied>)
    for ev := range stream { apply(ev); advance cursor }
    // 流退出（网络断/EOF 无 terminal event）
    if lastOutcome != nil { break }        // 已拿到终态
    backoff; continue                       // 带同一 cursor 重连取 suffix
  }
  ```
- cursor 无效（如记录被清理）：丢弃本 turn 投影，从 turn 开头重放重建。
- 按回显的 turn ordinal + event_id 去重，防旧流迟回调污染新流（桥接层对被替换的 watch 做代际标记）。
- thinking checkpoint 存在于 journal 但被 wire 流过滤——**不要**假设收到的事件是全部 journal 记录，也不能用 journal 总量对齐进度。

### 6.5 取消
- Stop 按钮 → `CancelSessionTurn(turn)`（unary）。receipt 解读表（照上游）：

| 观察 | UI |
| --- | --- |
| `runtime.completed && remaining_tasks==0` | 本地任务已退役；outcome `COMPLETED` → 等待 watch 补全文本 |
| outcome `REMOTE_UNCERTAIN` | 显示"已停止（远端结果未知）"，**保留已收到的部分文本** |
| `runtime.completed==false && outcome UNSPECIFIED` | 退役未确认，`timeout_ms` 显示等待预算，继续 watch |
| 无 receipt | "本地凭证缺失"，可用 `GetSessionTurnCancellation` 读回记录证据 |

- `cancellation_requested=false`（对已完成 turn 的重复取消）不视为错误。
- App/窗口关闭 ≠ 取消：mygo 的方法 context 会被页关闭取消（见 §7），这只应**断开 watch**，绝不能误触 CancelSessionTurn——turn 在 host 侧继续执行，下次打开用 summary + cursor 恢复。这正是该协议的设计意图，v1 把它作为默认行为（关闭窗口 = 后台继续跑）。

### 6.6 本地持久化
单文件 JSON（app data 目录，v1 足够；后续可换 SQLite）：
```jsonc
{
  "session_id": "...",          // GetCapabilities 读到的稳定标识
  "after_turn": 3,              // 已观察的最大 ordinal
  "pending": { "submission_id": "...", "user_text": "...", "after_turn": 3 },
  "turns": { "3": { "last_applied_event_id": "c1", "projection_len": 2048 } }
}
```
启动恢复流程：读文件 → `GetSessionSummary`（它给出 latest_turn 与 recovery_state，不依赖本地 cursor）→
- `EMPTY`：清空视图；
- `ACTIVE` 且本地有该 turn 投影：watch from cursor 续播；
- `ACTIVE` 无本地投影 / `FINISHED`：watch from start 重放；
- `UNRESOLVED` / `TOOL_CONTINUATION_PENDING`：呈现横幅，禁用输入，轮询 summary。

### 6.7 错误 → gRPC status 映射（client 层统一转换，UI 只看枚举）
`UNAVAILABLE`（host 未起/崩溃）、`DATA_LOSS`（不完整流）、`ALREADY_EXISTS`、`ABORTED`、`FAILED_PRECONDITION`、`CANCELLED`（context）各映射到 6.3 表的分支；未知码归"连接问题，可重试"。

---

## 7. 桥接层（`app/`，mygo typed IPC）

```go
// ChatService 暴露给前端。
type ChatService struct{ mgr *client.SessionManager }

// Send 发送一条消息，把该 turn 的可见事件流式推给前端。
func (c *ChatService) Send(ctx context.Context, text string, turn *mygo.Channel[TurnProgress]) error {
    sub := c.mgr.NextSubmissionID()
    ch, err := c.mgr.Send(ctx, sub, text)   // 内部: StartSessionTurn + Watch 循环
    if err != nil { return err }
    for ev := range ch {
        if err := turn.Send(mapEvent(ev)); err != nil {
            return err // 前端停听: 仅断流, turn 仍后台执行 (§6.5)
        }
    }
    return nil
}

// Stop 请求取消当前 turn。
func (c *ChatService) Stop(ctx context.Context) (client.CancelReceipt, error) { ... }

// State 给前端拉取当前会话状态（启动恢复用）。
func (c *ChatService) State(ctx context.Context) (Snapshot, error) { ... }

// Reconnecting 事件: 断线重连时广播。
var Reconnecting = mygo.NewEvent[ReconnectInfo]("reconnecting")
```

关键映射（proto → IPC）：
- `WatchSessionTurn`（server-stream）→ `*mygo.Channel[TurnProgress]`（一调用一流，天然对齐；channel 关闭 = watch 结束）。
- 需要页面级广播的状态（重连中、unresolved 横幅）用 `mygo.NewEvent` + `Emit`。
- `uint64` 序号在 `TurnProgress` 里以 string 编码。
- 桥接层不做协议决策（不重试、不推进 cursor），全部委托 client 层——保证语义只有一个实现点。

前端侧（`mygo generate` 产出的 TS client）：
```ts
const ch = new Channel<TurnProgress>();
const sending = Chat.send(text, ch);
for await (const ev of ch) appendMarkdown(ev);   // Started/TextCheckpoint/Outcome
await sending;                                   // reject = Send 失败, 草稿保留
```

---

## 8. 前端（`frontend/`，Bun + Vite + React/Svelte 任一）

- 消息列表：用户行 / 助手行；助手行按 turn ordinal 组织，流式 append markdown（增量渲染 + requestAnimationFrame 节流）。
- Stop 按钮：STREAMING 态可见；点击乐观显示"正在停止…"，由 receipt 三态（§6.5）更新终态文案。
- 状态横幅组件：UNRESOLVED、REMOTE_UNCERTAIN、重连中、限额拒绝——四类文案由 `Snapshot`/事件驱动。
- 输入框禁用逻辑严格跟随后端状态机（client 层经 `State()` 下发），前端不自行判断。
- 不实现：历史搜索、多会话、设置页里的模型路由（host 配置文件承担）。

---

## 9. 目录结构

```
marlin/
├── proto/                      # vendored app_server.proto（保留 SPDX 头）
├── gen/marlin/app/v1/          # protoc-gen-go(-grpc) 产物
├── scripts/
│   ├── vendor-proto.sh         # 锁 commit 拉取 proto
│   └── gen-stubs.sh
├── client/                     # §6：conn.go / session.go / persist.go / errors.go
├── sidecar/
│   ├── sidecar.go              # §5：spawn/token/port/就绪探测/守护
│   └── config.go               # app-sidecar.toml 模板与校验
├── app/
│   ├── main.go                 # mygo 入口（Bind 在 App.Run 前）
│   ├── bridge.go               # §7：ChatService + events
│   └── snapshot.go             # 状态快照类型（TS 侧同构）
├── frontend/                   # Bun/Vite 工程
│   └── src/mygo.ts             # mygo generate 产物（不手改）
├── host-bin/                   # CI 构建的 Rust host 二进制（打包资源）
├── mygo.config.ts
├── go.mod
└── docs/design.md              # 本文档
```

---

## 10. 里程碑

| 阶段 | 内容 | 验收 |
| --- | --- | --- |
| M0 骨架 | mygo init（web 模板）、vendor proto、生成 stubs、CI（stubs drift + cargo build host） | `mygo dev` 起空白窗口；stub 构建通过 |
| M1 sidecar 通道 | spawn + token + 就绪探测；`GetCapabilities` 打通并显示 session_id/限额 | 手动 `cargo run` 的 host 也能连（开发模式旁路 spawn） |
| M2 单轮对话 | Send → Watch 流式到前端 markdown 渲染；完成后 ordinal 推进 | 真实模型一问一答，流式可见 |
| M3 可靠性 | 幂等重试、断线重连、cursor 持久化与启动恢复、取消三态、unresolved 横幅 | 拔网线/杀 host 进程/重启 App 三场景各过一遍；重试不改 submission_id |
| M4 打包 | `mygo build`，host 二进制入包，签名/公证 | 干净机器双击可用 |

---

## 11. 风险与开放问题

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| 协议高频演进（分支尚在 review，97 commits ahead） | stubs/语义漂移 | 锁 commit + vendoring 脚本 + 语义对照表随 proto 变更强制更新 |
| 当前 host 绑定单 Session | 无多会话 UI | v1 明确不做；等上游 integration contract |
| host 二进制体积与 CI（需 Rust 工具链构建/分发三平台） | 打包复杂度 | host 构建单独 job；先只做 macOS（与需求一致） |
| unresolved turn 阻塞新工作 | 用户可被卡住 | UI 一等状态展示 + `GetSessionTurnCancellation` 读证据；等上游 provider reconciliation |
| after_turn 竞态（两窗口/重放并发发送） | ABORTED 抖动 | 单窗口假设 v1 成立；重试路径已覆盖 |
| journal 是 libsql 单文件锁 | dev 窗口与打包 App 同时运行会抢锁（第二次 spawn 靠 sidecar 重试缓解，仍不可共存） | 单实例假设 v1 成立；多实例/并发写入等上游 storage 支持后再放开 |
| thinking 不上 wire | 进度条无依据 | 不展示 thinking 进度；文本即进度 |

**待上游确认**：
1. `FAILED_PRECONDITION` 拒绝新 turn 时是否返回 typed reason 的具体字段（文档描述了需求但 schema 里尚无专门字段）。
2. host 侧 `authorize` 回调的 metadata key 约定（token 注入点需按实现对齐）。
3. `TOOL_CONTINUATION_PENDING` 恢复路径落地后是否新增 RPC。
4. 插件相关的三个契约缺口（审批/权限 RPC、插件 operator RPC、scoped panel 客户端契约），见 §12.6。

---

## 12. 插件体系（扩展设计）

> 决策（2026-11-13）：保持 Marlin Plugin Contract（RFC 40.166）作为 agent 插件的信任边界，
> 以 DeepSeek Harness（dsh）为机制参考。App 是 mediator，不是第二执行引擎。

### 12.0 两条扩展轴，一个原则

| 轴 | 扩展谁 | 机制 |
| --- | --- | --- |
| A | 桌面 App 自身 | mygo 插件（Go service + npm JS 包） |
| B | agent 语义（策略/工作流/工具） | Marlin bundle（数据包），装在 Rust host |
| C | 插件的 UI 面板 | bundle 携带的 TS 客户端资产，进 App 前端受信 shell |

原则：插件语义只存在于 host；App 永远是 mediator，不复制 agent 的安全边界。

### 12.1 轴 A：App 自身插件（mygo，现在就能做）

- `mygo.Plugin{Name, Service}` Go 半 + npm 包 JS 半，命名空间 `plugin:<name>`，仅 trusted 页面可调。
- 用途：SQLite（会话本地历史/草稿持久化，替代 §6.6 的 JSON 文件）、fetch（无 CORS）、watch、terminal 等。
- 管的是"App 好不好用"，与 agent 插件完全隔离，禁止借道访问 gRPC 凭证。

### 12.2 轴 B：agent 插件（Marlin Plugin Contract v0，RFC 40.166）

上游契约要点（摘自 `docs/40-rfcs/40.166-plugin-contract.org`，分支内原文）：

- 插件 = **bundle**：TOML envelope + 声明（选择已注册受信任 Module，如 AI Agentic Lib 的 Module Interface）+ typed options，data-only。
- **禁止注入代码**：安装插件不授权加载 Scheme 源码/编译模块/动态库/JS 进引擎。
- effects 必须 resolve 到 host 已有 Rust 操作，每次过实时授权；Rust 引擎拥有 catalog、项目授权、budgets、graph admission、取消与 typed receipts。
- 策略平面：Gerbil Scheme VM（C ABI 桥，受信任，host provisioning）；插件只向 allowlist 的 `PolicyModuleId` 提供数据。
- 状态：**Design proposal，代码未实现**（无 plugin gRPC 服务；"plugin RPCs and plugin SDK bindings are proposed integration work"）。

App 承担 RFC 的 **project operator** 角色：

- **v1（配置文件版）**：App 提供管理 UI（列出已装 bundle、enable 开关、按 module 发布的 typed options 渲染设置表单、capability 审批展示），写入 sidecar 的分层 TOML——内置默认 → 项目层 → 用户层（吸收 dsh profile/patch 的分层组合思想）。激活由 host 在启动时完成（resolve manifest → pin typed plan 与 policy epoch），重启生效。
- **v2（gRPC 版）**：上游 engine-owned plugin gRPC 服务落地后，operator UI 切到 RPC 调用；因 `client/` 层独立，切换只发生在该层，前端与 UI 不动。

### 12.3 轴 C：插件面板资产（受信 shell）

- bundle 的 npm 客户端包（prebuilt assets，RFC："A bundle can also contribute assets for a client that supports the declared interface"）加载进我们的 web 前端。
- **面板不持有 gRPC token / host 凭证**，不开放 gRPC-Web 网关给面板。
- 前端实现 slot registry（`single` / `list` / `keyed` 三种 cardinality，借鉴 dsh Web Client Slots 的声明-注入模型）：宿主组件声明挂载位置，面板按位置注册；注册/卸载跟随 bundle 激活生命周期，禁止 import 其他插件的组件。
- 通信：scoped events（`mygo.NewEvent` 广播）+ scoped actions（App Bind 的受仲裁服务，逐条映射到 host 允许的操作）。
- 隔离：面板页面独立 origin，`PageOptions.TrustedOrigins` 只信任自家；面板慢/挂不影响引擎（RFC："A slow or absent client cannot block the engine"）。
- 上游 declared client contract 稳定前，用我们定义的 scoped panel 协议过渡。

### 12.4 参考架构对照：DeepSeek Harness（dsh）

dsh（`deepseek-ai/deepseek-harness`，内核 Cordis）："产品里一切皆插件"（model adapter、tool registry、session log、agent loop 全部可替换），插件 = 进程内 TS 代码。Marlin RFC 明确对比后选择了更严的信任边界（数据包而非代码）。

| dsh 概念 | 采纳 | 我们的对应 |
| --- | --- | --- |
| Web Client Slots（typed slot 注册表） | 抄思想 | §12.3 的 slot registry |
| Tool 展示卡词汇表（`presentCall`/`presentResult` 的 `terminal`/`diff`/`search`/`read`/`web`/`generic` card） | **直接采用** | agent 工具调用 UI 的渲染词汇，provider 中立，换 agent 后端不改 UI 层 |
| `tools/pre-execute` 的 `ask` 审批 + monotonic guard（只能拒绝不能放行） | 抄思想 | App 审批 UI 的决策语义；见 §12.6 gap 1 |
| Profiles + patch 分层组合 | 抄思想 | §12.2 v1 的分层 TOML |
| Cordis 进程内代码插件（fiber 生命周期/HMR） | 不抄 | 与 Marlin 信任模型冲突 |
| 桌面形态（Electron 装签名 runtime、Desktop Host、OS 分配端口） | 已同构 | mygo + sidecar + 一次性 token，方向验证 |

### 12.5 落地顺序（并入 §10 里程碑）

1. M0–M3 照旧（Session 链路是插件的一切前提）。
2. M2 顺手启用轴 A：`mygo.Use(SQLite, fetch)`。
3. M3.5：轴 B 的 operator UI v1（配置文件版）+ 分层 TOML。
4. 推动上游定稿 plugin RPC（RFC 刚发布两天，正处意见窗口期）。
5. plugin RPC 落地后：operator UI 切 gRPC（M5）。
6. 轴 C 等上游 declared client contract 稳定，先用本地方案过渡。

### 12.6 与上游的契约缺口（合并进 §11 待确认清单）

1. **审批/权限 RPC**：dsh 的 `tools/pre-execute` 有 `ask` 决策（走审批服务），marlin 当前 app-server 协议是纯 text-turn，无审批/权限通道；工具调用本身也在契约外（"Tool-call recovery is outside the current text-turn contract"）。需要上游定义 permission request/response RPC 或复用 ACP 权限语义。
2. **插件 operator RPC**：列 bundle、enable、settings、capability 审批的管理接口尚不存在（RFC 标为 proposed）。
3. **scoped panel 客户端契约**："Plugin activation and scoped panel actions have their own contracts"（RFC 原文），契约未发布；定稿前用 §12.3 本地方案。

---

## 附：协议语义速查（实现时对照上游 README）

- submission_id：1–128 ASCII `[A-Za-z0-9_-]`，Session 内唯一；相同 ID+相同输入 → 原 turn；不同输入 → `ALREADY_EXISTS`。
- after_turn：客户端最后观察到的 turn ordinal；空 Session 为 0；stale 会被拒。
- event_id：opaque；cursor 语义 = "本 turn 内已交付的排他位置"；terminal cursor 重放 → 空 suffix。
- 去重键：`turn (session_id, ordinal) + event_id`。
- 事件只是 journal 的可见投影；journal 可能有 wire 流不出现的记录（thinking）。
- Session 关闭/窗口关闭不断 turn；turn 归 Runtime，连接只是观察者。
