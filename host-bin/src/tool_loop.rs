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

use marlin_agent_protocol::{
    ModelGatewayCompletionChoice, ModelGatewayCompletionOptions, ModelGatewayCompletionResponse,
    ModelGatewayError, ModelGatewayFuture, ModelGatewayMessage, ModelGatewayMessageRole,
    ModelGatewayRequest, ModelGatewayResult, ModelToolCall, ModelToolName,
    tool_gateway_message,
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
}

impl ToolSurface {
    /// Builds a single `run_command` shell tool rooted at `workspace`.
    /// Tools are opt-in: without `tools_workspace` the host offers none.
    pub fn shell(
        workspace: PathBuf,
        allow: Vec<String>,
    ) -> Result<Self, Box<dyn std::error::Error>> {
        let name = ModelToolName::new("run_command")
            .map_err(|e| format!("tool name: {e}"))?;
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
        let handler = Arc::new(ShellToolHandler::new(workspace, declaration));
        let mut handlers = HashMap::new();
        handlers.insert(definition.name.as_str().to_owned(), handler);
        Ok(Self {
            offer_options: ModelGatewayCompletionOptions {
                tools: Some(vec![definition]),
                ..ModelGatewayCompletionOptions::default()
            },
            handlers,
            allow,
        })
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
        Self { inner, surface, context, max_rounds }
    }
}

impl ModelStreamingGateway for ToolLoopGateway {
    fn stream(
        &self,
        request: ModelGatewayRequest,
    ) -> ModelGatewayFuture<ModelGatewayResult<ModelGatewayEventStream>> {
        let inner = self.inner.clone();
        let surface = self.surface.clone();
        let context = self.context.clone();
        let max_rounds = self.max_rounds;
        Box::pin(async move {
            let (tx, rx) = tokio::sync::mpsc::channel::<ModelGatewayResult<ModelStreamEvent>>(64);
            // The turn streams from its own task; when it finishes (or the
            // consumer hangs up and the loop notices a closed channel), the
            // senders drop and the receiver ends.
            tokio::spawn(run_turn(
                inner,
                surface,
                context,
                max_rounds,
                request,
                tx,
            ));
            Ok(Box::pin(tokio_stream::wrappers::ReceiverStream::new(rx))
                as ModelGatewayEventStream)
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
                    if content.len() > accumulated.len() && content.starts_with(accumulated.as_str())
                    {
                        let gap = content[accumulated.len()..].to_owned();
                        sequence += 1;
                        accumulated.push_str(&gap);
                        send!(ModelStreamEvent::Chunk(ModelStreamChunk::new(sequence, gap)));
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
                            .send(Err(ModelGatewayError::RequestNotDispatched(
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
            // The inner stream ended without a terminal event; report what
            // streamed so far as one complete turn.
            break;
        }
    }
    // Round budget exhausted (or a bare stream end): report what streamed so
    // far as one complete turn.
    let _ = tx.send(Ok(synthesized(&accumulated))).await;
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
        ModelToolOutcome::Failed(format!("tool {} is denied by the host policy", call.name.as_str()))
    } else {
        match surface.handlers.get(call.name.as_str()) {
            Some(handler) => handler.run_tool(call.clone(), context.child_context()).await,
            None => ModelToolOutcome::Failed(format!("no handler for tool {}", call.name.as_str())),
        }
    };
    let text = match outcome {
        ModelToolOutcome::Completed(output) => output,
        ModelToolOutcome::Failed(error) => format!("tool failed: {error}"),
    };
    tool_gateway_message(call.id.clone(), text)
}
