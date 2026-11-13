import { Channel } from "mygo-runtime";
import { ChatService, events, type TurnProgress } from "./mygo";

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

// Reconnect banners arrive as events; the initial pull covers app startup.
events.status.on((s) => showBanner(s.state, s.message));

interface MessageRow {
  root: HTMLElement;
  text: HTMLElement;
  pill: HTMLElement | null;
}

function addMessage(role: "user" | "assistant"): MessageRow {
  const root = document.createElement("div");
  root.className = `msg ${role}`;
  const text = document.createElement("div");
  text.className = "msg-text";
  text.style.whiteSpace = "pre-wrap";
  root.appendChild(text);
  messages.appendChild(root);
  messages.scrollTop = messages.scrollHeight;
  return { root, text, pill: null };
}

function setOutcome(row: MessageRow, outcome: string) {
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
  row.root.appendChild(pill);
  row.pill = pill;
}

function setError(row: MessageRow, err: unknown) {
  row.text.textContent ||= " ";
  const pill = document.createElement("div");
  pill.className = "pill error";
  pill.textContent = `send failed: ${err instanceof Error ? err.message : String(err)}`;
  row.root.appendChild(pill);
}

function scrollToBottom() {
  messages.scrollTop = messages.scrollHeight;
}

$("#composer").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (busy || !ready || !draft.value.trim()) return;
  const text = draft.value;
  draft.value = "";

  busy = true;
  setComposer();
  addMessage("user").text.textContent = text;
  const assistant = addMessage("assistant");
  scrollToBottom();

  const ch = new Channel<TurnProgress>();
  try {
    const sending = ChatService.send(text, ch);
    for await (const ev of ch) {
      if (ev.kind === "text") {
        assistant.text.textContent += ev.text;
        scrollToBottom();
      } else if (ev.kind === "outcome" && ev.outcome) {
        setOutcome(assistant, ev.outcome);
      }
    }
    await sending;
  } catch (err) {
    setError(assistant, err);
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

// Startup: pull the current snapshot (status events cover later changes).
const snap = await ChatService.state();
if (snap.ready) showBanner("ready");
else showBanner("starting", "waiting for the Marlin host…");
setComposer();
