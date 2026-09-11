package browser

import (
	"encoding/json"
	"fmt"

	"github.com/Don-Works/brw/internal/navpolicy"
)

// containmentGuardScript wraps the network APIs that CDP's Fetch domain cannot
// intercept.
//
// Fetch interception is the authoritative layer for HTTP requests, but it does
// not see WebSocket handshakes, and WebRTC reaches the network without an
// interceptable request at all. Those channels are the most direct way for a
// contained page to talk to a host the allowlist excludes, so they are closed
// in the page itself.
//
// This is defence in depth, not a sandbox. It runs before page scripts (via
// Page.addScriptToEvaluateOnNewDocument), which defeats ordinary page code and
// injected content, but a determined attacker inside the page can still reach a
// pristine realm — e.g. the constructors on a fresh same-origin iframe's
// contentWindow. HTTP requests from such a realm are still caught by Fetch
// interception; WebSocket and WebRTC from it are not. The honest boundary is
// "an allowlisted page cannot casually phone home", not "confinement no page
// script can escape".
const containmentGuardScript = `(function(allowed, blocked){
  if (window.__brwContainmentInstalled) return;
  Object.defineProperty(window, '__brwContainmentInstalled', {value: true, configurable: true});

  function hostMatches(host, domain){
    if (!host || !domain) return false;
    host = String(host).toLowerCase();
    domain = String(domain).toLowerCase();
    return host === domain || host.endsWith('.' + domain);
  }
  function hostOf(raw){
    try {
      // Resolving against the document base mirrors what the browser will
      // actually connect to for a relative or protocol-relative reference.
      var u = new URL(String(raw), document.baseURI);
      return u.hostname;
    } catch (e) { return ''; }
  }
  function permitted(raw){
    var host = hostOf(raw);
    // No host means a same-document or inline destination; it cannot leave.
    if (!host) return true;
    for (var i = 0; i < blocked.length; i++) if (hostMatches(host, blocked[i])) return false;
    if (!allowed.length) return true;
    for (var j = 0; j < allowed.length; j++) if (hostMatches(host, allowed[j])) return true;
    return false;
  }
  function refuse(api, raw){
    return new DOMException(
      'brw containment refused ' + api + ' to ' + hostOf(raw) + ': not permitted by --allowed-domains',
      'SecurityError');
  }

  try {
    var OrigWebSocket = window.WebSocket;
    if (OrigWebSocket) {
      var GuardedWebSocket = function(url, protocols){
        if (!permitted(url)) throw refuse('WebSocket', url);
        return protocols === undefined ? new OrigWebSocket(url) : new OrigWebSocket(url, protocols);
      };
      GuardedWebSocket.prototype = OrigWebSocket.prototype;
      ['CONNECTING','OPEN','CLOSING','CLOSED'].forEach(function(k){
        try { GuardedWebSocket[k] = OrigWebSocket[k]; } catch (e) {}
      });
      window.WebSocket = GuardedWebSocket;
    }
  } catch (e) {}

  try {
    var OrigEventSource = window.EventSource;
    if (OrigEventSource) {
      var GuardedEventSource = function(url, config){
        if (!permitted(url)) throw refuse('EventSource', url);
        return config === undefined ? new OrigEventSource(url) : new OrigEventSource(url, config);
      };
      GuardedEventSource.prototype = OrigEventSource.prototype;
      window.EventSource = GuardedEventSource;
    }
  } catch (e) {}

  try {
    if (navigator.sendBeacon) {
      var origSendBeacon = navigator.sendBeacon.bind(navigator);
      // sendBeacon returns a boolean rather than throwing; false is "not queued",
      // which is the truthful answer for a refused destination.
      navigator.sendBeacon = function(url, data){
        if (!permitted(url)) return false;
        return origSendBeacon(url, data);
      };
    }
  } catch (e) {}

  // WebRTC negotiates peer connections outside the URL-request model, so there
  // is no destination to check against the allowlist. Under containment it is
  // closed rather than filtered.
  try {
    ['RTCPeerConnection','webkitRTCPeerConnection'].forEach(function(name){
      if (!window[name]) return;
      window[name] = function(){
        throw new DOMException('brw containment disables WebRTC while --allowed-domains is active', 'NotAllowedError');
      };
    });
  } catch (e) {}
})`

// BuildContainmentGuard renders the guard with the policy's domain lists baked
// in. Exported so the extension transport installs the SAME guard as the
// direct-CDP one; two spellings of a security boundary is how they drift.
func BuildContainmentGuard(p *navpolicy.Policy) (string, error) {
	// A nil policy is a valid "no policy" value elsewhere (Policy.Confines and
	// Policy.Empty both accept it), so it must be valid here too. Interception
	// can be armed for routes alone, with no policy configured at all.
	if p == nil {
		p = &navpolicy.Policy{}
	}
	allowed := p.Allowed
	if allowed == nil {
		allowed = []string{}
	}
	blocked := p.Blocked
	if blocked == nil {
		blocked = []string{}
	}
	allowedJSON, err := json.Marshal(allowed)
	if err != nil {
		return "", err
	}
	blockedJSON, err := json.Marshal(blocked)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s(%s,%s)", containmentGuardScript, allowedJSON, blockedJSON), nil
}
