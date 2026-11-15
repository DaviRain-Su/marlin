import { Channel } from "mygo-runtime";
import { marked } from "marked";
import DOMPurify from "dompurify";
import hljs from "highlight.js/lib/common";
import { ChatService, SettingsService, ProjectService, events, type StoredTurn, type TurnProgress } from "./mygo";

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
const queueNote = $("#queue-note");
const unresolvedBar = $("#unresolved-bar");

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
let switching = false;
let stopping = false;
/** Current session id as the frontend sees it (drives the list highlight). */
let currentSessionId = "";
/** Messages typed while a turn streams; auto-sent when the turn ends. */
const queues = new Map<string, string[]>();
const pausedQueues = new Set<string>();
function queue() {
  let items = queues.get(currentSessionId);
  if (!items) queues.set(currentSessionId, (items = []));
  return items;
}
/** Per-session draft memory (switching sessions keeps what you typed). */
const drafts = new Map<string, string>();

const STATUS_LABEL: Record<string, string> = {
  ready: "已连接",
  starting: "正在启动 host…",
  reconnecting: "正在重连…",
  error: "连接异常",
  unresolved: "会话待恢复",
};

function setComposer() {
  sendBtn.disabled = !ready || switching;
  // Typing stays available while a turn streams: messages queue up.
  draft.disabled = !ready || switching;
  stopBtn.classList.toggle("hidden", !busy);
  stopBtn.disabled = stopping;
  $("#new-chat").toggleAttribute("disabled", busy || switching);
  $("#project-open").toggleAttribute("disabled", busy || switching);
  $("#project-choose").toggleAttribute("disabled", busy || switching);
  $("#settings-apply").toggleAttribute("disabled", busy || switching);
  for (const btn of sessionList.querySelectorAll<HTMLButtonElement>("button")) {
    btn.disabled = busy || switching;
  }
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
  if (s.state === "ready" && !busy && !switching) void refreshAll();
});

function showActionError(err: unknown) {
  banner.classList.remove("hidden");
  banner.dataset.state = "error";
  banner.textContent = err instanceof Error ? err.message : String(err);
}

// ---- Sidebar: conversation list, chips, connection state ----

async function refreshSessions() {
  try {
    const items = await ChatService.sessions();
    sessionList.replaceChildren(
      ...items.map((it) => {
        const row = document.createElement("div");
        row.className = `session-row${it.id === currentSessionId ? " active" : ""}`;
        const btn = document.createElement("button");
        btn.type = "button";
        btn.className = "session-item";
        btn.textContent = it.title || "新对话";
        btn.title = it.title || it.id;
        btn.addEventListener("click", () => void switchSession(it.id));
        row.appendChild(btn);
        // Hover actions: delete any session except the open one.
        if (it.id !== currentSessionId) {
          const del = document.createElement("button");
          del.type = "button";
          del.className = "session-del";
          del.title = "删除会话";
          del.textContent = "✕";
          del.addEventListener("click", (e) => {
            e.stopPropagation();
            if (del.dataset.confirm !== "1") {
              del.dataset.confirm = "1";
              del.textContent = "确认?";
              setTimeout(() => {
                del.dataset.confirm = "";
                del.textContent = "✕";
              }, 10000);
              return;
            }
            void ChatService.deleteSession(it.id)
              .then(refreshSessions)
              .catch(showActionError);
          });
          row.appendChild(del);
        }
        return row;
      }),
    );
    const current = items.find((it) => it.id === currentSessionId);
    if (current && document.activeElement !== renameInput) {
      sessionTitle.textContent = current.title || "新对话";
    }
    setComposer();
  } catch (err) {
    console.error("sessions refresh failed", err);
  }
}

// ---- Rename: click the top-bar title, edit inline, Enter/blur commits ----

const renameInput = document.createElement("input");
renameInput.className = "session-title rename hidden";
renameInput.maxLength = 80;
let renamingSessionId = "";

function startRename() {
  if (document.body.classList.contains("pristine")) return;
  renameInput.value = sessionTitle.textContent || "";
  renamingSessionId = currentSessionId;
  sessionTitle.classList.add("hidden");
  renameInput.classList.remove("hidden");
  renameInput.focus();
  renameInput.select();
}

function commitRename() {
  if (!renamingSessionId) return;
  const id = renamingSessionId;
  renamingSessionId = "";
  const title = renameInput.value.trim();
  renameInput.classList.add("hidden");
  sessionTitle.classList.remove("hidden");
  if (!title || title === sessionTitle.textContent) return;
  void ChatService.renameSession(id, title)
    .then(refreshSessions)
    .catch(showActionError);
}

renameInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    commitRename();
  }
  if (e.key === "Escape") {
    renamingSessionId = "";
    renameInput.classList.add("hidden");
    sessionTitle.classList.remove("hidden");
  }
});
renameInput.addEventListener("blur", commitRename);
sessionTitle.addEventListener("click", startRename);
sessionTitle.parentElement!.appendChild(renameInput);

async function refreshChips() {
  try {
    const snap = await SettingsService.state();
    const model = snap.model || (snap.provider === "deepseek" ? "deepseek-chat" : "k3");
    chipProvider.textContent = `${snap.provider || "kimi"} · ${model}`;
    composerChip.textContent = `${snap.provider || "kimi"} · ${model}`;
    await refreshProject();
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
  if (busy || switching || id === currentSessionId) return;
  switching = true;
  setComposer();
  try {
    drafts.set(currentSessionId, draft.value);
    await ChatService.switchSession(id);
    currentSessionId = id;
    draft.value = drafts.get(id) ?? "";
    autoGrow();
    await Promise.all([renderHistory(), refreshSessions()]);
  } catch (err) {
    showActionError(err);
  } finally {
    switching = false;
    renderQueue();
    setComposer();
  }
}

async function newSession(continueHistory = false) {
  if (busy || switching) return;
  switching = true;
  setComposer();
  try {
    drafts.set(currentSessionId, draft.value);
    const meta = continueHistory ? await ChatService.continueSession() : await ChatService.newSession();
    if (continueHistory) {
      drafts.set(meta.id, draft.value);
      queues.set(meta.id, queue());
      queues.delete(currentSessionId);
      if (pausedQueues.delete(currentSessionId)) pausedQueues.add(meta.id);
    }
    currentSessionId = meta.id;
    draft.value = drafts.get(meta.id) ?? "";
    autoGrow();
    await Promise.all([renderHistory(), refreshSessions()]);
  } catch (err) {
    showActionError(err);
  } finally {
    switching = false;
    renderQueue();
    setComposer();
    draft.focus();
  }
}
$("#new-chat").addEventListener("click", () => void newSession());

// ---- One active local project: native selection and read-only review ----

const projectOverlay = $("#project-overlay");
const projectPreview = $("#project-preview-text");
let projectRead = 0;
let projectRefresh = 0;
let projectRoot = "";

async function refreshProject() {
  const revision = ++projectRefresh;
  const snap = await ProjectService.state();
  if (revision !== projectRefresh) return;
  if (projectRoot !== snap.root) {
    ++projectRead;
    projectPreview.textContent = "";
    projectRoot = snap.root;
  }
  chipTools.textContent = snap.root ? `项目：${snap.name}` : "项目：未打开";
  chipTools.title = snap.root || "打开项目以启用编码";
  $("#project-review").textContent = snap.root ? `项目：${snap.name}` : "项目与变更";
  $("#project-review").title = snap.root || "查看项目文件与 Git 变更";
  $("#project-title").textContent = snap.name || "项目与变更";
  $("#project-root").textContent = snap.root || "打开本地目录后，可以让 Agent 读改文件、运行测试。";
  $("#project-summary").textContent = snap.root
    ? `${snap.branch ? `分支：${snap.branch}` : "普通目录（未启用 Git）"} · ${snap.files.length} 个文件${snap.truncated ? "（列表或状态已截断）" : ""}`
    : "";
  $("#project-diff").toggleAttribute("disabled", !snap.branch);
  const status = document.createElement("pre");
  status.className = "hint";
  status.textContent = snap.status || (snap.branch ? "工作区干净" : "");
  const files = snap.files.map((path) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "project-file";
    btn.textContent = path;
    btn.title = path;
    btn.addEventListener("click", () => void previewProjectFile(path));
    return btn;
  });
  $("#project-files").replaceChildren(status, ...files);
}

async function previewProjectFile(path: string) {
  const revision = ++projectRead;
  $("#project-preview-title").textContent = path;
  projectPreview.textContent = "加载中…";
  try {
    const text = await ProjectService.readFile(path);
    if (revision === projectRead) projectPreview.textContent = text;
  } catch (err) {
    if (revision === projectRead) projectPreview.textContent = String(err);
  }
}

async function showProjectDiff() {
  const revision = ++projectRead;
  $("#project-preview-title").textContent = "Git diff";
  projectPreview.textContent = "加载中…";
  try {
    const text = await ProjectService.diff();
    if (revision === projectRead) projectPreview.textContent = text || "没有已跟踪文件的变更。未跟踪文件可在左侧预览。";
  } catch (err) {
    if (revision === projectRead) projectPreview.textContent = String(err);
  }
}

