// tool_loop: an in-host agent loop behind the text-turn contract.
//
// The vendored marlin.app.v1 wire carries text turns only (started / text /
// outcome): a model tool request would end the turn as ToolRequested and
// leave the session unable to admit more work (the app-server refuses
// history containing pending tool requests). This gateway closes the loop
// INSIDE the host instead: when the provider asks for tools, the host's
// policy decides, the shell handler executes, and the conversation
// continues with the tool results — the outside world sees one clean text
// turn. This is the seam the design doc reserves for the host (§5.1);
// upstream approval RPCs will replace the policy gate when they land
// (docs/design.md §12.6 gap 1).

use std::{collections::HashMap, path::PathBuf, sync::Arc};
use tokio::io::AsyncReadExt;

const PROJECT_INSTRUCTION_LIMIT: u64 = 32 * 1024;

use marlin_agent_protocol::{
    ModelGatewayCompletionChoice, ModelGatewayCompletionOptions, ModelGatewayCompletionResponse,
    ModelGatewayError, ModelGatewayFuture, ModelGatewayMessage, ModelGatewayMessageRole,
    ModelGatewayRequest, ModelGatewayResult, ModelToolCall, ModelToolName, tool_gateway_message,
};
use marlin_agent_runtime::{
    ModelToolDeclaration, ModelToolOutcome, RuntimeContext, ShellToolDeclarationRequest,
    ShellToolHandler, ToolRuntime,
};
use marlin_agent_stream::{
    ModelGatewayEventStream, ModelStreamChunk, ModelStreamEvent, ModelStreamingGateway,
};
use tokio_stream::StreamExt;

/// Declared tool surface of the host: one shell tool bound to a workspace.
pub struct ToolSurface {
    pub offer_options: ModelGatewayCompletionOptions,
    pub handlers: HashMap<String, Arc<ShellToolHandler>>,
    pub allow: Vec<String>,
    workspace: PathBuf,
}

impl ToolSurface {
    /// Builds a single `run_command` shell tool rooted at `workspace`.
    /// Tools are opt-in: without `tools_workspace` the host offers none.
    pub fn shell(
        workspace: PathBuf,
        allow: Vec<String>,
    ) -> Result<Self, Box<dyn std::error::Error>> {
        let workspace = workspace.canonicalize()?;
        if !workspace.is_dir() {
            return Err("tool workspace must be a directory".into());
        }
        let name = ModelToolName::new("run_command").map_err(|e| format!("tool name: {e}"))?;
        let declaration = ModelToolDeclaration::shell(ShellToolDeclarationRequest {
            name,
            description: "Run one shell command inside the workspace and return its output."
                .to_owned(),
            argument: "command".to_owned(),
            timeout_seconds: 60,
            output_limit: 64 * 1024,
        })
        .map_err(|e| format!("invalid shell declaration: {e}"))?;
        let definition = declaration.definition();
        let handler = Arc::new(ShellToolHandler::new(workspace.clone(), declaration));
        let mut handlers = HashMap::new();
        handlers.insert(definition.name.as_str().to_owned(), handler);
        Ok(Self {
            offer_options: ModelGatewayCompletionOptions {
                tools: Some(vec![definition]),
                ..ModelGatewayCompletionOptions::default()
            },
            handlers,
            allow,
            workspace,
        })
    }

