package devtools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/devtools/axe"
)

// Summary bounds. An axe report on a real application is tens of thousands of
// lines; the whole point of the artifact is that the MCP answer stays small
// enough to read. These are the defaults, not the ceiling a caller may ask for.
const (
	DefaultAuditMaxRules = 10
	MaxAuditMaxRules     = 50
	DefaultAuditMaxRefs  = 3
	MaxAuditMaxRefs      = 20
	auditSampleLimit     = 300
)

// impactOrder ranks axe's impact labels worst-first. A summary that listed
// rules in axe's own order would bury a critical failure under three minor ones.
var impactOrder = map[string]int{"critical": 0, "serious": 1, "moderate": 2, "minor": 3}

// AuditTagNames are the axe rule tags worth naming in a tool schema: the WCAG
// conformance levels and the best-practice set. axe accepts many more.
func AuditTagNames() []string {
	return []string{"wcag2a", "wcag2aa", "wcag2aaa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"}
}

type AuditOptions struct {
	// Tags limits the run to axe rules carrying one of these tags. Empty runs
	// every rule axe ships.
	Tags []string `json:"tags,omitempty"`
	// Rules limits the run to these exact axe rule ids, for re-checking one
	// finding after a fix without paying for a whole audit.
	Rules []string `json:"rules,omitempty"`
	// IncludePasses keeps the passing nodes in the stored report. Off by
	// default: on a large page the passes outweigh everything else combined.
	IncludePasses bool   `json:"include_passes,omitempty"`
	MaxRules      int    `json:"max_rules,omitempty"`
	MaxRefs       int    `json:"max_refs,omitempty"`
	TabID         string `json:"tab_id,omitempty"`
}

func (o AuditOptions) Normalize() AuditOptions {
	if o.MaxRules <= 0 {
		o.MaxRules = DefaultAuditMaxRules
	}
	if o.MaxRules > MaxAuditMaxRules {
		o.MaxRules = MaxAuditMaxRules
	}
	if o.MaxRefs <= 0 {
		o.MaxRefs = DefaultAuditMaxRefs
	}
	if o.MaxRefs > MaxAuditMaxRefs {
		o.MaxRefs = MaxAuditMaxRefs
	}
	o.Tags = trimmedList(o.Tags)
	o.Rules = trimmedList(o.Rules)
	return o
}

func trimmedList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AuditRule is one failing axe rule, reduced to what an agent acts on: what is
// wrong, how bad it is, and which elements to go and look at.
type AuditRule struct {
	ID      string   `json:"id"`
	Impact  string   `json:"impact"`
	Help    string   `json:"help"`
	HelpURL string   `json:"help_url,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Nodes   int      `json:"nodes"`
	// Refs are brw refs for the offending elements, usable directly with
	// brw_highlight, brw_click and the rest. Bounded by max_refs.
	Refs []string `json:"refs,omitempty"`
	// Targets are the CSS selectors axe reported, in the same order as Refs,
	// for an element brw could not stamp a ref onto.
	Targets []string `json:"targets,omitempty"`
	Sample  string   `json:"sample,omitempty"`
}

// ArtifactRef is the payload-free handle to the stored full report. It mirrors
// artifact.Meta without importing it: the artifact package already depends on
// browser, and this package sits underneath both.
type ArtifactRef struct {
	ID        string    `json:"artifact_id"`
	MIMEType  string    `json:"mime_type,omitempty"`
	SizeBytes int64     `json:"size_bytes,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// AuditResult is the bounded answer. The complete axe document never travels
// in it: Report is populated by the transport that ran the audit and is moved
// into an artifact by the layer that has a store, then dropped.
type AuditResult struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Engine string `json:"engine"`

	Violations      int            `json:"violations"`
	ViolationNodes  int            `json:"violation_nodes"`
	ByImpact        map[string]int `json:"by_impact,omitempty"`
	Rules           []AuditRule    `json:"rules,omitempty"`
	Incomplete      int            `json:"incomplete"`
	IncompleteNodes int            `json:"incomplete_nodes"`
	Passes          int            `json:"passes"`
	Inapplicable    int            `json:"inapplicable"`
	Truncated       bool           `json:"truncated,omitempty"`

	Artifact *ArtifactRef `json:"artifact,omitempty"`
	Note     string       `json:"note,omitempty"`

	// Report is the complete audit document destined for the artifact store. It
	// is never serialized into a response.
	Report json.RawMessage `json:"-"`
}

// AxeProbeScript reports whether a usable axe is already in the document, and
// whether brw is the one that put it there.
const AxeProbeScript = `(function() {
  var present = !!(window.axe && typeof window.axe.run === 'function');
  return {
    present: present,
    version: present ? String(window.axe.version || '') : '',
    ours: present && window.__brwAxeInstalled === true
  };
})()`

