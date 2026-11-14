import { Channel } from "mygo-runtime";
import { marked } from "marked";
import DOMPurify from "dompurify";
import hljs from "highlight.js/lib/common";
import { ChatService, SettingsService, events, type StoredTurn, type TurnProgress } from "./mygo";

// Model output is untrusted content rendered inside the page that holds the
// Go IPC bridge — sanitize aggressively before it becomes HTML. Code blocks
// get a language label, highlighting and a per-block copy action.
marked.setOptions({ async: false, gfm: true, breaks: true });
marked.use({
  renderer: {
    code({ text, lang }: { text: string; lang?: string }) {
      const language = (lang || "").trim().split(/\s+/)[0] || "";
      let body: string;
      try {
        body = language && hljs.getLanguage(language)
          ? hljs.highlight(text, { language }).value
          : hljs.highlightAuto(text).value;
      } catch {
        body = text.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]!));
      }
      const label = language || "text";
      return (
        `<div class="codeblock"><div class="codeblock-head"><span>${label}</span>` +
        `<button type="button" class="code-copy">复制</button></div>` +
        `<pre><code class="hljs">${body}</code></pre></div>`
      );
    },
  },
});
function renderMarkdown(text: string): string {
  return DOMPurify.sanitize(marked.parse(text) as string, {
    FORBID_TAGS: ["style", "form", "iframe"],
    FORBID_ATTR: ["onerror", "onclick", "onload"],
    ADD_ATTR: ["target"],
  });
}

const $ = <T extends HTMLElement>(selector: string) => document.querySelector<T>(selector)!;

const thread = $("#thread");
const messages = $("#messages");
const emptyState = $("#empty");
const draft = $<HTMLTextAreaElement>("#draft");
const sendBtn = $<HTMLButtonElement>("#send");
const stopBtn = $<HTMLButtonElement>("#stop");
const banner = $("#banner");
const sessionTitle = $("#session-title");
const sessionList = $("#session-list");
const composerWrap = $("#composer-wrap");

// Sidebar status + chips.
const statusDot = $("#status-dot");
const statusText = $("#status-text");
const chipProvider = $<HTMLButtonElement>("#chip-provider");
const chipTools = $<HTMLButtonElement>("#chip-tools");
const composerChip = $<HTMLButtonElement>("#composer-chip");

// Settings modal elements.
const settingsOverlay = $("#settings-overlay");
const setProvider = $<HTMLSelectElement>("#set-provider");
const setModel = $<HTMLInputElement>("#set-model");
const setKey = $<HTMLInputElement>("#set-key");
const setBaseUrl = $<HTMLInputElement>("#set-base-url");
const setToolsWorkspace = $<HTMLInputElement>("#set-tools-workspace");
const setToolsAllow = $<HTMLInputElement>("#set-tools-allow");
const settingsHint = $("#settings-hint");
const settingsPath = $("#settings-path");

let busy = false;
let ready = false;
/** Current session id as the frontend sees it (drives the list highlight). */
let currentSessionId = "";

const STATUS_LABEL: Record<string, string> = {
  ready: "已连接",
  starting: "正在启动 host…",
  reconnecting: "正在重连…",
  error: "连接异常",
  unresolved: "会话待恢复",
};

function setComposer() {
  sendBtn.disabled = busy || !ready;
  draft.disabled = busy || !ready;
  stopBtn.classList.toggle("hidden", !busy);
  autoGrow();
}

function showBanner(state: string, message?: string) {
  const label = message || state;
  ready = state === "ready";
  banner.classList.toggle("hidden", ready);
  banner.dataset.state = state;
  banner.textContent = label;
  // The sidebar dot mirrors the connection state.
  statusDot.dataset.state = state;
  statusText.textContent = STATUS_LABEL[state] ?? label;
  setComposer();
}

// Reconnect / unresolved banners arrive as events; the initial pull covers
// app startup. A fresh "ready" re-renders from the persisted projection
// (host restarts must not blank the view).
events.status.on((s) => {
  showBanner(s.state, s.message);
  if (s.state === "ready") void refreshAll();
});

// ---- Sidebar: conversation list, chips, connection state ----

