# Zeron 设计对照

2026-11-15。问题：Zeron 哪些设计能改善 Marlin 的项目编码体验？

研究固定到 [`93bed8f`](https://github.com/zeronsh/zeron/tree/93bed8f3979613ba4f5e87884e9dcf93b2876ff4)，由产品界面和引擎架构两个独立方向阅读源码，再进行独立核查。没有安装、运行或登录 Zeron；源码存在不等于运行验收通过。

## 最值得借鉴的部分

Zeron 把执行位置、会话、运行过程、待响应事项、代码变化组织成完整工作区。
Marlin 当前主要还是围绕聊天组织功能。下一步应先解决“在哪里执行、正在做什么、
改了什么、哪里需要我处理”这四个问题。

| 设计 | Zeron 的源码证据 | Marlin 当前边界与借鉴方向 |
| --- | --- | --- |
| 项目在会话之上 | [默认分组](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/shell/sidebar.luau#L53-L60) 按项目、设备组织；[组装实现](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/shell/sidebar.luau#L473-L558) 提供 agent、状态和分支 | 当前侧栏仍是平铺会话；只有一个全局项目。先记录会话的项目来源，再做可信的项目分组 |
| 发送前明确执行位置 | [where.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/chat/where.luau#L579-L603) 生成设备、项目、checkout/worktree、分支计划 | 先持续展示实际目录与分支，不能把普通目录标成隔离 worktree |
| 工作过程有可展开记录 | [activity.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/ds/activity.luau#L579-L702) 展示工具树与工作摘要；[tool_detail.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/ds/tool_detail.luau#L346-L380) 折叠输出、显示退出状态 | 缺少工具事件与耐久记录。先补真实数据及关联 ID，再做工具卡片 |
| 需要响应的状态突出 | [sidebar.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/shell/sidebar.luau#L524-L547) 优先显示审批、问题；[asks.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/chat/asks.luau#L197-L205) 绑定具体请求 | Marlin 没有审批执行协议；可先清楚显示运行、失败、待恢复，不能只增加审批按钮 |
| 代码变化有比较范围 | [Git scope 定义](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/plugins-builtin/git/view.luau#L24-L26) 区分 working、branch、turn；[打开实现](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/plugins-builtin/git/view.luau#L352-L364) 应用具体 scope | 当前只有相对 HEAD 的工作区 diff，不能把已有用户修改都归给 Agent；逐轮 diff 需要额外基线 |
| 文件与终端按执行位置组织 | [终端面板](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/panel/terminal.luau#L24-L67) 按会话位置创建终端；关闭前检查运行状态 | Marlin 当前只有文件/变更只读面板，终端需要进程和会话生命周期设计 |
| 队列编辑不破坏原草稿 | [asks.luau](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/apps/zgx/ui/screens/chat/asks.luau#L57-L105) 暂存并恢复文字与附件 | 当前只有队列计数、恢复、清空；应增加逐条预览、编辑、移除，并持久化草稿/队列 |

信息布局建议：左侧放项目和会话；中央放任务对话和真实工作记录；右侧或辅助面板放文件、
diff、终端；输入区附近显示实际执行位置与待处理事项。第一步可以保留当前文件/diff modal，
避免为了布局改动一次重写整个前端。

## 为什么它能支撑这些界面

- **它集成的是编码 Agent。** [Claude adapter](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/harness/src/claude/mod.rs#L432-L494) 使用 stream-json；[Codex adapter](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/harness/src/codex/mod.rs#L933-L980) 使用 app-server。Marlin 当前直接调用模型并维护 shell 循环，不能靠复制界面获得这些运行时能力。
- **执行状态属于 engine。** [RPC 实现](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/rpc/src/lib.rs#L947-L954) 的内存连接也走消息协议；[engine 依赖](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/engine/Cargo.toml#L8-L53) 不依赖前端 app/UI crate，但包含视觉辅助组件。Marlin 可继续用 MyGo typed IPC，让 UI 发送命令、消费真实状态。
- **目录和会话有独立身份。** [Space](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/proto/src/entities.rs#L112-L140) 区分路径、设备、checkout、repository；[Chat](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/proto/src/entities.rs#L262-L303) 另有 space 与 runtime resume 身份。
- **工具事件与显示摘要分层。** [run_journal.rs](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/engine/src/run_journal.rs#L172-L217) 保存带序号的事件；[摘要常量与截尾实现](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/doc/src/parts.rs#L70-L168) 将命令输出摘要限制为最后 10 行或 1024 字节。不能据此声称磁盘占用有上限或断电零丢失。
- **Stop、打断当前轮和崩溃恢复是不同动作。** [sessions.rs](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/engine/src/sessions.rs#L1762-L1834) 的用户 Stop 会终止 runtime；[恢复逻辑](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/crates/engine/src/sessions.rs#L1940-L2073) 有近期轮次与尝试次数边界。Marlin 应先改善中断原因与结果的表达，再考虑后台任务和自动恢复。

## 建议的优先级

1. **明确项目与会话的关系。** 保存 workspace 来源，显示实际执行目录、分支和上下文。
   可以先串行切换并重绑 host；并行项目需要决定隔离 host 或扩展请求路由。
2. **补可观察的编码过程。** 定义 turn/tool ID、开始/结束/失败、退出码、有限输出、取消原因和回放游标。
   当前固定 text-turn gRPC 不携带这些事件，需要正式设计数据通道。
3. **让变更容易核查。** 提供逐文件 diff、明确比较基准、区分已有修改和本轮改动，然后再做逐块接受/撤销。
4. **完善输入与队列。** 持久化草稿、逐条队列操作；会话切换不丢待输入内容。
5. **将 Agent 后端、worktree、daemon、远程同步作为单独决策。** 不为模仿 Zeron 而悄悄替换 Marlin 内核。

## 证据边界

- 发布工作流默认选择 `apps/zgx`（Rust host + Luau UI），保留 GPUI 回退：
  [release.yml](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/.github/workflows/release.yml#L52)。
  线上仓库变量未验证，不能声称所有已安装版本使用同一前端。
- [README 截图](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/docs/media/readme/app-screenshot.jpg)
  只证明初始画面的布局，不证明侧栏、工具时间线和文件面板的运行行为。
- 部分架构文档滞后，不能只据概览宣称 watchdog、恢复或更新保证。
- [MIT LICENSE](https://github.com/zeronsh/zeron/blob/93bed8f3979613ba4f5e87884e9dcf93b2876ff4/LICENSE)
  与 third-party notices 已检查；本轮只借鉴设计，没有复制源码或视觉资产。
- 本轮没有验证各 Agent 的实际功能覆盖、跨设备执行、供应商订阅、性能或系统级隔离。

独立核查判定 PASS，并指出产品/UI 覆盖深度和引用精度的小缺口。按反馈补入终端面板、
Git scope 执行位置、默认分组和摘要数值的直接引用，收窄 engine/UI 依赖表述。
界面判断仍限于源码和静态截图，未做 Zeron 运行验证。
