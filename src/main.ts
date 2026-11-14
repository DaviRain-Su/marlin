import { Channel } from "mygo-runtime";
import { marked } from "marked";
import DOMPurify from "dompurify";
import { ChatService, SettingsService, events, type StoredTurn, type TurnProgress } from "./mygo";

// Model output is untrusted content rendered inside the page that holds the
// Go IPC bridge — sanitize aggressively before it becomes HTML.
marked.setOptions({ async: false, gfm: true, breaks: true });
function renderMarkdown(text: string): string {
  return DOMPurify.sanitize(marked.parse(text) as string, {
    FORBID_TAGS: ["style", "form", "iframe"],
    FORBID_ATTR: ["onerror", "onclick", "onload"],
  });
}

const $ = <T extends HTMLElement>(selector: string) => document.querySelector<T>(selector)!;

const messages = $("#messages");
const draft = $<HTMLTextAreaElement>("#draft");
const sendBtn = $<HTMLButtonElement>("#send");
const stopBtn = $<HTMLButtonElement>("#stop");
const banner = $("#banner");
const emptyState = $("#empty");

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
  if (s.state === "ready") void renderHistory();
});

// ---- Sidebar chips: the current provider/model and tool surface ----

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

function updateEmptyState() {
  emptyState.classList.toggle("hidden", messages.children.length > 0);
}

interface MessageRow {
  root: HTMLElement;
  text: HTMLElement;
  /** Raw accumulated markdown for assistant rows; "" for user rows. */
  raw: string;
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
  updateEmptyState();
  scrollToBottom();
  return { root, text: body, raw: "" };
}

function addAssistantRow(): MessageRow {
  const root = document.createElement("div");
  root.className = "msg assistant";
  const body = document.createElement("div");
  body.className = "msg-text markdown";
  root.appendChild(body);
  messages.appendChild(root);
  updateEmptyState();
  scrollToBottom();
  return { root, text: body, raw: "" };
}

/** Re-renders an assistant row's accumulated markdown (per streaming chunk). */
function appendMarkdown(row: MessageRow, delta: string) {
  row.raw += delta;
  row.text.innerHTML = renderMarkdown(row.raw);
}

/** Hover action: copy the assistant's full text (rendered or partial). */
function addCopyAction(row: MessageRow) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "copy-btn";
  btn.textContent = "复制";
  btn.addEventListener("click", async () => {
    const text = row.raw || row.text.textContent || "";
    await navigator.clipboard.writeText(text);
    btn.textContent = "已复制";
    setTimeout(() => (btn.textContent = "复制"), 1200);
  });
  const actions = document.createElement("div");
  actions.className = "msg-actions";
  actions.appendChild(btn);
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
  messages.scrollTop = messages.scrollHeight;
}

function renderStoredTurn(row: StoredTurn) {
  addUserRow(row.userText);
  const assistant = addAssistantRow();
  if (row.assistantText) appendMarkdown(assistant, row.assistantText);
  if (row.outcome) setOutcome(assistant.root, row.outcome);
  if (row.assistantText) addCopyAction(assistant);
}

async function renderHistory() {
  const snap = await ChatService.state();
  messages.replaceChildren();
  updateEmptyState();
  for (const row of snap.history ?? []) renderStoredTurn(row);
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
  const note = document.createElement("div");
  note.className = "pill";
  note.textContent = "上次发送未确认，正在恢复…";
  assistant.root.appendChild(note);
  scrollToBottom();
  try {
    await streamInto(assistant, (ch) => ChatService.resume(ch));
    note.remove();
    addCopyAction(assistant);
  } catch (err) {
    setError(assistant.root, err);
    note.remove();
  } finally {
    busy = false;
    setComposer();
  }
}

// streamInto drives one channel to completion against a row.
async function streamInto(assistant: MessageRow, start: (ch: Channel<TurnProgress>) => Promise<void>) {
  const ch = new Channel<TurnProgress>();
  const pendingPromise = start(ch);
  for await (const ev of ch) {
    if (ev.kind === "text" && typeof ev.text === "string") {
      appendMarkdown(assistant, ev.text);
      scrollToBottom();
    } else if (ev.kind === "outcome" && ev.outcome) {
      setOutcome(assistant.root, ev.outcome);
    }
  }
  await pendingPromise;
}

$("#composer").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (busy || !ready || !draft.value.trim()) return;
  const text = draft.value;
  draft.value = "";
  autoGrow();

  busy = true;
  setComposer();
  addUserRow(text);
  const assistant = addAssistantRow();
  scrollToBottom();

  try {
    await streamInto(assistant, (ch) => ChatService.send(text, ch));
    addCopyAction(assistant);
  } catch (err) {
    setError(assistant.root, err);
    draft.value = text; // the draft comes back: the host never admitted it
    autoGrow();
  } finally {
    busy = false;
    setComposer();
  }
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
const snap = await ChatService.state();
await Promise.all([renderHistory(), refreshChips()]);
if (snap.ready) showBanner("ready");
else showBanner("starting", "waiting for the Marlin host…");
setComposer();
