import { Channel } from "mygo-runtime";
import { ChatService, events, type StoredTurn, type TurnProgress } from "./mygo";

const $ = <T extends HTMLElement>(selector: string) => document.querySelector<T>(selector)!;

const messages = $("#messages");
const draft = $<HTMLInputElement>("#draft");
const sendBtn = $<HTMLButtonElement>("#send");
const stopBtn = $<HTMLButtonElement>("#stop");
const banner = $("#banner");

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
  return { root, text: body };
}

function addAssistantRow(): MessageRow {
  const root = document.createElement("div");
  root.className = "msg assistant";
  const body = document.createElement("div");
  body.className = "msg-text";
  body.style.whiteSpace = "pre-wrap";
  root.appendChild(body);
  messages.appendChild(root);
  scrollToBottom();
  return { root, text: body };
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
  assistant.text.textContent = row.assistantText || "";
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
    if (ev.kind === "text") {
      assistant.text.textContent += ev.text;
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

// Startup: render the persisted projection, then let the snapshot decide
// whether a pending submission needs resuming.
const snap = await ChatService.state();
await renderHistory();
if (snap.ready) showBanner("ready");
else showBanner("starting", "waiting for the Marlin host…");
setComposer();
