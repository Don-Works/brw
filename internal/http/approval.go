package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/Don-Works/brw/internal/approval"
)

// SetApprovalStore enables the loopback approval inbox with an independent operator bearer token.
func (s *Server) SetApprovalStore(store *approval.Store, operatorToken string) error {
	if store == nil {
		return errors.New("approval inbox requires a store")
	}
	if !s.loopbackBind {
		return errors.New("approval inbox requires a loopback bind")
	}
	if len(operatorToken) < 32 || strings.ContainsAny(operatorToken, " \t\r\n") {
		return errors.New("approval inbox requires an operator token of at least 32 characters without whitespace")
	}
	s.approvals = store
	s.approvalOperatorToken = operatorToken
	return nil
}

func approvalPrivacyHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (s *Server) approvalGuard(w http.ResponseWriter, r *http.Request, operator bool) bool {
	approvalPrivacyHeaders(w)
	if s.approvals == nil {
		http.NotFound(w, r)
		return false
	}
	u, err := url.Parse("http://" + r.Host)
	if !s.loopbackBind || !requestIsLoopback(r) || err != nil || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !isLoopbackHost(strings.ToLower(u.Hostname())) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval inbox requires a loopback request"})
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && !approvalSameOrigin(origins[0], r)) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval inbox requires a same-origin request"})
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval inbox requires a same-origin request"})
		return false
	}
	if operator {
		headers := r.Header.Values("Authorization")
		provided := ""
		if len(headers) == 1 && strings.HasPrefix(headers[0], "Bearer ") {
			provided = strings.TrimPrefix(headers[0], "Bearer ")
		}
		got := sha256.Sum256([]byte(provided))
		want := sha256.Sum256([]byte(s.approvalOperatorToken))
		if s.approvalOperatorToken == "" || provided == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer realm=\"brw operator\"")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "operator authentication required"})
			return false
		}
	}
	return true
}

func approvalSameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func (s *Server) approvalList(w http.ResponseWriter, r *http.Request) {
	if !s.approvalGuard(w, r, true) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": s.approvals.List()})
}

func (s *Server) approvalStatus(w http.ResponseWriter, r *http.Request) {
	if !s.approvalGuard(w, r, false) {
		return
	}
	request, ok := s.approvals.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "approval request not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": request.ID, "status": request.Status, "expires_at": request.ExpiresAt})
}

func (s *Server) approvalDecide(w http.ResponseWriter, r *http.Request) {
	if !s.approvalGuard(w, r, true) {
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "decision requires application/json"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decision, note, err := decodeApprovalDecision(r.Body)
	if err != nil || (decision != "approved" && decision != "denied") || len(note) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decision must contain approved or denied and an optional note of at most 2000 bytes"})
		return
	}
	if _, ok := s.approvals.Get(r.PathValue("id")); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "approval request not found"})
		return
	}
	request, err := s.approvals.Decide(r.PathValue("id"), decision, "operator", note)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval decision could not be applied; refresh the inbox"})
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func decodeApprovalDecision(body io.Reader) (string, string, error) {
	decoder := json.NewDecoder(body)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", "", errors.New("invalid decision")
	}
	values := map[string]string{}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "decision" && key != "note") {
			return "", "", errors.New("invalid decision field")
		}
		if _, duplicate := values[key]; duplicate {
			return "", "", errors.New("duplicate decision field")
		}
		var value any
		if err = decoder.Decode(&value); err != nil {
			return "", "", err
		}
		text, ok := value.(string)
		if !ok {
			return "", "", errors.New("decision field must be a string")
		}
		values[key] = text
	}
	if _, err = decoder.Token(); err != nil {
		return "", "", err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return "", "", errors.New("trailing decision data")
	}
	return values["decision"], values["note"], nil
}

func (s *Server) approvalPage(w http.ResponseWriter, r *http.Request) {
	if !s.approvalGuard(w, r, false) {
		return
	}
	scriptHash := sha256.Sum256([]byte(approvalScript))
	styleHash := sha256.Sum256([]byte(approvalStyle))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(scriptHash[:])+"'; style-src 'sha256-"+base64.StdEncoding.EncodeToString(styleHash[:])+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, approvalHTMLStart+approvalStyle+approvalHTMLMiddle+approvalScript+approvalHTMLEnd)
}

