"use strict";
const log = document.getElementById("log");
const spinner = document.getElementById("spinner");
const ask = document.getElementById("ask");
const conn = document.getElementById("conn");
const text = document.getElementById("text");

const verbs = ["Pontificating", "Swirling", "Ruminating", "Percolating", "Noodling",
  "Cogitating", "Marinating", "Mulling", "Simmering", "Conjuring", "Untangling",
  "Deliberating", "Musing", "Brewing", "Tinkering", "Woolgathering", "Spelunking",
  "Divining", "Rummaging", "Puzzling", "Whittling", "Daydreaming", "Concocting",
  "Fermenting", "Moseying"];
const frames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏";

function escapeHTML(s) {
  return s.replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// Small markdown subset: fenced code, inline code, bold, error lines, and the
// "[profile | model | 1.2s | 34 tok]" footer. Text is escaped first.
function render(src) {
  const lines = src.split("\n");
  let footer = "";
  if (lines.length && /^\[[^\]]*\|[^\]]*\]$/.test(lines[lines.length - 1].trim())) footer = lines.pop();
  const out = [];
  let fence = null;
  for (const line of lines) {
    if (line.trim().startsWith("```")) {
      if (fence === null) { fence = []; } else { out.push('<pre class="code">' + fence.map(escapeHTML).join("\n") + "</pre>"); fence = null; }
      continue;
    }
    if (fence !== null) { fence.push(line); continue; }
    let h = escapeHTML(line)
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    if (line.trim().startsWith("error:")) h = '<span class="err">' + h + "</span>";
    out.push(h);
  }
  if (fence !== null) out.push('<pre class="code">' + fence.map(escapeHTML).join("\n") + "</pre>");
  let html = out.join("\n").replace(/\n?(<pre class="code">)/g, "$1").replace(/(<\/pre>)\n?/g, "$1");
  if (footer) html += '<span class="footer">' + escapeHTML(footer) + "</span>";
  return html;
}

function add(cls, content, html) {
  const div = document.createElement("div");
  div.className = "msg " + cls;
  if (html) div.innerHTML = content; else div.textContent = content;
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
}

let spinTimer = null;
function startSpin() {
  stopSpin();
  const start = Date.now();
  let i = 0, verb = verbs[Math.floor(Math.random() * verbs.length)];
  spinner.hidden = false;
  spinTimer = setInterval(() => {
    i++;
    if (i % 45 === 0) {
      let v; do { v = verbs[Math.floor(Math.random() * verbs.length)]; } while (v === verb);
      verb = v;
    }
    spinner.querySelector(".frame").textContent = frames[i % frames.length];
    spinner.querySelector(".verb").textContent = verb;
    spinner.querySelector(".secs").textContent = Math.round((Date.now() - start) / 1000) + "s";
  }, 90);
}
function stopSpin() {
  if (spinTimer) clearInterval(spinTimer);
  spinTimer = null;
  spinner.hidden = true;
}

let busy = false, asking = false, question = "";
function handle(e) {
  switch (e.type) {
    case "ready": conn.textContent = "connected"; conn.className = "ok"; break;
    case "user": add("user", e.text); break;
    case "reply": add("reply", render(e.text), true); break;
    case "busy": busy = true; if (!asking) startSpin(); break;
    case "idle": busy = false; stopSpin(); break;
    case "ask":
      asking = true; stopSpin(); question = e.text;
      ask.querySelector(".q").textContent = e.text;
      ask.hidden = false;
      document.getElementById("yes").focus();
      break;
    case "answered":
      asking = false; ask.hidden = true;
      add("asked", question + " → " + e.text);
      if (busy) startSpin();
      break;
  }
}

let source = null;
function connect() {
  log.textContent = "";            // the server replays recent history on connect
  source = new EventSource("/api/events");
  source.onmessage = m => handle(JSON.parse(m.data));
  source.onerror = () => {
    conn.textContent = "reconnecting…"; conn.className = "down";
    if (source.readyState === EventSource.CLOSED) { setTimeout(connect, 3000); }
  };
}

async function send(t) {
  const r = await fetch("/api/send", {
    method: "POST", headers: { "Content-Type": "application/json", "X-Gostafson": "1" },
    body: JSON.stringify({ text: t }),
  });
  if (r.status === 401) location.reload();
  else if (!r.ok) add("note", "send failed: " + r.status + " " + (await r.text()));
}

document.getElementById("compose").addEventListener("submit", ev => {
  ev.preventDefault();
  const t = text.value.trim();
  if (!t) return;
  text.value = ""; text.style.height = "";
  send(t);
});
text.addEventListener("keydown", ev => {
  if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
    ev.preventDefault();
    document.getElementById("compose").requestSubmit();
  }
});
text.addEventListener("input", () => { text.style.height = ""; text.style.height = text.scrollHeight + "px"; });
document.getElementById("yes").addEventListener("click", () => send("yes"));
document.getElementById("no").addEventListener("click", () => send("no"));

connect();
