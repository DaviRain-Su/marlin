// marlin-host: the real Marlin App Server host binary.
//
// It embeds the upstream marlin-agent-app-server library and serves the
// marlin.app.v1 gRPC contract on loopback, replacing the development
// devhost (Go fakehost) in packaged builds. The CLI contract and token
// environment are the ones sidecar.Options documents (docs/design.md §5):
//
//   marlin-host --bind-addr 127.0.0.1:PORT --session-id gui-session
//               --config <path to flat TOML>
//
// The one-time token arrives through the MARLIN_HOST_AUTH_TOKEN
// environment; it is never in argv and never logged.

mod tool_loop;

use std::{env, path::PathBuf, sync::Arc, time::Duration};

use marlin_agent_app_server::{AppServer, SessionBinding};
use marlin_agent_protocol::ModelEndpoint;
use marlin_agent_runtime::{TokioAgentRuntime, TokioAgentRuntimeBuildRequest};
use marlin_agent_sessions::{AgentSessionContext, ContextVisibility};
use marlin_agent_storage::{
    AgentId, ProjectId, TursoAgentStorage, TursoAgentStorageConfig, TursoBatchTransactionMode,
    TursoOptimizationProfile,
};
use marlin_agent_stream::{ModelStreamOutputLimit, WeaverModelGateway};
use weaver_llm::{Authentication, Client, Endpoint, Protocol};

#[derive(Default, serde::Deserialize)]
struct FileConfig {
    provider: Option<String>,
    /// Wire protocol: "anthropic" | "openai". Defaults per provider.
    protocol: Option<String>,
    /// Auth scheme for Anthropic routes: "bearer" | "provider-default".
    /// Kimi's coding endpoint needs "bearer"; api.anthropic.com does not.
    auth: Option<String>,
    api_key: Option<String>,
    base_url: Option<String>,
    model: Option<String>,
    /// Path of the libsql session journal; defaults under the user's
    /// config directory.
    storage_path: Option<String>,
    /// Tool surface (opt-in). `tools_workspace` roots the shell tool;
    /// `tools_allow` is a comma-separated tool-name list ("*" = all).
    tools_workspace: Option<String>,
    tools_allow: Option<String>,
    /// Provider round trips the tool loop may take per turn (default 8).
    tools_max_rounds: Option<usize>,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut bind_addr = "127.0.0.1:0".to_owned();
    let mut session_id = String::new();
    let mut config_path: Option<PathBuf> = None;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--bind-addr" => bind_addr = args.next().ok_or("--bind-addr needs a value")?,
            "--session-id" => session_id = args.next().ok_or("--session-id needs a value")?,
            "--config" => config_path = Some(PathBuf::from(args.next().ok_or("--config needs a value")?)),
            other => return Err(format!("unknown argument {other:?}").into()),
        }
    }
    if env::var("MARLIN_HOST_AUTH_TOKEN").is_err() {
        return Err("MARLIN_HOST_AUTH_TOKEN is required (the sidecar sets it)".into());
    }

    let cfg: FileConfig = match &config_path {
        Some(path) => toml_cfg(path)?,
        None => FileConfig::default(),
    };

    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build()?;
    rt.block_on(serve(bind_addr, session_id, cfg, config_path))
}

fn toml_cfg(path: &PathBuf) -> Result<FileConfig, Box<dyn std::error::Error>> {
    let raw = std::fs::read_to_string(path)?;
    // Unknown keys are ignored: the config grows before this reader does.
    Ok(toml::from_str(&raw)?)
}

