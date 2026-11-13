const RELAY = "wss://relay.example.com";

function connectRelay(url) {
  const ws = new WebSocket(url);
  ws.onopen = () => ws.send(JSON.stringify(["REQ", "feed", { kinds: [1], limit: 20 }]));
  ws.onmessage = (msg) => {
    const [type, , event] = JSON.parse(msg.data);
    if (type === "EVENT") showEvent(event);
  };
  return ws;
}

function showEvent(event) {
  const li = document.createElement("li");
  li.textContent = event.content;
  document.getElementById("events").appendChild(li);
}

connectRelay(RELAY);
