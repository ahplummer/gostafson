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
    case "user": addUser(e); break;
    case "reply":
      add("reply", render(e.text), true);
      if (reloadAfter) { reloadAfter = false; const note = e.text; loadHistory().then(() => add("note", note)); }
      loadChats();
      break;
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
// Saved conversations (PRD D38): the sidebar lists them; the page draws the
// current one from the saved history, and again after /chat, /new or
// /delete switches it.
const chatsEl = document.getElementById("chats");
let reloadAfter = false;
const closeSide = () => document.body.classList.remove("side-open");
document.getElementById("menu").addEventListener("click", () => document.body.classList.toggle("side-open"));
document.getElementById("scrim").addEventListener("click", closeSide);
document.getElementById("newchat").addEventListener("click", () => { reloadAfter = true; send("/new"); closeSide(); });

// In a project, the sidebar shows that project's conversations unless "all"
// is picked (PRD D42); the choice is kept per browser.
let showAll = false;
try { showAll = localStorage.getItem("chats-all") === "1"; } catch {}
const filterEl = document.getElementById("filter");
const fproj = document.getElementById("fproj"), fall = document.getElementById("fall");
function setShowAll(v) {
  showAll = v;
  try { localStorage.setItem("chats-all", v ? "1" : "0"); } catch {}
  loadChats();
}
fproj.addEventListener("click", () => setShowAll(false));
fall.addEventListener("click", () => setShowAll(true));

async function loadChats() {
  let data;
  try { const r = await fetch("/api/chats"); if (!r.ok) return; data = await r.json(); } catch { return; }
  const project = data.project || "";
  filterEl.hidden = !project;
  fproj.textContent = project;
  fproj.setAttribute("aria-pressed", String(!showAll));
  fall.setAttribute("aria-pressed", String(showAll));
  const list = project && !showAll ? data.chats.filter(c => c.project === project) : data.chats;
  chatsEl.textContent = "";
  if (list.length === 0 && project && !showAll) {
    const li = document.createElement("li"); li.className = "empty";
    li.textContent = "no saved conversations in " + project + " yet";
    chatsEl.append(li);
  }
  for (const c of list) {
    const li = document.createElement("li");
    if (c.current) li.className = "current";
    const label = document.createElement("span"); label.className = "label";
    const m = /^\[([^\]]+)\] (.*)$/.exec(c.label);    // "[travato] Van oil": the project stands out
    if (m) {
      const p = document.createElement("span"); p.className = "proj"; p.textContent = "[" + m[1] + "] ";
      label.append(p, document.createTextNode(m[2]));
    } else label.textContent = c.label;
    const meta = document.createElement("span"); meta.className = "meta";
    meta.textContent = c.when + (c.profile ? " · @" + c.profile : "") + " · " + c.messages + " messages";
    li.append(label, meta);
    li.title = c.label;
    li.addEventListener("click", () => { if (!c.current) { reloadAfter = true; send("/chat " + c.id); } closeSide(); });
    chatsEl.append(li);
  }
}

async function loadHistory() {
  let msgs;
  try { const r = await fetch("/api/history"); if (!r.ok) return; msgs = await r.json(); } catch { return; }
  log.textContent = "";
  for (const m of msgs) {
    if (m.role === "user") addUser({ text: m.text, images: m.images });
    else add("reply", render(m.text), true);
  }
}

function connect() {
  log.textContent = "";
  loadHistory();
  loadChats();
  source = new EventSource("/api/events");
  source.onmessage = m => handle(JSON.parse(m.data));
  source.onerror = () => {
    conn.textContent = "reconnecting…"; conn.className = "down";
    if (source.readyState === EventSource.CLOSED) { setTimeout(connect, 3000); }
  };
}

// Images: picked, pasted or dropped. Each is scaled to at most 1568 px on
// its long side before upload (the server checks again).
const attachments = document.getElementById("attachments");
const fileInput = document.getElementById("file");
let pending = [];        // [{name, blob, url}] waiting to be sent
let sentPreviews = [];   // preview URLs of messages this page sent, matched to "user" events

function showAttachments() {
  attachments.textContent = "";
  attachments.hidden = pending.length === 0;
  pending.forEach((p, i) => {
    const chip = document.createElement("div"); chip.className = "chip";
    let img;
    if (p.doc) { img = document.createElement("div"); img.className = "doc"; img.textContent = "📄 " + p.name; }
    else { img = document.createElement("img"); img.src = p.url; img.alt = p.name; }
    const x = document.createElement("button"); x.type = "button"; x.textContent = "×"; x.title = "Remove";
    x.onclick = () => { if (p.url) URL.revokeObjectURL(p.url); pending.splice(i, 1); showAttachments(); };
    chip.append(img, x); attachments.append(chip);
  });
}

