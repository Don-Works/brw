package snapshot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The ARIA structure brw compares between runs.
//
// This is deliberately NOT the platform accessibility tree. It is the role and
// accessible name of every element the page gives a role to, nested in document
// order with untyped wrapper elements flattened away. That is what makes it
// useful as a regression baseline: it carries no geometry, so a CSS refactor
// that swaps three nested divs for a grid does not move it and neither does a
// repaint, while a button that loses its accessible name does — which is
// exactly the regression a pixel diff is worst at seeing, because a name lives
// in an attribute rather than in ink.
//
// It is computed in the page so every transport can produce it: the extension
// bridge has no browser-level Accessibility domain, and a baseline that only
// worked on one transport would be a gate half the deployments could not use.

// AriaNode is one node of that structure.
type AriaNode struct {
	Role     string     `json:"role"`
	Name     string     `json:"name,omitempty"`
	Children []AriaNode `json:"children,omitempty"`
}

// AriaTree is the whole structure for one page state.
type AriaTree struct {
	Nodes []AriaNode `json:"nodes"`
	// Truncated reports that the node cap was hit, so a diff over this tree
	// covers a prefix of the page rather than all of it.
	Truncated bool `json:"truncated,omitempty"`
}

// Empty reports a tree with nothing to compare.
func (t AriaTree) Empty() bool { return len(t.Nodes) == 0 }

// Count is the number of nodes in the tree.
func (t AriaTree) Count() int { return countAriaNodes(t.Nodes) }

func countAriaNodes(nodes []AriaNode) int {
	total := 0
	for _, node := range nodes {
		total += 1 + countAriaNodes(node.Children)
	}
	return total
}

// AriaTreeExpression returns the ARIA structure of the current document. It is
// an expression, not a function declaration, so it can go through the ordinary
// evaluate path on every transport.
const AriaTreeExpression = `(function(){
  var MAX_NODES = 2000;
  var MAX_DEPTH = 40;
  var count = 0;
  var truncated = false;
  function clean(s){ return String(s == null ? '' : s).replace(/\s+/g, ' ').trim().slice(0, 200); }
  function labelText(el){
    if (el.labels && el.labels.length) {
      return clean(Array.prototype.map.call(el.labels, function(l){ return l.innerText || l.textContent; }).join(' '));
    }
    var parent = el.closest ? el.closest('label') : null;
    if (parent) return clean(parent.innerText || parent.textContent);
    return '';
  }
  function ownText(el){
    var tag = el.tagName.toLowerCase();
    if (tag === 'input') return '';
    var text = '';
    for (var i = 0; i < el.childNodes.length; i++) {
      var child = el.childNodes[i];
      if (child.nodeType === 3) text += child.nodeValue;
    }
    text = clean(text);
    if (text) return text;
    // A control whose whole content is its label (a button wrapping a span)
    // still has one accessible name; fall back to the rendered text for the
    // roles where that is how a name is normally given.
    if (tag === 'button' || tag === 'a' || tag === 'summary' || tag === 'label') {
      return clean(el.innerText || el.textContent);
    }
    return '';
  }
  function nameFor(el){
    var explicit = clean(el.getAttribute('aria-label'));
    if (explicit) return explicit;
    var labelled = clean(el.getAttribute('aria-labelledby'));
    if (labelled) {
      var parts = [];
      labelled.split(/\s+/).forEach(function(id){
        var target = document.getElementById(id);
        if (target) parts.push(clean(target.innerText || target.textContent));
      });
      var joined = clean(parts.join(' '));
      if (joined) return joined;
    }
    var fromLabel = labelText(el);
    if (fromLabel) return fromLabel;
    var tag = el.tagName.toLowerCase();
    if (tag === 'input' && (el.getAttribute('type') || '').toLowerCase() === 'password') return '';
    return clean(el.getAttribute('alt') || el.getAttribute('title') || el.getAttribute('placeholder') || ownText(el));
  }
  function roleFor(el){
    var explicit = clean(el.getAttribute('role'));
    if (explicit) return explicit.split(' ')[0];
    var tag = el.tagName.toLowerCase();
    var type = (el.getAttribute('type') || '').toLowerCase();
    if (tag === 'a' && el.hasAttribute('href')) return 'link';
    if (tag === 'button' || type === 'button' || type === 'submit' || type === 'reset') return 'button';
    if (tag === 'textarea' || el.isContentEditable) return 'textbox';
    if (tag === 'select') return el.multiple ? 'listbox' : 'combobox';
    if (tag === 'input') {
      if (type === 'hidden') return '';
      if (type === 'checkbox') return 'checkbox';
      if (type === 'radio') return 'radio';
      if (type === 'range') return 'slider';
      if (type === 'number') return 'spinbutton';
      if (type === 'search') return 'searchbox';
      return 'textbox';
    }
    if (tag === 'img') return 'image';
    if (tag === 'summary') return 'button';
    if (/^h[1-6]$/.test(tag)) return 'heading';
    if (tag === 'main') return 'main';
    if (tag === 'nav') return 'navigation';
    if (tag === 'header') return 'banner';
    if (tag === 'footer') return 'contentinfo';
    if (tag === 'aside') return 'complementary';
    if (tag === 'form') return 'form';
    if (tag === 'table') return 'table';
    if (tag === 'tr') return 'row';
    if (tag === 'td' || tag === 'th') return 'cell';
    if (tag === 'ul' || tag === 'ol') return 'list';
    if (tag === 'li') return 'listitem';
    if (tag === 'dialog') return 'dialog';
    if (tag === 'option') return 'option';
    return '';
  }
  function hidden(el){
    if (el.getAttribute('aria-hidden') === 'true') return true;
    if (el.hasAttribute('hidden')) return true;
    var style = el.ownerDocument.defaultView.getComputedStyle(el);
    return !style || style.display === 'none' || style.visibility === 'hidden';
  }
  function walk(el, depth){
    var out = [];
    if (depth > MAX_DEPTH) { truncated = true; return out; }
    for (var i = 0; i < el.children.length; i++) {
      var child = el.children[i];
      var tag = child.tagName.toLowerCase();
      if (tag === 'script' || tag === 'style' || tag === 'template' || tag === 'noscript') continue;
      if (hidden(child)) continue;
      var role = roleFor(child);
      if (!role) {
        // An untyped wrapper contributes no structure: splice its children in
        // so a purely presentational nesting change is not a regression.
        var inner = walk(child, depth + 1);
        for (var j = 0; j < inner.length; j++) out.push(inner[j]);
        continue;
      }
      if (count >= MAX_NODES) { truncated = true; return out; }
      count++;
      var node = { role: role };
      var name = nameFor(child);
      if (name) node.name = name;
      var kids = walk(child, depth + 1);
      if (kids.length) node.children = kids;
      out.push(node);
    }
    return out;
  }
  var root = document.body || document.documentElement;
  var nodes = root ? walk(root, 0) : [];
  return { nodes: nodes, truncated: truncated };
})()`