async function openProjectPanel() {
  projectOverlay.classList.remove("hidden");
  try {
    await refreshProject();
    if (projectRoot) await showProjectDiff();
  } catch (err) {
    projectPreview.textContent = String(err);
  }
}

async function chooseProject() {
  if (busy || switching) return;
  switching = true;
  setComposer();
  try {
    drafts.set(currentSessionId, draft.value);
    const selected = await ProjectService.choose();
    if (!selected.sessionId) return;
    currentSessionId = selected.sessionId;
    draft.value = "";
    projectOverlay.classList.add("hidden");
    const snap = await refreshAll();
    showBanner(snap.ready ? "ready" : "reconnecting", snap.ready ? undefined : "正在加载项目…");
  } catch (err) {
    showActionError(err);
  } finally {
    switching = false;
    renderQueue();
    setComposer();
  }
}

for (const id of ["#project-open", "#project-choose"]) {
  $(id).addEventListener("click", () => void chooseProject());
}
for (const el of [$("#project-review"), chipTools]) {
  el.addEventListener("click", () => void openProjectPanel());
}
$("#project-close").addEventListener("click", () => {
  ++projectRead;
  projectOverlay.classList.add("hidden");
});
$("#project-diff").addEventListener("click", () => void showProjectDiff());
$("#project-refresh").addEventListener("click", () => {
  void refreshProject().then(showProjectDiff).catch((err) => { projectPreview.textContent = String(err); });
});
projectOverlay.addEventListener("click", (e) => {
  if (e.target === projectOverlay) {
    ++projectRead;
    projectOverlay.classList.add("hidden");
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    ++projectRead;
    projectOverlay.classList.add("hidden");
  }
});

// ---- Unresolved sessions: the host refuses new turns after a cancelled or
// interrupted one. A continuation preserves the transcript in a fresh
// journal while keeping the original uncertainty intact.

function showUnresolved(message?: string) {
  $("#unresolved-text").textContent = message || "上次回复已中断，远端结果未知。可以继续发送，已知历史会带入续接会话。";
  unresolvedBar.classList.remove("hidden");
}

function hideUnresolved() {
  unresolvedBar.classList.add("hidden");
}

function isUnresolvedError(err: unknown): boolean {
  return err instanceof Error && /unresolved|unfinished turn/i.test(err.message);
}

