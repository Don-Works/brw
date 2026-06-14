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
	Since         int64  `json:"since,omitempty"`
}

type FindOptions struct {
	Query         string `json:"query,omitempty"`
	Text          string `json:"text,omitempty"`
	Role          string `json:"role,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	ViewportOnly  bool   `json:"viewport_only,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
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

type Element struct {
	Ref              string   `json:"ref"`
	Role             string   `json:"role"`
	Name             string   `json:"name"`
	Tag              string   `json:"tag"`
	Type             string   `json:"type,omitempty"`
	Href             string   `json:"href,omitempty"`
	Value            string   `json:"value,omitempty"`
	Sensitive        bool     `json:"sensitive,omitempty"`
	Visible          bool     `json:"visible"`
	InViewport       bool     `json:"in_viewport"`
	Disabled         bool     `json:"disabled"`
	Required         bool     `json:"required,omitempty"`
	Valid            *bool    `json:"valid,omitempty"`
	ValidationMsg    string   `json:"validation_message,omitempty"`
	Checked          *bool    `json:"checked,omitempty"`
	Selected         *bool    `json:"selected,omitempty"`
	Expanded         *bool    `json:"expanded,omitempty"`
	Controls         string   `json:"controls,omitempty"`
	Signals          []string `json:"signals,omitempty"`
	MatchReasons     []string `json:"match_reasons,omitempty"`
	Source           []string `json:"source"`
	Key              string   `json:"-"`
}

type AccessibilitySummary struct {
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