    async fn project_context(&self) -> String {
        let mut text = format!(
            "You are a coding assistant working on the local project at {:?}.\n\
             run_command starts in this directory; this is not a filesystem sandbox.\n\
             Inspect existing files and Git changes before editing. Preserve user changes. \
             Read scoped AGENTS.md instructions before editing nested directories. \
             Use the project's checks to verify edits. Report actual command results and \
             unfinished work honestly. Do not commit, push, or publish unless requested.\n",
            self.workspace,
        );
        for name in ["AGENTS.md", "CLAUDE.md"] {
            let Ok(path) = self.workspace.join(name).canonicalize() else {
                continue;
            };
            if !path.starts_with(&self.workspace) {
                continue;
            }
            let Ok(metadata) = tokio::fs::metadata(&path).await else {
                continue;
            };
            if !metadata.is_file() {
                continue;
            }
            let Ok(file) = tokio::fs::File::open(&path).await else {
                continue;
            };
            let mut bytes = Vec::new();
            if file
                .take(PROJECT_INSTRUCTION_LIMIT + 1)
                .read_to_end(&mut bytes)
                .await
                .is_err()
            {
                continue;
            }
            let truncated = bytes.len() > PROJECT_INSTRUCTION_LIMIT as usize;
            bytes.truncate(PROJECT_INSTRUCTION_LIMIT as usize);
            text.push_str(&format!("\nProject instructions from {name}:\n"));
            text.push_str(&String::from_utf8_lossy(&bytes));
            if truncated {
                text.push_str("\n[Instructions truncated; read the rest with run_command.]\n");
            }
        }
        text
    }

    fn admitted(&self, call: &ModelToolCall) -> bool {
        let name = call.name.as_str();
        self.allow.iter().any(|a| a == "*" || a == name)
    }
}

/// Wraps a provider gateway with the tool loop. Text chunks are forwarded
/// with renumbered sequences so all rounds read as one consecutive stream;
/// the terminal response is always synthesized with the FULL accumulated
/// text and no tool calls, exactly what the app-server journal requires of
/// a Completed turn.
pub struct ToolLoopGateway {
    inner: Arc<dyn ModelStreamingGateway>,
    surface: Arc<ToolSurface>,
    context: RuntimeContext,
    max_rounds: usize,
}

impl ToolLoopGateway {
    pub fn new(
        inner: Arc<dyn ModelStreamingGateway>,
        surface: Arc<ToolSurface>,
        context: RuntimeContext,
        max_rounds: usize,
    ) -> Self {
        Self {
            inner,
            surface,
            context,
            max_rounds,
        }
    }
}

impl ModelStreamingGateway for ToolLoopGateway {
    fn stream(
        &self,
        request: ModelGatewayRequest,
    ) -> ModelGatewayFuture<ModelGatewayResult<ModelGatewayEventStream>> {
        let inner = self.inner.clone();
        let surface = self.surface.clone();
        let context = self.context.child_context();
        let max_rounds = self.max_rounds;
        Box::pin(async move {
            let (tx, mut rx) =
                tokio::sync::mpsc::channel::<ModelGatewayResult<ModelStreamEvent>>(64);
            // Drive the loop from the consumer's stream. Dropping it on Stop
            // drops provider I/O and managed shell processes immediately,
            // instead of leaving a detached task running until its next send.
            let output = async_stream::stream! {
                let work = run_turn(inner, surface, context, max_rounds, request, tx);
                tokio::pin!(work);
                loop {
                    tokio::select! {
                        item = rx.recv() => match item {
                            Some(item) => yield item,
                            None => break,
                        },
                        () = &mut work => {
                            while let Some(item) = rx.recv().await {
                                yield item;
                            }
                            break;
                        }
                    }
                }
            };
            Ok(Box::pin(output) as ModelGatewayEventStream)
        })
    }
}