// AxeInstallExpression wraps the embedded bundle so evaluating it returns a
// small confirmation instead of whatever the UMD wrapper's completion value
// happens to be. The bundle takes the global explicitly, so nesting it inside
// another function changes nothing about how it installs itself.
func AxeInstallExpression() string {
	return `(function() {
` + axe.Source + `
  try { window.__brwAxeInstalled = true; } catch (_) {}
  return { installed: !!(window.axe && typeof window.axe.run === 'function'), version: String((window.axe && window.axe.version) || '') };
})()`
}

// BuildAuditExpression renders one axe run for already-normalized options.
func BuildAuditExpression(opts AuditOptions) string {
	args, _ := json.Marshal(map[string]any{
		"tags":           opts.Tags,
		"rules":          opts.Rules,
		"include_passes": opts.IncludePasses,
	})
	return fmt.Sprintf("%s(%s)", AuditScript, args)
}

// AuditScript runs the installed axe engine and stamps a brw ref onto every
// element a rule failed on, so the answer names elements the rest of the tool
// surface can act on rather than CSS selectors an agent has to re-resolve.
//
// Stamping reuses the data-brw-ref attribute brw_snapshot already writes, and
// the same window.__brw counter, so a ref minted here is the same ref a later
// snapshot reports for that element. That attribute is the audit's only effect
// on the page.
const AuditScript = `(function(opts) {
  if (!window.axe || typeof window.axe.run !== 'function') {
    return Promise.resolve({ ok: false, error: 'axe-core is not installed in this document' });
  }
  var state = window.__brw || (window.__brw = { next: 1, byKey: {}, byRef: {} });
  function refFor(el) {
    if (!el || !el.getAttribute) return '';
    try {
      var existing = el.getAttribute('data-brw-ref');
      if (existing) return existing;
      var ref = 'e' + state.next++;
      el.setAttribute('data-brw-ref', ref);
      return ref;
    } catch (_) { return ''; }
  }
  // axe target entries are a chain: one selector per nested same-origin frame,
  // and an array of selectors for a shadow-root descent.
  function resolveTarget(target) {
    if (!target) return null;
    var chain = Array.isArray(target) ? target : [target];
    try {
      var scope = document;
      var el = null;
      for (var i = 0; i < chain.length; i++) {
        var step = chain[i];
        if (Array.isArray(step)) {
          var ctx = scope;
          el = null;
          for (var j = 0; j < step.length; j++) {
            el = ctx.querySelector(step[j]);
            if (!el) return null;
            ctx = el.shadowRoot || el;
          }
        } else {
          el = scope.querySelector(step);
        }
        if (!el) return null;
        if (i < chain.length - 1) {
          scope = el.contentDocument;
          if (!scope) return null;
        }
      }
      return el;
    } catch (_) { return null; }
  }
  function annotate(rules) {
    if (!rules || !rules.length) return;
    for (var i = 0; i < rules.length; i++) {
      var nodes = rules[i].nodes || [];
      for (var j = 0; j < nodes.length; j++) {
        nodes[j].brw_ref = refFor(resolveTarget(nodes[j].target));
      }
    }
  }
  var runOptions = {
    resultTypes: opts.include_passes ? ['violations', 'incomplete', 'passes'] : ['violations', 'incomplete']
  };
  if (opts.rules && opts.rules.length) runOptions.runOnly = { type: 'rule', values: opts.rules };
  else if (opts.tags && opts.tags.length) runOptions.runOnly = { type: 'tag', values: opts.tags };
  return window.axe.run(document, runOptions).then(function(results) {
    annotate(results.violations);
    annotate(results.incomplete);
    return {
      ok: true,
      engine: 'axe-core ' + String(window.axe.version || ''),
      ours: window.__brwAxeInstalled === true,
      url: location.href,
      title: document.title || '',
      report: results
    };
  }).catch(function(e) {
    return { ok: false, error: String((e && e.message) || e) };
  });
})`

// RawAudit is the script's answer before summarization.
type RawAudit struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Engine string          `json:"engine"`
	Ours   bool            `json:"ours"`
	URL    string          `json:"url"`
	Title  string          `json:"title"`
	Report json.RawMessage `json:"report"`
}

// axeReport is the slice of the axe document the summary reads.
type axeReport struct {
	Violations   []axeRule `json:"violations"`
	Incomplete   []axeRule `json:"incomplete"`
	Passes       []axeRule `json:"passes"`
	Inapplicable []axeRule `json:"inapplicable"`
}

type axeRule struct {
	ID      string    `json:"id"`
	Impact  string    `json:"impact"`
	Help    string    `json:"help"`
	HelpURL string    `json:"helpUrl"`
	Tags    []string  `json:"tags"`
	Nodes   []axeNode `json:"nodes"`
}

type axeNode struct {
	Impact         string          `json:"impact"`
	Target         json.RawMessage `json:"target"`
	FailureSummary string          `json:"failureSummary"`
	Ref            string          `json:"brw_ref"`
}

