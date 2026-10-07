
const KEEPALIVE_MESSAGE_MS = 20000;
const AUDIO_RESUME_MS = 25000;
const PORT_REOPEN_MS = 1000;

function startSilentAudio() {
  try {
    const Ctx = self.AudioContext || self.webkitAudioContext;
    if (!Ctx) return;
    const ctx = new Ctx();
    const oscillator = ctx.createOscillator();
    const gain = ctx.createGain();
    gain.gain.value = 0;
    oscillator.connect(gain);
    gain.connect(ctx.destination);
    oscillator.start();
    const resume = () => {
      if (ctx.state === "suspended") ctx.resume().catch(() => {});
    };
    resume();
    setInterval(resume, AUDIO_RESUME_MS);
  } catch (_) {
  }
}

let keepAlivePort = null;
function connectPort() {
  try {
    keepAlivePort = chrome.runtime.connect({ name: "brw-keepalive" });
    keepAlivePort.onDisconnect.addListener(() => {
      keepAlivePort = null;
      setTimeout(connectPort, PORT_REOPEN_MS);
    });
  } catch (_) {
    keepAlivePort = null;
    setTimeout(connectPort, PORT_REOPEN_MS);
  }
}

function pokeWorker() {
  chrome.runtime.sendMessage({ type: "SW_KEEPALIVE" }).catch(() => {
  });
  if (!keepAlivePort) connectPort();
}

startSilentAudio();
connectPort();
setInterval(pokeWorker, KEEPALIVE_MESSAGE_MS);
