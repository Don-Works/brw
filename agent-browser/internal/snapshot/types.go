package snapshot

type PageSnapshot struct {
	URL           string                 `json:"url"`
	Title         string                 `json:"title"`
	Elements      []Element              `json:"elements"`
	Accessibility AccessibilitySummary   `json:"accessibility"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

type Element struct {
	Ref        string   `json:"ref"`
	Role       string   `json:"role"`
	Name       string   `json:"name"`
	Tag        string   `json:"tag"`
	Type       string   `json:"type,omitempty"`
	Href       string   `json:"href,omitempty"`
	Value      string   `json:"value,omitempty"`
	Visible    bool     `json:"visible"`
	InViewport bool     `json:"in_viewport"`
	Disabled   bool     `json:"disabled"`
	Required   bool     `json:"required,omitempty"`
	Checked    *bool    `json:"checked,omitempty"`
	Source     []string `json:"source"`
	Key        string   `json:"-"`
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