async function refreshSessions() {
  try {
    const items = await ChatService.sessions();
    sessionList.replaceChildren(
      ...items.map((it) => {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = `session-item${it.id === currentSessionId ? " active" : ""}`;
        btn.textContent = it.title || "新对话";
        btn.title = it.title || it.id;
        btn.addEventListener("click", () => void switchSession(it.id));
        return btn;
      }),
    );
    const current = items.find((it) => it.id === currentSessionId);
    if (current) sessionTitle.textContent = current.title || "新对话";
  } catch (err) {
    console.error("sessions refresh failed", err);
  }
}

async function refreshChips() {
  try {
    const snap = await SettingsService.state();
    const model = snap.model || (snap.provider === "deepseek" ? "deepseek-chat" : "k3");
    chipProvider.textContent = `${snap.provider || "kimi"} · ${model}`;
    composerChip.textContent = `${snap.provider || "kimi"} · ${model}`;
    chipTools.textContent = snap.toolsWorkspace
      ? `工具：${snap.toolsWorkspace.split("/").pop() || snap.toolsWorkspace}`
      : "工具：未启用";
  } catch {
    chipProvider.textContent = "—";
  }
}

/** Keeps list highlight, title and pristine layout in step. */
function updatePristine() {
  document.body.classList.toggle("pristine", messages.children.length === 0);
}

async function refreshAll() {
  const snap = await ChatService.state();
  currentSessionId = snap.sessionId || currentSessionId;
  await Promise.all([renderHistory(), refreshSessions(), refreshChips()]);
  return snap;
}

async function switchSession(id: string) {
  if (busy || id === currentSessionId) return;
  try {
    await ChatService.switchSession(id);
    currentSessionId = id;
    await Promise.all([renderHistory(), refreshSessions()]);
  } catch (err) {
    showBanner("error", err instanceof Error ? err.message : String(err));
  }
}

$("#new-chat").addEventListener("click", () => {
  void (async () => {
    if (busy) return;
    const meta = await ChatService.newSession();
    currentSessionId = meta.id;
    await Promise.all([renderHistory(), refreshSessions()]);
    draft.focus();
  })().catch((err) => showBanner("error", err instanceof Error ? err.message : String(err)));
});

// Sidebar collapse, persisted like Claude's.
{
  const side = $("#sidebar");
  const hidden = localStorage.getItem("marlin.side-hidden") === "1";
  side.classList.toggle("side-hidden", hidden);
  $("#side-toggle").addEventListener("click", () => {
    const nowHidden = side.classList.toggle("side-hidden");
    localStorage.setItem("marlin.side-hidden", nowHidden ? "1" : "0");
  });
}

// ---- Message rows ----

interface MessageRow {
  root: HTMLElement;
  text: HTMLElement;
  /** Raw accumulated markdown for assistant rows; "" for user rows. */
  raw: string;
  userText: string;
}

function addUserRow(text: string): MessageRow {
  const root = document.createElement("div");
  root.className = "msg user";
  const body = document.createElement("div");
  body.className = "msg-text";
  body.style.whiteSpace = "pre-wrap";
  body.textContent = text;
  root.appendChild(body);
  messages.appendChild(root);
  updatePristine();
  scrollToBottom();
  return { root, text: body, raw: "", userText: text };
}

function addAssistantRow(): MessageRow {
  const root = document.createElement("div");
  root.className = "msg assistant";
  const body = document.createElement("div");
  body.className = "msg-text markdown";
  root.appendChild(body);
  messages.appendChild(root);
  updatePristine();
  scrollToBottom();
  return { root, text: body, raw: "", userText: "" };
}

/** Re-renders an assistant row's accumulated markdown (per streaming chunk). */
function appendMarkdown(row: MessageRow, delta: string) {
  row.raw += delta;
  row.text.innerHTML = renderMarkdown(row.raw);
}

/** Hover actions under an assistant row: copy its text, retry the turn. */
function addActions(row: MessageRow) {
  const actions = document.createElement("div");
  actions.className = "msg-actions";
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "action-btn";
  copy.textContent = "复制";
  copy.addEventListener("click", async () => {
    await navigator.clipboard.writeText(row.raw || row.text.textContent || "");
    copy.textContent = "已复制";
    setTimeout(() => (copy.textContent = "复制"), 1200);
  });
  actions.appendChild(copy);
  if (row.userText) {
    const retry = document.createElement("button");
    retry.type = "button";
    retry.className = "action-btn";
    retry.textContent = "重试";
    retry.addEventListener("click", () => void sendText(row.userText));
    actions.appendChild(retry);
  }
  row.root.appendChild(actions);
}