$("#unresolved-new").addEventListener("click", () => {
  void newSession(true);
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
  frame?: number;
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
  if (row.frame !== undefined) return;
  row.frame = requestAnimationFrame(() => flushMarkdown(row));
}

function flushMarkdown(row: MessageRow) {
  if (row.frame !== undefined) cancelAnimationFrame(row.frame);
  row.frame = undefined;
  row.text.innerHTML = renderMarkdown(row.raw);
  scrollToBottom();
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
  if (snap.recoveryState === "UNRESOLVED") showUnresolved();
  else hideUnresolved();
  if (snap.pendingText && snap.ready && !busy) await resumePending(snap.pendingText);
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
    think.remove();
  } catch (err) {
    think.remove();
    pausedQueues.add(currentSessionId);
    setError(assistant.root, err);
    if (!draft.value) {
      draft.value = pendingText;
      drafts.set(currentSessionId, pendingText);
    }
  } finally {
    busy = false;
    setComposer();
    drainQueue();
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
  const pendingPromise = start(ch).then(() => null, (err: unknown) => ({ err }));
  let first = true;
  for await (const ev of ch) {
    if (ev.sessionId && ev.sessionId !== currentSessionId) {
      drafts.set(ev.sessionId, draft.value);
      queues.set(ev.sessionId, queue());
      queues.delete(currentSessionId);
      if (pausedQueues.delete(currentSessionId)) pausedQueues.add(ev.sessionId);
      currentSessionId = ev.sessionId;
      hideUnresolved();
      void refreshSessions();
    }
    if (first && ev.kind !== "started") {
      first = false;
      onFirst?.();
    }
    if (ev.kind === "text" && typeof ev.text === "string") {
      appendMarkdown(assistant, ev.text);
      scrollToBottom();
    } else if (ev.kind === "outcome" && ev.outcome) {
      flushMarkdown(assistant);
      setOutcome(assistant.root, ev.outcome);
      if (ev.outcome === "REMOTE_UNCERTAIN") showUnresolved();
    }
  }
  flushMarkdown(assistant);
  const result = await pendingPromise;
  if (result) throw result.err;
}

/** The queued-messages indicator above the composer. */
function renderQueue() {
  const count = queue().length;
  const paused = pausedQueues.has(currentSessionId);
  queueNote.classList.toggle("hidden", count === 0);
  $("#queue-actions").classList.toggle("hidden", count === 0 || !paused || busy);
  if (count > 0) {
    queueNote.textContent = paused ? `已暂停 ${count} 条排队消息` : `已排队 ${count} 条，当前回复结束后依次发送`;
  }
}

function drainQueue() {
  renderQueue();
  if (busy || switching || !ready || pausedQueues.has(currentSessionId)) return;
  const next = queue().shift();
  renderQueue();
  if (next) void performSend(next);
}

$("#queue-resume").addEventListener("click", () => {
  pausedQueues.delete(currentSessionId);
  drainQueue();
});
$("#queue-clear").addEventListener("click", () => {
  queue().length = 0;
  pausedQueues.delete(currentSessionId);
  renderQueue();
});

async function sendText(text: string) {
  if (!text.trim() || switching || !ready) return;
  queue().push(text);
  if (!busy) pausedQueues.delete(currentSessionId);
  drainQueue();
}

async function performSend(text: string) {
  busy = true;
  setComposer();
  addUserRow(text);
  const assistant = addAssistantRow();
  const think = thinkingPill(assistant);
  scrollToBottom();

  try {
    await streamInto(assistant, (ch) => ChatService.send(text, ch), () => think.remove());
    think.remove();
    assistant.userText = text;
    addActions(assistant);
    await refreshSessions(); // the new turn may have renamed the session
  } catch (err) {
    pausedQueues.add(currentSessionId);
    think.remove();
    if (isUnresolvedError(err)) {
      setError(assistant.root, "会话待恢复");
      showUnresolved();
    } else {
      setError(assistant.root, err);
    }
    if (!draft.value) draft.value = text;
    else queue().unshift(text);
    drafts.set(currentSessionId, draft.value);
    autoGrow();
  } finally {
    busy = false;
    setComposer();
    // Send whatever was queued while this turn streamed.
    drainQueue();
  }
}

$("#composer").addEventListener("submit", (e) => {
  e.preventDefault();
  if (!draft.value.trim()) return;
  if (!ready || switching) return;
  const text = draft.value;
  draft.value = "";
  drafts.set(currentSessionId, "");
  autoGrow();
  void sendText(text);
});

// Enter sends; Shift+Enter makes a newline (like the composer it mirrors).
draft.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !draft.disabled) {
    e.preventDefault();
    $("#composer").dispatchEvent(new Event("submit", { cancelable: true }));
  }
});

function autoGrow() {
  draft.style.height = "auto";
  draft.style.height = `${Math.min(draft.scrollHeight, window.innerHeight * 0.4)}px`;
}
draft.addEventListener("input", () => {
  drafts.set(currentSessionId, draft.value);
  autoGrow();
});

stopBtn.addEventListener("click", () => {
  stopping = true;
  pausedQueues.add(currentSessionId);
  renderQueue();
  setComposer();
  void ChatService.stop()
    .then((receipt) => {
      // A cancelled turn commits as uncertain: the host will refuse new
      // turns in THIS session until reconciliation (upstream limitation).
      if (receipt.outcome === "REMOTE_UNCERTAIN") {
        showUnresolved();
      }
    })
    .catch((err) => {
      showActionError(`停止失败：${err instanceof Error ? err.message : String(err)}`);
    }).finally(() => {
      stopping = false;
      setComposer();
      renderQueue();
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

// Keyboard shortcuts, mirroring the desktop app: Cmd+N new chat,
// Cmd+, settings.
document.addEventListener("keydown", (e) => {
  if (!(e.metaKey || e.ctrlKey)) return;
  if (e.key === "n" || e.key === "N") {
    e.preventDefault();
    $("#new-chat").dispatchEvent(new Event("click"));
  }
  if (e.key === ",") {
    e.preventDefault();
    openSettings();
  }
});

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
    settingsHint.textContent = snap.hostRunning ? "" : "保存后点击「保存并重启 host」即可重新启动。";
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
for (const el of [chipProvider, composerChip]) {
  el.addEventListener("click", openSettings);
}
setProvider.addEventListener("change", () => {
  setModel.value = "";
  setBaseUrl.value = "";
  setKey.value = "";
  setKey.placeholder = "输入此 provider 的 API Key，或使用其环境变量";
  settingsHint.textContent = "切换 provider 会清除旧凭证并恢复该 provider 的模型、端点和协议默认值。";
});

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