/// Drives the provider/tool loop for one turn, sending renumbered events.
async fn run_turn(
    inner: Arc<dyn ModelStreamingGateway>,
    surface: Arc<ToolSurface>,
    context: RuntimeContext,
    max_rounds: usize,
    request: ModelGatewayRequest,
    tx: tokio::sync::mpsc::Sender<ModelGatewayResult<ModelStreamEvent>>,
) {
    let endpoint = request.endpoint().clone();
    let mut messages: Vec<ModelGatewayMessage> = request.messages().to_vec();
    messages.insert(
        0,
        plain_message(
            ModelGatewayMessageRole::System,
            surface.project_context().await,
        ),
    );
    let options = surface.offer_options.clone();
    let mut accumulated = String::new();
    let mut sequence: u64 = 0;
    let mut started = false;

    macro_rules! send {
        ($event:expr) => {
            if tx.send(Ok($event)).await.is_err() {
                return; // consumer hung up; stop the turn
            }
        };
    }

    for _round in 0..max_rounds {
        let round_start = accumulated.len();
        let req = ModelGatewayRequest::new(endpoint.clone(), messages.clone())
            .with_options(options.clone());
        let mut source = match inner.stream(req).await {
            Ok(source) => source,
            Err(error) => {
                let _ = tx.send(Err(error)).await;
                return;
            }
        };
        let mut tool_requested = false;
        while let Some(item) = source.next().await {
            match item {
                Ok(ModelStreamEvent::Started { transport }) => {
                    // Exactly one Started per turn: the session stream
                    // rejects a second one.
                    if !started {
                        started = true;
                        send!(ModelStreamEvent::Started { transport });
                    }
                }
                Ok(ModelStreamEvent::Chunk(chunk)) => {
                    sequence += 1;
                    accumulated.push_str(&chunk.content);
                    send!(ModelStreamEvent::Chunk(ModelStreamChunk::new(
                        sequence,
                        chunk.content
                    )));
                }
                Ok(ModelStreamEvent::Thinking(chunk)) => {
                    send!(ModelStreamEvent::Thinking(chunk));
                }
                Ok(ModelStreamEvent::Completed { response }) => {
                    // Text a terminal response may carry that was never
                    // streamed as chunks (non-stream transports) is flushed
                    // here first.
                    let content = response
                        .choices
                        .iter()
                        .find(|choice| choice.message.role == ModelGatewayMessageRole::Assistant)
                        .map(|choice| choice.message.content.as_str())
                        .unwrap_or("");
                    let streamed = &accumulated[round_start..];
                    if content.len() > streamed.len() && content.starts_with(streamed) {
                        let gap = content[streamed.len()..].to_owned();
                        sequence += 1;
                        accumulated.push_str(&gap);
                        send!(ModelStreamEvent::Chunk(ModelStreamChunk::new(
                            sequence, gap
                        )));
                    }
                    send!(synthesized(&accumulated));
                    return;
                }
                Ok(ModelStreamEvent::ToolRequested { response }) => {
                    let calls: Vec<ModelToolCall> = response
                        .choices
                        .iter()
                        .flat_map(|choice| choice.message.tool_calls.iter().cloned())
                        .collect();
                    if calls.is_empty() {
                        let _ = tx
                            .send(Err(ModelGatewayError::Completion(
                                "tool request carried no calls".into(),
                            )))
                            .await;
                        return;
                    }
                    // The assistant turn (with its tool calls) joins the
                    // conversation the results answer to.
                    if let Some(choice) = response
                        .choices
                        .iter()
                        .find(|choice| choice.message.role == ModelGatewayMessageRole::Assistant)
                    {
                        messages.push(choice.message.clone());
                    }
                    for call in &calls {
                        let result = evaluate(&surface, &context, call).await;
                        messages.push(result);
                    }
                    tool_requested = true;
                    break;
                }
                Err(error) => {
                    let _ = tx.send(Err(error)).await;
                    return;
                }
            }
        }
        if !tool_requested {
            let _ = tx
                .send(Err(ModelGatewayError::Completion(
                    "provider stream ended without a terminal response".into(),
                )))
                .await;
            return;
        }
    }
    let _ = tx
        .send(Err(ModelGatewayError::Completion(format!(
            "tool round limit ({max_rounds}) reached; task may be unfinished"
        ))))
        .await;
}

/// Hand-rolled plain message (the protocol's `plain` helper is private).
fn plain_message(role: ModelGatewayMessageRole, content: impl Into<String>) -> ModelGatewayMessage {
    ModelGatewayMessage {
        role,
        content: content.into(),
        name: None,
        tool_call_id: None,
        tool_calls: Vec::new(),
        continuation: None,
    }
}

/// Synthesizes the terminal response the journal requires: one assistant
/// choice whose content is the whole accumulated turn text, no tool calls.
fn synthesized(accumulated: &str) -> ModelStreamEvent {
    let message = plain_message(ModelGatewayMessageRole::Assistant, accumulated.to_owned());
    ModelStreamEvent::Completed {
        response: ModelGatewayCompletionResponse {
            id: String::new(),
            model: String::new(),
            choices: vec![ModelGatewayCompletionChoice::new(0, message, None)],
        },
    }
}

