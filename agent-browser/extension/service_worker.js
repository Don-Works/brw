const state = {
  socket: null,
  connected: false
};

chrome.action.onClicked.addListener(async (tab) => {
  await connect();
  if (tab?.id) {
    send({ type: "active_tab", tabId: tab.id, url: tab.url, title: tab.title });
  }
});

async function connect() {
  if (state.socket && state.socket.readyState === WebSocket.OPEN) return;
  state.socket = new WebSocket("ws://127.0.0.1:17311/extension");
  state.socket.onopen = () => {
    state.connected = true;
    send({ type: "hello", source: "agent-browser-extension", version: "0.1.0" });
  };
  state.socket.onclose = () => {
    state.connected = false;
  };
  state.socket.onmessage = async (event) => {
    let message;
    try {
      message = JSON.parse(event.data);
    } catch (error) {
      send({ id: null, ok: false, error: String(error) });
      return;
    }
    await handle(message);
  };
}

async function handle(message) {
  if (message.type === "cdp") {
    try {
      const debuggee = { tabId: message.tabId };
      await chrome.debugger.attach(debuggee, "1.3").catch((error) => {
        if (!String(error).includes("Another debugger is already attached")) throw error;
      });
      const result = await chrome.debugger.sendCommand(debuggee, message.method, message.params || {});
      send({ id: message.id, ok: true, result });
    } catch (error) {
      send({ id: message.id, ok: false, error: String(error?.message || error) });
    }
  }
}

function send(payload) {
  if (!state.socket || state.socket.readyState !== WebSocket.OPEN) return;
  state.socket.send(JSON.stringify(payload));
}