function setOutcome(row: HTMLElement, outcome: string) {
  const pill = document.createElement("div");
  pill.className = `pill ${outcome}`;
  pill.textContent =
    outcome === "COMPLETED"
      ? "已完成"
      : outcome === "REMOTE_UNCERTAIN"
        ? "已停止（远端结果未知）"
        : outcome === "REJECTED_BEFORE_DISPATCH"
          ? "发送前被拒绝"
          : outcome.toLowerCase();
  row.appendChild(pill);
}

function setError(row: HTMLElement, err: unknown) {
  const pill = document.createElement("div");
  pill.className = "pill error";
  pill.textContent = `发送失败：${err instanceof Error ? err.message : String(err)}`;
  row.appendChild(pill);
}

function scrollToBottom() {
  thread.scrollTop = thread.scrollHeight;
}

function renderStoredTurn(row: StoredTurn) {
  addUserRow(row.userText);
  const assistant = addAssistantRow();
  if (row.assistantText) appendMarkdown(assistant, row.assistantText);
  if (row.outcome) setOutcome(assistant.root, row.outcome);
  if (row.assistantText) {
    assistant.userText = row.userText;
    addActions(assistant);
  }
}

async function renderHistory() {
  const snap = await ChatService.state();
  currentSessionId = snap.sessionId || currentSessionId;
  messages.replaceChildren();
  for (const row of snap.history ?? []) renderStoredTurn(row);
  updatePristine();
  scrollToBottom();
  if (snap.pendingText) await resumePending(snap.pendingText);
}

// resumePending re-admits the submission whose response was lost: the same
// id + original after_turn resolve to the original turn by idempotency.
async function resumePending(pendingText: string) {
  if (busy) return;
  busy = true;
  setComposer();
  addUserRow(pendingText);
  const assistant = addAssistantRow();
  const think = thinkingPill(assistant);
  try {
    await streamInto(assistant, (ch) => ChatService.resume(ch), () => think.remove());
    addActions(assistant);
  } catch (err) {
    think.remove();
    setError(assistant.root, err);
  } finally {
    busy = false;
    setComposer();
  }
}

/** The "thinking" indicator shown between admission and the first token. */
function thinkingPill(row: MessageRow): HTMLElement {
  const pill = document.createElement("div");
  pill.className = "pill thinking";
  pill.textContent = "正在思考…";
  row.root.appendChild(pill);
  scrollToBottom();
  return pill;
}

// streamInto drives one channel to completion against a row.
async function streamInto(
  assistant: MessageRow,
  start: (ch: Channel<TurnProgress>) => Promise<void>,
  onFirst?: () => void,
) {
  const ch = new Channel<TurnProgress>();
  const pendingPromise = start(ch);
  let first = true;
  for await (const ev of ch) {
    if (first) {
      first = false;
      onFirst?.();
    }
    if (ev.kind === "text" && typeof ev.text === "string") {
      appendMarkdown(assistant, ev.text);
      scrollToBottom();
    } else if (ev.kind === "outcome" && ev.outcome) {
      setOutcome(assistant.root, ev.outcome);
    }
  }
  await pendingPromise;
}

async function sendText(text: string) {
  if (busy || !ready || !text.trim()) return;
  busy = true;
  setComposer();
  addUserRow(text);
  const assistant = addAssistantRow();
  const think = thinkingPill(assistant);
  scrollToBottom();

  try {
    await streamInto(assistant, (ch) => ChatService.send(text, ch), () => think.remove());
    assistant.userText = text;
    addActions(assistant);
    await refreshSessions(); // the new turn may have renamed the session
  } catch (err) {
    think.remove();
    setError(assistant.root, err);
    draft.value = text; // the draft comes back: the host never admitted it
    autoGrow();
  } finally {
    busy = false;
    setComposer();
  }
}

$("#composer").addEventListener("submit", (e) => {
  e.preventDefault();
  if (busy || !ready || !draft.value.trim()) return;
  const text = draft.value;
  draft.value = "";
  autoGrow();
  void sendText(text);
});

// Enter sends; Shift+Enter makes a newline (like the composer it mirrors).
draft.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !draft.disabled) {
    e.preventDefault();
    $("#composer").dispatchEvent(new Event("submit", { cancelable: true }));
  }
});