const approvalHTMLStart = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>brw approval inbox</title><style>`
const approvalHTMLMiddle = `</style></head><body><main><header><div><p class="eyebrow">brw · operator</p><h1>Approval inbox</h1></div><span id="count" class="count">Locked</span></header><p class="intro">Review the exact action before it proceeds. Approval permits one retry of that action; the agent must retry to execute it.</p><section id="login" class="panel"><h2>Unlock inbox</h2><p>Use the separate operator token configured for this daemon. It stays in this page’s memory until you lock or close it.</p><form id="unlock"><label for="token">Operator token</label><div class="unlock-row"><input id="token" type="password" autocomplete="off" spellcheck="false" required minlength="32"><button type="submit">Unlock</button></div></form><p class="hint">Authentication uses the Authorization: Bearer header. Tokens are never accepted in URLs or cookies.</p></section><section id="inbox" hidden><div class="toolbar"><button id="refresh" type="button">Refresh</button><button id="notifications" type="button">Enable notifications</button><button id="lock" type="button">Lock inbox</button></div><p id="updated" class="hint"></p><div id="requests"></div></section><p id="message" role="status" aria-live="polite"></p><footer>Approval does not execute an action or reverse a completed action. Expired, changed, rejected, and consumed requests cannot be approved again.</footer></main><script>`
const approvalHTMLEnd = `</script></body></html>`
const approvalStyle = `:root{color-scheme:dark;font-family:ui-sans-serif,system-ui,sans-serif;background:#0b0f14;color:#e7edf5}*{box-sizing:border-box}body{margin:0}main{max-width:960px;margin:auto;padding:36px 24px}header{display:flex;align-items:center;justify-content:space-between;gap:16px}h1{font-size:clamp(26px,5vw,36px);margin:0}h2{font-size:20px;margin-top:0}.eyebrow{color:#8ea4ba;letter-spacing:.09em;text-transform:uppercase;font-size:12px}.intro{max-width:720px;line-height:1.6;color:#b4c2d0}.panel,.card{background:#121a24;border:1px solid #283544;border-radius:14px;padding:24px;margin:22px 0}.count,.state{border:1px solid #33475d;background:#172737;border-radius:999px;padding:8px 14px;white-space:nowrap}.toolbar,.unlock-row,.actions{display:flex;gap:12px;flex-wrap:wrap;align-items:center}label{display:block;font-weight:600;margin:12px 0 8px}input,textarea,button{font:inherit;border-radius:8px;border:1px solid #3c536a;padding:12px;color:inherit}input,textarea{background:#0b1119;width:100%}.unlock-row input{flex:1;min-width:180px}button{background:#243b51;cursor:pointer;min-height:44px}button:hover{background:#304d69}button:disabled{opacity:.5;cursor:default}button:focus-visible,input:focus-visible,textarea:focus-visible{outline:3px solid #89c4ff;outline-offset:3px}.approve{background:#164a3c}.reject{background:#4c2631}.hint,footer{color:#97a9bb;font-size:13px;line-height:1.6}footer{margin-top:32px}.card-top{display:flex;align-items:start;justify-content:space-between;gap:12px}.card-top h2{overflow-wrap:anywhere}.summary{line-height:1.5;white-space:pre-wrap;overflow-wrap:anywhere}dl{display:grid;grid-template-columns:110px 1fr;gap:10px;font-size:14px}dt{color:#97a9bb}dd{margin:0;overflow-wrap:anywhere}pre{font-size:13px;background:#0b1119;padding:16px;border:1px solid #283544;border-radius:8px;white-space:pre-wrap;overflow-wrap:anywhere;max-height:360px;overflow:auto}.state.pending{color:#ffe2a1}.state.approved{color:#91e0ba}.state.denied,.state.stale,.state.expired{color:#ffb8bb}.empty{padding:24px;border:1px dashed #33475d;border-radius:12px;text-align:center;color:#b4c2d0}[hidden]{display:none!important}#message{min-height:24px;line-height:1.5}@media(max-width:600px){main{padding:20px 16px}.panel,.card{padding:18px}dl{grid-template-columns:1fr;gap:5px}dd{margin-bottom:10px}.card-top{flex-wrap:wrap}.toolbar button{flex:1}.actions button{flex:1}.count{font-size:13px;padding:7px 10px}}`
const approvalScript = `(()=>{
'use strict';
const el=id=>document.getElementById(id);
let token='',generation=0,busy=false,timer=0,controller=null,notify=false,seen=new Set(),baseline=false,drafts=new Map(),expanded=new Set(),lastRender='';
const message=text=>{el('message').textContent=text;};
function schedule(){clearTimeout(timer);if(token&&!document.hidden)timer=setTimeout(refresh,3000);}
function lock(text='Inbox locked.'){generation++;token='';busy=false;clearTimeout(timer);if(controller)controller.abort();controller=null;seen.clear();baseline=false;drafts.clear();expanded.clear();lastRender='';el('token').value='';el('requests').replaceChildren();el('inbox').hidden=true;el('login').hidden=false;el('count').textContent='Locked';message(text);el('token').focus();}
async function api(path,options={}){const authGeneration=generation;const response=await fetch(path,{...options,headers:{'Authorization':'Bearer '+token,...options.headers},credentials:'omit',cache:'no-store',redirect:'error',signal:controller.signal});if(authGeneration!==generation)throw new DOMException('Inbox locked.','AbortError');if(response.status===401){lock('Token rejected. Unlock with the operator token.');throw new Error('Authentication required.');}const data=await response.json();if(!response.ok)throw new Error(data.error||'Request failed.');return data;}
function node(tag,text,className){const result=document.createElement(tag);if(text!==undefined)result.textContent=String(text);if(className)result.className=className;return result;}
function pair(list,label,value){list.append(node('dt',label),node('dd',value||'—'));}
function render(requests){el('updated').textContent='Updated '+new Date().toLocaleTimeString();const signature=JSON.stringify(requests);if(signature===lastRender)return;lastRender=signature;const focused=document.activeElement;const focusID=focused&&focused.id;const selection=focused&&focused.tagName==='TEXTAREA'?[focused.selectionStart,focused.selectionEnd]:null;const list=el('requests');list.replaceChildren();let pending=0;const now=Date.now();for(const request of requests){const status=request.status==='pending'&&Date.parse(request.expires_at)<=now?'expired':request.status;if(status==='pending')pending++;const card=node('article',undefined,'card');const top=node('div',undefined,'card-top');top.append(node('h2',request.tool),node('span',status,'state '+status));card.append(top,node('p',request.summary,'summary'));const details=node('dl');pair(details,'Site',request.origin);pair(details,'Tab',request.tab_id);pair(details,'Session',request.session_id);pair(details,'Request',request.id);pair(details,'Expires',new Date(request.expires_at).toLocaleString());card.append(details);const argumentsBox=node('details');argumentsBox.open=expanded.has(request.id);argumentsBox.addEventListener('toggle',()=>{if(argumentsBox.open)expanded.add(request.id);else expanded.delete(request.id);});argumentsBox.append(node('summary','Exact arguments'),node('pre',JSON.stringify(request.arguments,null,2)));card.append(argumentsBox);if(request.decision_note)card.append(node('p','Decision note: '+request.decision_note,'summary'));if(status==='pending'){const label=node('label','Decision note (optional)');const note=node('textarea');note.rows=2;note.maxLength=2000;note.id='note-'+request.id;note.value=drafts.get(request.id)||'';note.addEventListener('input',()=>drafts.set(request.id,note.value));label.htmlFor=note.id;card.append(label,note);const actions=node('div',undefined,'actions');for(const [decision,title,className] of [['approved','Approve once','approve'],['denied','Reject','reject']]){const button=node('button',title,className);button.type='button';button.id='decision-'+request.id+'-'+decision;button.addEventListener('click',()=>decide(request.id,decision,note.value,card));actions.append(button);}card.append(actions);}else{const labels={approved:'Approved for one exact retry; awaiting the agent.',denied:'Rejected by the operator.',consumed:'Approval already used.',expired:'Request expired. A fresh request is required.',stale:'The action or browser state changed. A fresh request is required.'};card.append(node('p',labels[status]||'This request is no longer pending.','hint'));}list.append(card);}
el('count').textContent=pending+' pending';if(!requests.length)list.append(node('p','No requests. New approval requests will appear here.','empty'));const incoming=requests.filter(r=>r.status==='pending'&&!seen.has(r.id));if(notify&&baseline&&incoming.length&&'Notification'in window&&Notification.permission==='granted')new Notification('brw approval inbox',{body:incoming.length+' new request'+(incoming.length===1?'':'s')+' awaiting your review.'});seen=new Set(requests.map(r=>r.id));baseline=true;if(focusID){const target=el(focusID);if(target){target.focus({preventScroll:true});if(selection)target.setSelectionRange(...selection);}}}
async function refresh(){if(!token||document.hidden||busy)return;schedule();busy=true;controller=new AbortController();const current=generation;try{const data=await api('/operator/approvals');if(current!==generation)return;render(data.requests||[]);el('login').hidden=true;el('inbox').hidden=false;message('');}catch(error){if(current===generation&&error.name!=='AbortError')message('Could not refresh: '+error.message);}finally{if(current===generation){busy=false;schedule();}}}
async function decide(id,decision,note,card){if(!token||busy)return;clearTimeout(timer);busy=true;controller=new AbortController();const current=generation;const buttons=card.querySelectorAll('button');buttons.forEach(b=>b.disabled=true);let succeeded=false;try{await api('/operator/approvals/'+encodeURIComponent(id)+'/decision',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({decision,note})});if(current!==generation)return;succeeded=true;drafts.delete(id);message(decision==='approved'?'Approved once. The agent must retry the exact action.':'Request rejected.');}catch(error){if(current===generation&&error.name!=='AbortError')message('Decision was not applied: '+error.message);}finally{if(current===generation){busy=false;buttons.forEach(b=>b.disabled=false);if(succeeded)await refresh();else schedule();}}}
el('unlock').addEventListener('submit',event=>{event.preventDefault();if(busy)return;token=el('token').value;el('token').value='';generation++;seen.clear();baseline=false;el('login').hidden=true;el('inbox').hidden=false;el('count').textContent='Loading…';message('Unlocking…');refresh();});
el('lock').addEventListener('click',()=>lock());el('refresh').addEventListener('click',refresh);
el('notifications').addEventListener('click',async()=>{if(!('Notification'in window)){message('Browser notifications are unavailable.');return;}const permission=await Notification.requestPermission();notify=permission==='granted';el('notifications').textContent=notify?'Notifications enabled':'Enable notifications';el('notifications').disabled=notify;message(notify?'Notifications enabled for new requests while this inbox is unlocked.':'Notifications were not enabled.');});
document.addEventListener('visibilitychange',()=>{clearTimeout(timer);if(!document.hidden)refresh();});
window.addEventListener('pagehide',()=>lock());
})();`
