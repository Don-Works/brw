package snapshot

type PageSnapshot struct {
	URL           string                 `json:"url"`
	Title         string                 `json:"title"`
	Elements      []Element              `json:"elements"`
	Accessibility AccessibilitySummary   `json:"accessibility"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

type SnapshotOptions struct {
	Mode          string `json:"mode,omitempty"`
	Query         string `json:"query,omitempty"`
	Text          string `json:"text,omitempty"`
	Role          string `json:"role,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	ViewportOnly  bool   `json:"viewport_only,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
	IncludeAX     bool   `json:"include_ax,omitempty"`
	// TextContent, when true, expands the search haystack to include each
	// element's full visible text (innerText) so queries can match prose that is
	// not otherwise captured by the element's name/attributes. Opt-in to avoid
	// bloating normal ref-based output.
	TextContent bool  `json:"text_content,omitempty"`
	Since       int64 `json:"since,omitempty"`
	// VisualIslands, when true, detects semantically-opaque visual content
	// (canvas/svg/video/large image/background-image/custom-rendered widget) and
	// emits them as first-class Element entries carrying source:["visual"], a
	// visual_type, and a visual_hint. Off by default for backward compatibility;
	// when on, islands compete with DOM elements in the merged element list up to
	// the limit so dense pages stay token-efficient.
	VisualIslands bool `json:"visual_islands,omitempty"`
	// VisualIslandsLimit caps how many of the highest-scored islands are detected
	// before they are merged into the element list. Defaults to 10.
	VisualIslandsLimit int `json:"visual_islands_limit,omitempty"`
}

type FindOptions struct {
	Query         string `json:"query,omitempty"`
	Text          string `json:"text,omitempty"`
	Role          string `json:"role,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	ViewportOnly  bool   `json:"viewport_only,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
	// TextContent enables matching against full visible text content (innerText)
	// in addition to element name/attributes — lets brw_find locate visible
	// prose, not just interactive-element metadata. Opt-in.
	TextContent bool `json:"text_content,omitempty"`
}

type FindResult struct {
	URL      string                 `json:"url"`
	Title    string                 `json:"title"`
	Elements []Element              `json:"elements"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type FillOptions struct {
	Ref     string `json:"ref,omitempty"`
	Query   string `json:"query,omitempty"`
	Role    string `json:"role,omitempty"`
	Text    string `json:"text"`
	Replace bool   `json:"replace"`
}

type UploadOptions struct {
	Ref   string   `json:"ref,omitempty"`
	Query string   `json:"query,omitempty"`
	Role  string   `json:"role,omitempty"`
	Path  string   `json:"path,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

type ClickTextOptions struct {
	Text  string `json:"text"`
	Role  string `json:"role,omitempty"`
	Exact bool   `json:"exact,omitempty"`
	// AutoScroll controls whether a below-fold text match is scrolled into view
	// before clicking. Defaults to true (omit the field) so a link beneath the
	// fold is found and clicked without a manual scroll; set false to opt out and
	// click only elements already in the viewport.
	AutoScroll *bool `json:"auto_scroll,omitempty"`
}

type Element struct {
	Ref           string   `json:"ref"`
	Role          string   `json:"role"`
	Name          string   `json:"name"`
	Tag           string   `json:"tag"`
	Type          string   `json:"type,omitempty"`
	Href          string   `json:"href,omitempty"`
	Value         string   `json:"value,omitempty"`
	Sensitive     bool     `json:"sensitive,omitempty"`
	Visible       bool     `json:"visible"`
	InViewport    bool     `json:"in_viewport"`
	Disabled      bool     `json:"disabled"`
	Required      bool     `json:"required,omitempty"`
	Valid         *bool    `json:"valid,omitempty"`
	ValidationMsg string   `json:"validation_message,omitempty"`
	Checked       *bool    `json:"checked,omitempty"`
	Selected      *bool    `json:"selected,omitempty"`
	Expanded      *bool    `json:"expanded,omitempty"`
	Controls      string   `json:"controls,omitempty"`
	Signals       []string `json:"signals,omitempty"`
	MatchReasons  []string `json:"match_reasons,omitempty"`
	Source        []string `json:"source"`
	// VisualType classifies a visual island when source includes "visual":
	// "canvas"|"svg"|"video"|"image"|"bg_image"|"custom". Empty for DOM elements.
	VisualType string `json:"visual_type,omitempty"`
	// VisualHint carries an alt/aria-label/title/content hint for the visual
	// (image alt text, canvas title, etc.) so the model has a textual anchor for
	// otherwise-opaque painted content.
	VisualHint string `json:"visual_hint,omitempty"`
	Key        string `json:"-"`
}

type AccessibilitySummary struct {
	// Requested distinguishes "accessibility was never asked for" (the default,
	// include_ax=false) from "accessibility was requested but the fetch failed".
	// Without it, the Available:false zero value is semantically overloaded and
	// misleads agents into thinking an AX fetch failed when it was simply not
	// opt-in.
	Requested            bool           `json:"requested"`
	Available            bool           `json:"available"`
	NodeCount            int            `json:"node_count"`
	InteractiveNodeCount int            `json:"interactive_node_count"`
	Roles                map[string]int `json:"roles,omitempty"`
	Error                string         `json:"error,omitempty"`
}

type ElementBox struct {
	OK        bool    `json:"ok"`
	Ref       string  `json:"ref"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	ViewportX float64 `json:"viewport_x"`
	ViewportY float64 `json:"viewport_y"`
}

type ScrollResult struct {
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
	Target     string  `json:"target,omitempty"`
	Tag        string  `json:"tag,omitempty"`
	Role       string  `json:"role,omitempty"`
	Name       string  `json:"name,omitempty"`
	Changed    bool    `json:"changed"`
	ScrollTop  float64 `json:"scroll_top,omitempty"`
	ScrollLeft float64 `json:"scroll_left,omitempty"`
}