// SummarizeAudit reduces one axe document to a bounded answer and packages the
// complete document for the artifact store. Both transports call it, so the
// summary cannot mean different things over CDP and over the extension bridge.
func SummarizeAudit(raw RawAudit, opts AuditOptions, now time.Time) (AuditResult, error) {
	if !raw.OK {
		message := strings.TrimSpace(raw.Error)
		if message == "" {
			message = "accessibility audit failed in the page"
		}
		return AuditResult{}, fmt.Errorf("accessibility audit: %s", message)
	}
	opts = opts.Normalize()
	var report axeReport
	if err := json.Unmarshal(raw.Report, &report); err != nil {
		return AuditResult{}, fmt.Errorf("decode accessibility report: %w", err)
	}

	result := AuditResult{
		URL:          raw.URL,
		Title:        raw.Title,
		Engine:       strings.TrimSpace(raw.Engine),
		Violations:   len(report.Violations),
		Incomplete:   len(report.Incomplete),
		Passes:       len(report.Passes),
		Inapplicable: len(report.Inapplicable),
		ByImpact:     map[string]int{},
	}
	for _, rule := range report.Incomplete {
		result.IncompleteNodes += len(rule.Nodes)
	}

	rules := make([]AuditRule, 0, len(report.Violations))
	for _, rule := range report.Violations {
		result.ViolationNodes += len(rule.Nodes)
		impact := impactOf(rule)
		if impact != "" {
			result.ByImpact[impact] += len(rule.Nodes)
		}
		summarized := AuditRule{
			ID:      rule.ID,
			Impact:  impact,
			Help:    rule.Help,
			HelpURL: rule.HelpURL,
			Tags:    rule.Tags,
			Nodes:   len(rule.Nodes),
		}
		for _, node := range rule.Nodes {
			if len(summarized.Refs) >= opts.MaxRefs {
				break
			}
			summarized.Refs = append(summarized.Refs, node.Ref)
			summarized.Targets = append(summarized.Targets, flattenTarget(node.Target))
			if summarized.Sample == "" {
				summarized.Sample = firstLine(node.FailureSummary)
			}
		}
		rules = append(rules, summarized)
	}
	// Worst impact first, then most nodes, then rule id: an agent reading only
	// the first entry should be reading the worst thing on the page.
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if rankImpact(a.Impact) != rankImpact(b.Impact) {
			return rankImpact(a.Impact) < rankImpact(b.Impact)
		}
		if a.Nodes != b.Nodes {
			return a.Nodes > b.Nodes
		}
		return a.ID < b.ID
	})
	if len(rules) > opts.MaxRules {
		rules = rules[:opts.MaxRules]
		result.Truncated = true
	}
	result.Rules = rules
	if len(result.ByImpact) == 0 {
		result.ByImpact = nil
	}
	if !raw.Ours {
		result.Note = "audited with the page's own axe-core, not the copy embedded in brw"
	}

	document, err := json.Marshal(struct {
		URL         string          `json:"url"`
		Title       string          `json:"title"`
		Engine      string          `json:"engine"`
		GeneratedAt time.Time       `json:"generated_at"`
		IncludePass bool            `json:"includes_passing_nodes"`
		AxeRunOnly  []string        `json:"run_only,omitempty"`
		AxeRunRules []string        `json:"run_rules,omitempty"`
		Report      json.RawMessage `json:"report"`
	}{
		URL:         raw.URL,
		Title:       raw.Title,
		Engine:      result.Engine,
		GeneratedAt: now.UTC(),
		IncludePass: opts.IncludePasses,
		AxeRunOnly:  opts.Tags,
		AxeRunRules: opts.Rules,
		Report:      raw.Report,
	})
	if err != nil {
		return AuditResult{}, fmt.Errorf("encode accessibility report: %w", err)
	}
	result.Report = document
	return result, nil
}

// impactOf prefers the rule's own impact and falls back to the worst impact any
// of its nodes carries, because axe leaves the rule-level field empty on some
// rules whose severity depends on what was found.
func impactOf(rule axeRule) string {
	if rule.Impact != "" {
		return rule.Impact
	}
	worst := ""
	for _, node := range rule.Nodes {
		if node.Impact == "" {
			continue
		}
		if worst == "" || rankImpact(node.Impact) < rankImpact(worst) {
			worst = node.Impact
		}
	}
	return worst
}

func rankImpact(impact string) int {
	if rank, ok := impactOrder[impact]; ok {
		return rank
	}
	return len(impactOrder)
}

// flattenTarget renders an axe target chain as one readable selector. Frame and
// shadow hops are joined with " >> ", the notation axe itself prints.
func flattenTarget(target json.RawMessage) string {
	if len(target) == 0 {
		return ""
	}
	var one string
	if err := json.Unmarshal(target, &one); err == nil {
		return one
	}
	var many []json.RawMessage
	if err := json.Unmarshal(target, &many); err != nil {
		return ""
	}
	parts := make([]string, 0, len(many))
	for _, item := range many {
		parts = append(parts, flattenTarget(item))
	}
	return strings.Join(parts, " >> ")
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = strings.TrimSpace(text[:index])
	}
	if len(text) > auditSampleLimit {
		text = text[:auditSampleLimit]
	}
	return text
}