function autoGrow() {
  draft.style.height = "auto";
  draft.style.height = `${Math.min(draft.scrollHeight, window.innerHeight * 0.4)}px`;
}
draft.addEventListener("input", autoGrow);

stopBtn.addEventListener("click", () => {
  void ChatService.stop().catch((err) => {
    showBanner("error", `stop failed: ${err instanceof Error ? err.message : String(err)}`);
  });
});

// Per-code-block copy buttons (delegated: rows re-render while streaming).
messages.addEventListener("click", (e) => {
  const btn = (e.target as HTMLElement).closest(".code-copy");
  if (!btn) return;
  const code = btn.closest(".codeblock")?.querySelector("code");
  if (!code) return;
  void navigator.clipboard.writeText(code.textContent ?? "").then(() => {
    btn.textContent = "已复制";
    setTimeout(() => (btn.textContent = "复制"), 1200);
  });
});

// ---- Empty-state idea prompts ----

for (const idea of document.querySelectorAll<HTMLButtonElement>(".idea")) {
  idea.addEventListener("click", () => void sendText(idea.dataset.text ?? ""));
}

// ---- Settings modal ----

function openSettings() {
  settingsOverlay.classList.remove("hidden");
  settingsHint.textContent = "加载中…";
  void (async () => {
    const snap = await SettingsService.state();
    setProvider.value = snap.provider || "kimi";
    setModel.value = snap.model || "";
    setBaseUrl.value = snap.baseUrl || "";
    setToolsWorkspace.value = snap.toolsWorkspace || "";
    setToolsAllow.value = snap.toolsAllow || "";
    // The stored credential is never sent to the page; an empty field
    // keeps it.
    setKey.value = "";
    setKey.placeholder = snap.keyConfigured ? "已配置（输入新值以更换）" : "未配置";
    settingsPath.textContent = `配置文件：${snap.configPath}`;
    settingsHint.textContent = snap.hostRunning ? "" : "host 未运行，保存后将于下次启动生效";
  })().catch((err) => {
    settingsHint.textContent = `读取失败：${err instanceof Error ? err.message : String(err)}`;
  });
}

function closeSettings() {
  settingsOverlay.classList.add("hidden");
}

function settingsInput() {
  return {
    provider: setProvider.value,
    model: setModel.value.trim(),
    baseUrl: setBaseUrl.value.trim(),
    apiKey: setKey.value,
    toolsWorkspace: setToolsWorkspace.value.trim(),
    toolsAllow: setToolsAllow.value.trim(),
  };
}

$("#settings-toggle").addEventListener("click", openSettings);
$("#settings-close").addEventListener("click", closeSettings);
settingsOverlay.addEventListener("click", (e) => {
  if (e.target === settingsOverlay) closeSettings();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeSettings();
});

// The provider/tool chips open the same modal.
for (const el of [chipProvider, chipTools, composerChip]) {
  el.addEventListener("click", openSettings);
}

$("#settings-save").addEventListener("click", () => {
  void SettingsService.save(settingsInput())
    .then(() => {
      settingsHint.textContent = "已保存。host 重启后生效（点「保存并重启 host」立即生效）。";
      return refreshChips();
    })
    .catch((err) => {
      settingsHint.textContent = `保存失败：${err instanceof Error ? err.message : String(err)}`;
    });
});

$("#settings-apply").addEventListener("click", () => {
  void (async () => {
    settingsHint.textContent = "保存并重启 host…";
    await SettingsService.save(settingsInput());
    await SettingsService.apply();
    settingsHint.textContent = "已保存，host 正在用新配置重启。";
    await refreshChips();
  })().catch((err) => {
    settingsHint.textContent = `失败：${err instanceof Error ? err.message : String(err)}`;
  });
});

// Time-based greeting on the empty state.
{
  const h = new Date().getHours();
  $("#greeting").textContent =
    h < 6 ? "夜深了" : h < 12 ? "早上好" : h < 18 ? "下午好" : "晚上好";
}

// Startup: render the persisted projection, then let the snapshot decide
// whether a pending submission needs resuming.
{
  const snap = await refreshAll();
  if (snap.ready) showBanner("ready");
  else showBanner("starting", "waiting for the Marlin host…");
}
setComposer();