/// Runs one tool call under the host policy. Denial is reported back to the
/// model as a tool result so it can adapt instead of failing the turn.
async fn evaluate(
    surface: &ToolSurface,
    context: &RuntimeContext,
    call: &ModelToolCall,
) -> ModelGatewayMessage {
    let outcome = if !surface.admitted(call) {
        ModelToolOutcome::Failed(format!(
            "tool {} is denied by the host policy",
            call.name.as_str()
        ))
    } else {
        match surface.handlers.get(call.name.as_str()) {
            Some(handler) => {
                handler
                    .run_tool(call.clone(), context.child_context())
                    .await
            }
            None => ModelToolOutcome::Failed(format!("no handler for tool {}", call.name.as_str())),
        }
    };
    let text = match outcome {
        ModelToolOutcome::Completed(output) => output,
        ModelToolOutcome::Failed(error) => format!("tool failed: {error}"),
    };
    tool_gateway_message(call.id.clone(), text)
}

#[cfg(test)]
mod tests {
    use super::*;
    use marlin_agent_protocol::{ModelEndpoint, ModelGatewayTransport};
    use marlin_agent_runtime::{TokioAgentRuntime, TokioAgentRuntimeBuildRequest};
    use std::sync::atomic::{AtomicBool, Ordering};

    struct EmptyGateway;
    impl ModelStreamingGateway for EmptyGateway {
        fn stream(
            &self,
            _: ModelGatewayRequest,
        ) -> ModelGatewayFuture<ModelGatewayResult<ModelGatewayEventStream>> {
            Box::pin(async { Ok(Box::pin(tokio_stream::empty()) as ModelGatewayEventStream) })
        }
    }

    #[tokio::test]
    async fn eof_and_exhausted_budget_are_not_success() {
        for rounds in [0, 1] {
            let (runtime, _events) = TokioAgentRuntime::from_build_request(
                TokioAgentRuntimeBuildRequest::runtime_root(64),
            );
            let surface = Arc::new(ToolSurface::shell(std::env::temp_dir(), vec![]).unwrap());
            let gateway =
                ToolLoopGateway::new(Arc::new(EmptyGateway), surface, runtime.context(), rounds);
            let request = ModelGatewayRequest::new(
                ModelEndpoint::new("test", "model"),
                vec![plain_message(ModelGatewayMessageRole::User, "hi")],
            );
            let result: Vec<_> = gateway.stream(request).await.unwrap().collect().await;
            assert!(result.iter().any(Result::is_err), "must report failure");
            assert!(
                !result
                    .iter()
                    .any(|event| matches!(event, Ok(ModelStreamEvent::Completed { .. })))
            );
        }
    }

    struct PendingGateway(Arc<AtomicBool>);