async fn serve(bind_addr: String, session_flag: String, cfg: FileConfig, config_path: Option<PathBuf>) -> Result<(), Box<dyn std::error::Error>> {
    let provider = cfg.provider.clone().unwrap_or_else(|| "kimi".into());
    let protocol = match cfg.protocol.as_deref().unwrap_or(match provider.as_str() {
        "deepseek" => "openai",
        _ => "anthropic",
    }) {
        "openai" => Protocol::OpenAi,
        "openai-responses" => Protocol::OpenAiResponses,
        _ => Protocol::Anthropic,
    };

    // Credential: config api_key wins, else the provider's env names — the
    // same names devhost documents.
    let credential = cfg.api_key.clone().unwrap_or_else(|| {
        let names: &[&str] = match provider.as_str() {
            "deepseek" => &["DEEPSEEK_API_KEY"],
            _ => &["KIMI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"],
        };
        names.iter().find_map(|n| env::var(n).ok()).unwrap_or_default()
    });
    if credential.trim().is_empty() {
        // Fail fast instead of producing REMOTE_UNCERTAIN turns: the App
        // banner shows this through the sidecar's error state.
        return Err(format!(
            "no provider credential: set api_key in {} or export {}",
            config_path.as_ref().map(|p| p.display().to_string()).unwrap_or_else(|| "app-sidecar.toml".into()),
            match provider.as_str() {
                "deepseek" => "DEEPSEEK_API_KEY",
                _ => "KIMI_API_KEY",
            }
        )
        .into());
    }

    // Endpoint URL: config base_url, else per-provider default (the kimi
    // default honors ANTHROPIC_BASE_URL like devhost does).
    let base_url = cfg.base_url.clone().unwrap_or_else(|| match provider.as_str() {
        "deepseek" => "https://api.deepseek.com".into(),
        _ => env::var("ANTHROPIC_BASE_URL").unwrap_or_else(|_| "https://api.kimi.com/coding/".into()),
    });

    // Model: config model, else per-provider default (kimi honors the
    // ANTHROPIC_MODEL family like devhost does).
    let model = cfg.model.clone().unwrap_or_else(|| match provider.as_str() {
        "deepseek" => "deepseek-chat".into(),
        _ => ["KIMI_MODEL", "ANTHROPIC_MODEL", "CLAUDE_CODE_MODEL"]
            .iter()
            .find_map(|n| env::var(n).ok())
            .unwrap_or_else(|| "k3".into()),
    });

    let client = Client::new(Duration::from_secs(120), 8 * 1024 * 1024)?;
    let resolved = weaver_llm::resolve_direct_target(protocol, Some(&base_url), Some(&model), |name| env::var(name).ok())
        .map_err(|e| format!("resolve endpoint {base_url} {model}: {e:?}"))?;
    let model_endpoint = ModelEndpoint::new(provider.clone(), resolved.1);
    model_endpoint.validate_contract()?;
    // The Endpoint URL is the COMPLETE request target (weaver POSTs it
    // verbatim); resolve_direct_target only normalizes env-derived URLs,
    // so a configured base_url is expanded here — the same rules
    // weaver_llm applies and the same paths devhost uses.
    let full_url = match protocol {
        Protocol::Anthropic => {
            let base = base_url.trim_end_matches('/');
            if base.ends_with("/v1/messages") {
                base.to_owned()
            } else if base.ends_with("/v1") {
                format!("{base}/messages")
            } else {
                format!("{base}/v1/messages")
            }
        }
        Protocol::OpenAiResponses => format!("{}/responses", base_url.trim_end_matches('/')),
        _ => {
            let base = base_url.trim_end_matches('/');
            if base.ends_with("/chat/completions") {
                base.to_owned()
            } else {
                format!("{base}/chat/completions")
            }
        }
    };
    // Anthropic routes default to x-api-key (right for api.anthropic.com);
    // proxy-style coding endpoints (Kimi) authenticate with Bearer.
    let authentication = match cfg.auth.as_deref() {
        Some("bearer") => Authentication::Bearer,
        Some("provider-default") | None => {
            if protocol == Protocol::Anthropic && provider.eq_ignore_ascii_case("kimi") {
                Authentication::Bearer
            } else {
                Authentication::ProviderDefault
            }
        }
        Some(other) => return Err(format!("unknown auth scheme {other:?}").into()),
    };
    let endpoint = Endpoint::new(protocol, &full_url)?.with_authentication(authentication);

    let weaver: Arc<dyn marlin_agent_stream::ModelStreamingGateway> = Arc::new(
        WeaverModelGateway::new(client, provider.clone().into(), endpoint, credential)
            .with_stream_output_limit(ModelStreamOutputLimit::new(
                std::num::NonZeroUsize::new(64 * 1024).expect("positive limit"),
            )),
    );

    // Session journal: one libsql file, next to the App's other state.
    let storage_path = cfg.storage_path.map(PathBuf::from).unwrap_or_else(default_storage_path);
    if let Some(dir) = storage_path.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    let storage = TursoAgentStorage::open_local(TursoAgentStorageConfig {
        path: storage_path,
        optimization_profile: TursoOptimizationProfile::AsyncIoOnlyCompatibility,
        batch_transaction_mode: TursoBatchTransactionMode::Immediate,
    })
    .await?;

    // --session-id > config (already merged into session_flag by the caller
    // defaults) > gui-session, mirroring devhost.
    let session_id = if session_flag.is_empty() { "gui-session".to_owned() } else { session_flag };
    let session = AgentSessionContext::root(
        marlin_agent_sessions::SessionId::new(session_id.clone()),
        ContextVisibility::default_runtime(),
    );
    let (runtime, _events) = TokioAgentRuntime::from_build_request(
        TokioAgentRuntimeBuildRequest::runtime_root(64).with_session(session),
    );

    // Tool surface (opt-in): with tools_workspace the host closes the tool
    // loop itself; the wire still carries pure text turns.
    let gateway: Arc<dyn marlin_agent_stream::ModelStreamingGateway> = match &cfg.tools_workspace {
        Some(workspace) if !workspace.trim().is_empty() => {
            let allow = cfg.tools_allow.clone().unwrap_or_else(|| "*".into());
            let allow: Vec<String> = allow.split(',').map(|s| s.trim().to_owned()).filter(|s| !s.is_empty()).collect();
            let surface = Arc::new(tool_loop::ToolSurface::shell(PathBuf::from(workspace), allow)?);
            let max_rounds = cfg.tools_max_rounds.unwrap_or(8).max(1);
            println!(
                "tool surface: run_command at {workspace} (allow {:?}, max {max_rounds} rounds/turn)",
                surface.allow,
            );
            Arc::new(tool_loop::ToolLoopGateway::new(
                weaver,
                surface,
                runtime.context(),
                max_rounds,
            ))
        }
        _ => weaver,
    };

    let binding = SessionBinding::new(storage, ProjectId::new("desktop")?, AgentId::new("app")?, runtime)?;

    let token = env::var("MARLIN_HOST_AUTH_TOKEN")?;
    let service = AppServer::new(
        model_endpoint,
        gateway,
        move |metadata| {
            let expected = format!("Bearer {token}");
            match metadata.get("authorization") {
                Some(value) if value.to_str().is_ok_and(|v| v == expected) => Ok(()),
                _ => Err(tonic::Status::unauthenticated("missing or invalid bearer token")),
            }
        },
    )?
    .with_session(binding)
    .into_service();

    println!("host ready addr={bind_addr}");
    tonic::transport::Server::builder().add_service(service).serve(bind_addr.parse()?).await?;
    Ok(())
}

/// Same location the App already uses for its projection file:
/// <config>/marlin-desktop/sessions.db (design §6.6 keeps them apart, but
/// one directory keeps backup simple).
fn default_storage_path() -> PathBuf {
    if let Some(home) = env::var_os("HOME") {
        let mut p = PathBuf::from(home);
        if cfg!(target_os = "macos") {
            p.push("Library/Application Support/marlin-desktop");
        } else {
            p.push(".config/marlin-desktop");
        }
        p.push("sessions.db");
        return p;
    }
    PathBuf::from("sessions.db")
}
