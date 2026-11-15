# Marlin

基于 MyGo、Marlin Agent Core 和 Weaver 的本地桌面编码助理。目前优先支持 macOS arm64，使用系统 WebView，Go 管理桌面与会话，Rust host 管理模型和工具执行。

可以打开本地项目，让 Agent 阅读和修改文件、执行命令与测试，并在应用内查看文件和 Git diff。支持多会话、本地历史、流式回复、停止与历史续接、消息排队，以及 Kimi / DeepSeek 配置。

## 使用

1. 在设置中配置 provider 和 API Key，点击“保存并重启 host”。启动失败后也可重试。
2. 点击“打开项目”，选择信任的本地目录。这会启用命令执行、创建新对话，并加载根目录的 `AGENTS.md` / `CLAUDE.md`。
3. 描述要做的修改和验收条件。点击顶部的项目名称查看文件、分支、状态和变更，执行后点击“刷新变更”。

当前是**单活动项目**：所有会话共享当前目录，切换会话不会切换项目。目录不是沙箱；Agent 的命令具有本机用户权限。Git diff 包含已跟踪文件的暂存和未暂存修改，未跟踪文件可以在列表中预览。

## 与 T3 Code / Claude Code 的关系

[T3 Code](https://github.com/pingdotgg/t3code) 是现有编码 Agent 的控制台，可调用 Claude Code、Codex 等本机工具。Marlin 当前使用自己的模型与 shell 工具循环，没有接入这些 CLI 的会话和订阅。

[Zeron](https://github.com/zeronsh/zeron) 的项目组织、工具记录与变更审查也值得借鉴，源码依据与改进优先级见 [Zeron 设计对照](docs/project-coding/zeron-design-study.md)。

相较 [Claude Code](https://code.claude.com/docs/en/overview)，目前主要缺少：

| 能力 | Marlin 当前情况 |
| --- | --- |
| 读、改文件，执行测试 | 已有 shell 工具；提供项目入口、根目录指令和文件预览 |
| 变更审查 | 已有工作区 Git diff；缺少逐次任务快照、逐块接受/撤销 |
| 项目与并行任务 | 单活动目录；缺少每会话目录绑定、独立 worktree 和后台并行 |
| 工具透明度与权限 | 缺少工具调用卡片、持久工具记录、逐项审批与系统隔离 |
| 编码上下文 | 缺少文件附件、符号导航、上下文压缩与 IDE/LSP 集成 |
| Git 协作 | 可通过 shell 操作；缺少内置提交、分支、push 和 PR 流程 |
| 扩展与自动化 | 缺少产品级 MCP、hooks、skills、subagents 和远程任务 |

这是早期可用的编码工作流，尚未达到完整编码 Agent 的能力和可靠性。技术方案、边界与验收记录见 [项目编码文档](docs/project-coding/01-prd.md)。

## 开发

需要 Go 1.27.3、Rust toolchain、Bun 和 macOS 开发工具。依赖固定的上游版本见 `host-bin/Cargo.toml`。

```sh
bun install
bun run build
open build/darwin-arm64/Marlin.app
```

Go 检查使用 `CGO_ENABLED=0 go test ./...`；前端使用 `bun run typecheck`；Rust 使用 `cargo test --manifest-path host-bin/Cargo.toml`。配置和凭证保存在本机，不要提交配置文件。`MARLIN_DATA_DIR` 可隔离桌面状态；测试实例还应显式配置独立的 `storage_path`。
