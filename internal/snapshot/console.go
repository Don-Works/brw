package snapshot

// ConsoleCaptureInstallScript installs a bounded, idempotent console interceptor.
// It is intentionally in-page so direct CDP and the extension transport expose
// the same drain semantics. The original console methods always run.
const ConsoleCaptureInstallScript = `(function() {
  if (Array.isArray(window.__brwConsole)) return {installed:true, already:true};
  var MAX = 200;
  var TEXT_CAP = 1000;
  var messages = [];
  window.__brwConsole = messages;
  function stringify(value) {
    try {
      if (value instanceof Error) return value.stack || value.message || String(value);
      if (typeof value === 'object' && value !== null) return JSON.stringify(value, function(_key, item) {
        return typeof item === 'bigint' ? String(item) : item;
      });
      return String(value);
    } catch (_err) {
      try { return String(value); } catch (_ignored) { return '[unprintable]'; }
    }
  }
  ['log','warn','error','info','debug'].forEach(function(level) {
    var original = console[level];
    if (typeof original !== 'function') return;
    console[level] = function() {
      var text = Array.prototype.map.call(arguments, stringify).join(' ');
      messages.push({level:level, text:text.slice(0, TEXT_CAP), timestamp:new Date().toISOString()});
      if (messages.length > MAX) messages.shift();
      return original.apply(console, arguments);
    };
  });
  return {installed:true, already:false};
})()`

const ConsoleCaptureDrainScript = `(function() {
  if (!Array.isArray(window.__brwConsole)) return [];
  var messages = window.__brwConsole.slice();
  window.__brwConsole.length = 0;
  return messages;
})()`
