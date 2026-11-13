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
const draft = $<HTMLInputElement>("#draft");
const sendBtn = $<HTMLButtonElement>("#send");
const stopBtn = $<HTMLButtonElement>("#stop");
const banner = $("#banner");

// Settings panel elements.
const settingsPanel = $("#settings");
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

function setComposer() {
  sendBtn.disabled = busy || !ready;
  draft.disabled = busy;
  stopBtn.classList.toggle("hidden", !busy);
}

function showBanner(state: string, message?: string) {
  const label = message || state;
  banner.classList.remove("hidden");
  banner.dataset.state = state;
  banner.textContent = label;
  ready = state === "ready";
  if (ready) banner.classList.add("hidden");
  setComposer();
}

// Reconnect / unresolved banners arrive as events; the initial pull covers
// app startup. A fresh "ready" re-renders from the persisted projection
// (host restarts must not blank the view).
events.status.on((s) => {
  showBanner(s.state, s.message);
  if (s.state === "ready") void renderHistory();
});

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
  scrollToBottom();
  return { root, text: body, raw: "" };
}

/** Re-renders an assistant row's accumulated markdown (per streaming chunk). */
function appendMarkdown(row: MessageRow, delta: string) {
  row.raw += delta;
  row.text.innerHTML = renderMarkdown(row.raw);
}

function setOutcome(row: HTMLElement, outcome: string) {
  const pill = document.createElement("div");
  pill.className = `pill ${outcome}`;
  pill.textContent =
    outcome === "COMPLETED"
      ? "completed"
      : outcome === "REMOTE_UNCERTAIN"
        ? "stopped (remote result unknown)"
        : outcome === "REJECTED_BEFORE_DISPATCH"
          ? "refused before dispatch"
          : outcome.toLowerCase();
  row.appendChild(pill);
}

function setError(row: HTMLElement, err: unknown) {
  const pill = document.createElement("div");
  pill.className = "pill error";
  pill.textContent = `send failed: ${err instanceof Error ? err.message : String(err)}`;
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
}

async function renderHistory() {
  const snap = await ChatService.state();
  messages.replaceChildren();
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

  busy = true;
  setComposer();
  addUserRow(text);
  const assistant = addAssistantRow();
  scrollToBottom();

  try {
    await streamInto(assistant, (ch) => ChatService.send(text, ch));
  } catch (err) {
    setError(assistant.root, err);
    draft.value = text; // the draft comes back: the host never admitted it
  } finally {
    busy = false;
    setComposer();
  }
});

stopBtn.addEventListener("click", () => {
  void ChatService.stop().catch((err) => {
    showBanner("error", `stop failed: ${err instanceof Error ? err.message : String(err)}`);
  });
});

// ---- Settings panel ----

function setHint(text: string) {
  settingsHint.textContent = text;
}

async function openSettings() {
  settingsPanel.classList.remove("hidden");
  setHint("加载中…");
  try {
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
    setHint(snap.hostRunning ? "" : "host 未运行，保存后将于下次启动生效");
  } catch (err) {
    setHint(`读取失败：${err instanceof Error ? err.message : String(err)}`);
  }
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

document.querySelector("#settings-toggle")!.addEventListener("click", () => {
  if (settingsPanel.classList.contains("hidden")) void openSettings();
  else settingsPanel.classList.add("hidden");
});

document.querySelector("#settings-save")!.addEventListener("click", () => {
  void SettingsService.save(settingsInput())
    .then(() => setHint("已保存。host 重启后生效（点「保存并重启 host」立即生效）。"))
    .catch((err) => setHint(`保存失败：${err instanceof Error ? err.message : String(err)}`));
});

document.querySelector("#settings-apply")!.addEventListener("click", () => {
  void (async () => {
    setHint("保存并重启 host…");
    await SettingsService.save(settingsInput());
    await SettingsService.apply();
    setHint("已保存，host 正在用新配置重启。");
  })().catch((err) => setHint(`失败：${err instanceof Error ? err.message : String(err)}`));
});

// Startup: render the persisted projection, then let the snapshot decide
// whether a pending submission needs resuming.
const snap = await ChatService.state();
await renderHistory();
if (snap.ready) showBanner("ready");
else showBanner("starting", "waiting for the Marlin host…");
setComposer();
