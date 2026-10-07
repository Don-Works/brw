package approvalgate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type evidence struct {
	URL     string
	Digest  string
	Preview any
	Target  string
}

func (g *Gate) capture(ctx context.Context, tool string, args map[string]json.RawMessage) (evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ref string
	_ = json.Unmarshal(args["ref"], &ref)
	allowOpaque := ref != "" && !strings.Contains(ref, ":")
	switch tool {
	case "brw_click", "brw_type", "brw_select", "brw_check", "brw_commit", "brw_focus":
	default:
		allowOpaque = false
	}
	settings, _ := json.Marshal(map[string]any{"ref": ref, "allow_opaque_frames": allowOpaque})
	value, err := g.manager.Evaluate(ctx, evidenceScript+"("+string(settings)+")")
	if err != nil {
		return evidence{}, errors.New("approval could not capture current page state; use human takeover")
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 256*1024 {
		return evidence{}, errors.New("approval evidence exceeds its bound; use human takeover")
	}
	var state struct {
		URL      string `json:"url"`
		Complete bool   `json:"complete"`
		Preview  any    `json:"preview"`
		Target   string `json:"target"`
	}
	if json.Unmarshal(encoded, &state) != nil || state.URL == "" || !state.Complete {
		return evidence{}, errors.New("approval could not bind complete page state; use human takeover")
	}
	return evidence{URL: state.URL, Digest: digest(encoded), Preview: state.Preview, Target: state.Target}, nil
}

const evidenceScript = `(settings => {
  const roots = [document];
  const frames = [];
  const controls = [];
  const targets = [];
  const visible = [];
  let complete = true;
  let targetSeen = !settings.ref;
  let targetLabel = '';
  let nodes = 0;
  const walk = root => {
    for (const el of root.querySelectorAll('*')) {
      if (++nodes > 12000) { complete = false; return; }
      if (el.shadowRoot) roots.push(el.shadowRoot);
      if (settings.ref && el.getAttribute('data-brw-ref') === settings.ref) {
        targetSeen = true;
        targetLabel = (el.getAttribute('aria-label') || el.innerText || el.name || el.tagName).trim().slice(0,240);
      }
      if (el.matches('a,button,input,textarea,select,form,[role],[contenteditable="true"]')) {
        const attrs = Array.from(el.attributes).map(a => [a.name,a.value]).filter(a => a[0] !== 'style' && !a[0].startsWith('data-brw-')).sort((a,b) => a[0].localeCompare(b[0]));
        targets.push({tag:el.tagName,attrs:attrs,disabled:!!el.disabled});
      }
      if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
        try {
          if (!el.contentDocument) { frames.push({opaque:true,src:el.src}); if (!settings.allow_opaque_frames) complete = false; continue; }
          frames.push(el.contentWindow.location.href);
          roots.push(el.contentDocument);
        } catch (_) { frames.push({opaque:true,src:el.src}); if (!settings.allow_opaque_frames) complete = false; }
      }
      if (el.matches('input,textarea,select,[contenteditable="true"]')) {
        const label = el.getAttribute('aria-label') || el.name || el.id || el.type || el.tagName;
        const value = el.isContentEditable ? el.innerText : el.value;
        const sensitive = el.type === 'password' || /password|token|secret|card|cvv|cvc|ssn|social.security/i.test(label + ' ' + el.autocomplete);
        controls.push({tag:el.tagName,type:el.type,name:label,value:value,checked:!!el.checked,disabled:!!el.disabled});
        visible.push({field:label,value:sensitive ? '[redacted]' : value,checked:!!el.checked});
      }
    }
  };
  for (let i = 0; i < roots.length; i++) {
    if (roots.length > 100) { complete = false; break; }
    walk(roots[i]);
  }
  const text = document.body ? document.body.innerText : '';
  if (text.length > 64000) complete = false;
  return {url:location.href,complete:complete&&targetSeen,target:targetLabel,time_origin:performance.timeOrigin,frames:frames,
    targets:targets,controls:controls,text:text.slice(0,64000),preview:{title:document.title,fields:visible,text:text.slice(0,16000)}};
})`
