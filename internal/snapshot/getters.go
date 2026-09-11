package snapshot

import (
	"encoding/json"
	"fmt"
)

// GetScript answers a single typed question about the page or one element.
//
// It exists so the eleven separate "get X" / "is X" reads an agent needs
// (text, title, url, value, box, attr, styles, count, visible, enabled,
// checked) are one round trip and one tool rather than eleven, and so none of
// them requires hand-written JavaScript through brw_evaluate. Element lookup is
// frame-aware: a ref or selector resolves across same-origin iframes and open
// shadow roots, which is where ad-hoc document.querySelector silently fails.
const GetScript = `(function(what, target, name){` + FrameWalkHelpers + `
  function roots(){ return __abRootList(); }
  function resolve(sel){
    if(!sel) return null;
    var refSelector = '[data-brw-ref="' + (window.CSS && CSS.escape ? CSS.escape(sel) : sel) + '"]';
    var list = roots();
    for(var i=0;i<list.length;i++){
      var root = list[i];
      if(!root || !root.querySelector) continue;
      var byRef = null;
      try { byRef = root.querySelector(refSelector); } catch(e){}
      if(byRef) return byRef;
    }
    for(var j=0;j<list.length;j++){
      var r2 = list[j];
      if(!r2 || !r2.querySelector) continue;
      try { var el = r2.querySelector(sel); if(el) return el; } catch(e){}
    }
    return null;
  }
  function all(sel){
    var out = [];
    var list = roots();
    for(var i=0;i<list.length;i++){
      var root = list[i];
      if(!root || !root.querySelectorAll) continue;
      try { Array.prototype.push.apply(out, root.querySelectorAll(sel)); } catch(e){}
    }
    return out;
  }
  function visible(el){
    if(!el) return false;
    var style = window.getComputedStyle(el);
    if(!style || style.visibility === 'hidden' || style.display === 'none') return false;
    if(parseFloat(style.opacity || '1') === 0) return false;
    var rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  }
  function need(){
    var el = resolve(target);
    if(!el) throw new Error('no element matched ' + JSON.stringify(target));
    return el;
  }

  switch(what){
    case 'url':   return {value: location.href};
    case 'title': return {value: document.title};
    case 'text':
      if(!target) return {value: document.body ? document.body.innerText : ''};
      return {value: need().innerText};
    case 'value': {
      var el = need();
      if(el.type === 'checkbox' || el.type === 'radio') return {value: el.checked ? 'on' : ''};
      return {value: el.value === undefined ? '' : String(el.value)};
    }
    case 'attr':  return {value: need().getAttribute(name)};
    case 'count': return {value: all(target).length};
    case 'box': {
      var rect = need().getBoundingClientRect();
      return {value: {x: rect.x, y: rect.y, width: rect.width, height: rect.height,
                      top: rect.top, left: rect.left, bottom: rect.bottom, right: rect.right}};
    }
    case 'styles': {
      var computed = window.getComputedStyle(need());
      if(name){ return {value: computed.getPropertyValue(name)}; }
      // Without a named property, return the properties that actually explain
      // layout and appearance rather than the full ~340-property dump.
      var keys = ['display','visibility','opacity','position','color','background-color',
                  'font-size','font-weight','width','height','margin','padding','border',
                  'z-index','overflow','flex-direction','justify-content','align-items'];
      var out = {};
      for(var i=0;i<keys.length;i++) out[keys[i]] = computed.getPropertyValue(keys[i]);
      return {value: out};
    }
    case 'visible': return {value: visible(resolve(target))};
    case 'hidden':  return {value: !visible(resolve(target))};
    case 'enabled': { var e1 = resolve(target); return {value: !!e1 && !e1.disabled}; }
    case 'disabled':{ var e2 = resolve(target); return {value: !!e2 && !!e2.disabled}; }
    case 'checked': { var e3 = resolve(target); return {value: !!e3 && !!e3.checked}; }
    default: throw new Error('unknown get target ' + JSON.stringify(what));
  }
})`

// BuildGetExpression renders GetScript for one question.
func BuildGetExpression(what, target, name string) string {
	whatJSON, _ := json.Marshal(what)
	targetJSON, _ := json.Marshal(target)
	nameJSON, _ := json.Marshal(name)
	return fmt.Sprintf("%s(%s,%s,%s)", GetScript, whatJSON, targetJSON, nameJSON)
}