    struct Scratch(PathBuf);
    impl Scratch {
        fn new() -> Self {
            static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
            let nanos = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let id = NEXT.fetch_add(1, Ordering::Relaxed);
            let path = std::env::temp_dir().join(format!(
                "marlin-context-{}-{nanos}-{id}",
                std::process::id()
            ));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Scratch {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    struct CaptureGateway(Arc<std::sync::Mutex<Vec<ModelGatewayMessage>>>);
    impl ModelStreamingGateway for CaptureGateway {
        fn stream(
            &self,
            request: ModelGatewayRequest,
        ) -> ModelGatewayFuture<ModelGatewayResult<ModelGatewayEventStream>> {
            *self.0.lock().unwrap() = request.messages().to_vec();
            Box::pin(async {
                Ok(Box::pin(tokio_stream::iter([Ok(synthesized("done"))]))
                    as ModelGatewayEventStream)
            })
        }
    }

    #[tokio::test]
    async fn project_instructions_reach_provider_and_refresh_each_turn() {
        let dir = Scratch::new();
        let (runtime, _events) =
            TokioAgentRuntime::from_build_request(TokioAgentRuntimeBuildRequest::runtime_root(64));
        let surface = Arc::new(ToolSurface::shell(dir.0.clone(), vec![]).unwrap());
        let captured = Arc::new(std::sync::Mutex::new(Vec::new()));
        let gateway = ToolLoopGateway::new(
            Arc::new(CaptureGateway(captured.clone())),
            surface,
            runtime.context(),
            2,
        );
        for instruction in ["Use TEST_COMMAND_A", "Use TEST_COMMAND_B"] {
            std::fs::write(dir.0.join("AGENTS.md"), instruction).unwrap();
            let request = ModelGatewayRequest::new(
                ModelEndpoint::new("test", "model"),
                vec![plain_message(ModelGatewayMessageRole::User, "inspect")],
            );
            let result: Vec<_> = gateway.stream(request).await.unwrap().collect().await;
            assert!(
                result
                    .iter()
                    .any(|r| matches!(r, Ok(ModelStreamEvent::Completed { .. })))
            );
            let messages = captured.lock().unwrap();
            assert_eq!(messages[0].role, ModelGatewayMessageRole::System);
            assert!(messages[0].content.contains(instruction));
            assert!(
                messages[0]
                    .content
                    .contains(dir.0.canonicalize().unwrap().to_str().unwrap())
            );
        }
    }

    #[tokio::test]
    async fn instruction_limits_and_external_symlinks() {
        let dir = Scratch::new();
        std::fs::write(
            dir.0.join("AGENTS.md"),
            "a".repeat(PROJECT_INSTRUCTION_LIMIT as usize + 1),
        )
        .unwrap();
        let surface = ToolSurface::shell(dir.0.clone(), vec![]).unwrap();
        let text = surface.project_context().await;
        assert!(text.contains("Instructions truncated"));
        assert!(text.len() < PROJECT_INSTRUCTION_LIMIT as usize + 2048);
        #[cfg(unix)]
        {
            let outside = Scratch::new();
            std::fs::write(outside.0.join("outside.md"), "EXTERNAL_INSTRUCTION").unwrap();
            std::os::unix::fs::symlink(outside.0.join("outside.md"), dir.0.join("CLAUDE.md"))
                .unwrap();
            assert!(
                !surface
                    .project_context()
                    .await
                    .contains("EXTERNAL_INSTRUCTION")
            );
        }
    }

    struct Dropped(Arc<AtomicBool>);
    impl Drop for Dropped {
        fn drop(&mut self) {
            self.0.store(true, Ordering::SeqCst);
        }
    }
    impl ModelStreamingGateway for PendingGateway {
        fn stream(
            &self,
            _: ModelGatewayRequest,
        ) -> ModelGatewayFuture<ModelGatewayResult<ModelGatewayEventStream>> {
            let flag = self.0.clone();
            Box::pin(async move {
                let source = async_stream::stream! {
                    let _guard = Dropped(flag);
                    yield Ok(ModelStreamEvent::Started { transport: ModelGatewayTransport::Sse });
                    std::future::pending::<()>().await;
                };
                Ok(Box::pin(source) as ModelGatewayEventStream)
            })
        }
    }

    #[tokio::test]
    async fn dropping_turn_drops_provider_without_waiting_for_another_token() {
        let dropped = Arc::new(AtomicBool::new(false));
        let (runtime, _events) =
            TokioAgentRuntime::from_build_request(TokioAgentRuntimeBuildRequest::runtime_root(64));
        let surface = Arc::new(ToolSurface::shell(std::env::temp_dir(), vec!["*".into()]).unwrap());
        let gateway = ToolLoopGateway::new(
            Arc::new(PendingGateway(dropped.clone())),
            surface,
            runtime.context(),
            8,
        );
        let request = ModelGatewayRequest::new(
            ModelEndpoint::new("test", "model"),
            vec![plain_message(ModelGatewayMessageRole::User, "hi")],
        );
        let mut stream = gateway.stream(request).await.unwrap();
        assert!(matches!(
            stream.next().await,
            Some(Ok(ModelStreamEvent::Started { .. }))
        ));
        assert!(!dropped.load(Ordering::SeqCst));
        drop(stream);
        assert!(dropped.load(Ordering::SeqCst));
    }
}