// ParseAriaTree decodes what AriaTreeExpression returned. The evaluate path
// hands back an already-decoded any, so this re-encodes once rather than
// duplicating the shape by hand.
func ParseAriaTree(raw any) (AriaTree, error) {
	if raw == nil {
		return AriaTree{}, fmt.Errorf("the page returned no ARIA structure")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return AriaTree{}, err
	}
	var tree AriaTree
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return AriaTree{}, fmt.Errorf("decode ARIA structure: %w", err)
	}
	return tree, nil
}

// Kinds of ARIA change a baseline reports.
const (
	AriaChangeAdded       = "added"
	AriaChangeRemoved     = "removed"
	AriaChangeRoleChanged = "role_changed"
	AriaChangeNameChanged = "name_changed"
)

// AriaChange is one structural difference. Path is the role chain from the
// document down to the node, with the sibling index, so two buttons in a row
// are distinguishable.
type AriaChange struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Role string `json:"role,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// MaxAriaChanges caps the reported list. The count stays exact, so a wholesale
// rewrite is still answerable without enumerating every node.
const MaxAriaChanges = 50

// AriaDiff is the result of comparing two ARIA structures.
type AriaDiff struct {
	Changed bool         `json:"changed"`
	Count   int          `json:"count"`
	Changes []AriaChange `json:"changes,omitempty"`
	// Truncated reports the list was capped; Count remains exact.
	Truncated bool `json:"truncated,omitempty"`
	// Note explains a comparison that could not be trusted, for example a
	// baseline captured from a truncated tree.
	Note string `json:"note,omitempty"`
}

// DiffAriaTrees compares two ARIA structures positionally: siblings are paired
// by index within their parent, so an inserted node shifts the ones after it
// and reports as an insertion plus the shifted differences rather than as an
// unrelated rewrite.
func DiffAriaTrees(before, after AriaTree) AriaDiff {
	var diff AriaDiff
	collector := &ariaCollector{}
	collector.walk(nil, before.Nodes, after.Nodes)
	diff.Count = collector.count
	diff.Changes = collector.changes
	diff.Truncated = collector.count > len(collector.changes)
	diff.Changed = collector.count > 0
	if before.Truncated || after.Truncated {
		diff.Note = "one of the compared ARIA structures hit the node cap, so this comparison covers a prefix of the page"
	}
	return diff
}

type ariaCollector struct {
	count   int
	changes []AriaChange
}

func (c *ariaCollector) add(change AriaChange) {
	c.count++
	if len(c.changes) < MaxAriaChanges {
		c.changes = append(c.changes, change)
	}
}

func (c *ariaCollector) walk(path []string, before, after []AriaNode) {
	longest := len(before)
	if len(after) > longest {
		longest = len(after)
	}
	for i := 0; i < longest; i++ {
		switch {
		case i >= len(after):
			node := before[i]
			c.add(AriaChange{Kind: AriaChangeRemoved, Path: ariaPath(path, node.Role, i), Role: node.Role, From: node.Name})
		case i >= len(before):
			node := after[i]
			c.add(AriaChange{Kind: AriaChangeAdded, Path: ariaPath(path, node.Role, i), Role: node.Role, To: node.Name})
		default:
			oldNode, newNode := before[i], after[i]
			here := ariaPath(path, newNode.Role, i)
			if oldNode.Role != newNode.Role {
				c.add(AriaChange{Kind: AriaChangeRoleChanged, Path: ariaPath(path, oldNode.Role, i), From: oldNode.Role, To: newNode.Role})
			} else if oldNode.Name != newNode.Name {
				c.add(AriaChange{Kind: AriaChangeNameChanged, Path: here, Role: newNode.Role, From: oldNode.Name, To: newNode.Name})
			}
			// A fresh slice per level: appending to the caller's backing array
			// would let one sibling's path segment overwrite the next one's.
			next := make([]string, 0, len(path)+1)
			next = append(next, path...)
			next = append(next, fmt.Sprintf("%s[%d]", newNode.Role, i))
			c.walk(next, oldNode.Children, newNode.Children)
		}
	}
}

func ariaPath(path []string, role string, index int) string {
	parts := make([]string, 0, len(path)+1)
	parts = append(parts, path...)
	parts = append(parts, role+"["+strconv.Itoa(index)+"]")
	return strings.Join(parts, " > ")
}