async function shrink(file) {
  try {
    const bmp = await createImageBitmap(file);
    const scale = Math.min(1, 1568 / Math.max(bmp.width, bmp.height));
    if (scale === 1 && file.size < 4 << 20) return file;
    const c = document.createElement("canvas");
    c.width = Math.round(bmp.width * scale); c.height = Math.round(bmp.height * scale);
    c.getContext("2d").drawImage(bmp, 0, 0, c.width, c.height);
    const type = file.type === "image/png" ? "image/png" : "image/jpeg";
    return await new Promise(res => c.toBlob(b => res(b || file), type, 0.9));
  } catch { return file; } // the server can still read (and scale) it
}

async function addFiles(files) {
  for (const f of files) {
    if (pending.length >= 4) { add("note", "up to 4 files per message"); break; }
    if (!f.type.startsWith("image/")) {
      if (!/\.(pdf|txt|md|markdown|csv)$/i.test(f.name)) { add("note", f.name + ": I can take images, PDFs and text files"); continue; }
      if (f.size > 20 << 20) { add("note", f.name + " is over 20 MB"); continue; }
      pending.push({ name: f.name, blob: f, doc: true }); // a document for the project
      continue;
    }
    const blob = await shrink(f);
    pending.push({ name: f.name || "pasted.png", blob, url: URL.createObjectURL(blob) });
  }
  showAttachments();
}

function toBase64(blob) {
  return new Promise((res, rej) => {
    const r = new FileReader();
    r.onload = () => res(String(r.result).split(",", 2)[1]);
    r.onerror = rej;
    r.readAsDataURL(blob);
  });
}

function addUser(e) {
  const div = document.createElement("div");
  div.className = "msg user";
  div.textContent = e.text || "";
  if (e.files && e.files.length) {
    const docs = document.createElement("div"); docs.className = "thumbs";
    docs.textContent = e.files.map(n => "📄 " + n).join("  ");
    div.append(docs);
  }
  if (e.images) {
    const thumbs = document.createElement("div"); thumbs.className = "thumbs";
    const mine = sentPreviews.shift();
    if (mine) {
      for (const url of mine) { const img = document.createElement("img"); img.src = url; thumbs.append(img); }
    } else {
      thumbs.textContent = "🖼 " + e.images + (e.images === 1 ? " image" : " images");
    }
    div.append(thumbs);
  }
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
}

async function send(t, items = []) {
  const images = [], files = [];
  for (const p of items) (p.doc ? files : images).push({ name: p.name, data: await toBase64(p.blob) });
  const imgs = items.filter(p => !p.doc);
  if (imgs.length) sentPreviews.push(imgs.map(p => p.url));
  const r = await fetch("/api/send", {
    method: "POST", headers: { "Content-Type": "application/json", "X-Gostafson": "1" },
    body: JSON.stringify({ text: t, images, files }),
  });
  if (r.status === 401) location.reload();
  else if (!r.ok) add("note", "send failed: " + r.status + " " + (await r.text()));
}

document.getElementById("compose").addEventListener("submit", ev => {
  ev.preventDefault();
  const t = text.value.trim();
  if (!t && !pending.length) return;
  if (/^\/(chat|new|reset|delete|project)\b/.test(t)) reloadAfter = true; // these can switch the conversation
  const imgs = pending; pending = []; showAttachments();
  text.value = ""; text.style.height = "";
  send(t, imgs);
});
document.getElementById("attach").addEventListener("click", () => fileInput.click());
fileInput.addEventListener("change", () => { addFiles([...fileInput.files]); fileInput.value = ""; });
text.addEventListener("paste", ev => {
  const files = [...(ev.clipboardData?.files || [])];
  if (files.length) { ev.preventDefault(); addFiles(files); }
});
document.addEventListener("dragover", ev => { ev.preventDefault(); document.body.classList.add("dragging"); });
document.addEventListener("dragleave", ev => { if (!ev.relatedTarget) document.body.classList.remove("dragging"); });
document.addEventListener("drop", ev => {
  ev.preventDefault(); document.body.classList.remove("dragging");
  addFiles([...(ev.dataTransfer?.files || [])]);
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
