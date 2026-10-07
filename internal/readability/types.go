package readability

// PageRead is one semantic read of a page.
type PageRead struct {
	URL              string    `json:"url"`
	Title            string    `json:"title"`
	Main             string    `json:"main"`
	Headings         []Heading `json:"headings"`
	Links            []Link    `json:"links"`
	Forms            []Form    `json:"forms"`
	Tables           []Table   `json:"tables"`
	TablesTruncated  bool      `json:"tables_truncated,omitempty"`
	TablesComplete   bool      `json:"tables_complete,omitempty"`
	SectionsAnchored bool      `json:"sections_anchored,omitempty"`
	Metadata         Metadata  `json:"metadata"`

	// Paging metadata, set by Window when a read is bounded.
	MainTotalChars    int  `json:"main_total_chars,omitempty"`
	MainTruncated     bool `json:"main_truncated,omitempty"`
	NextOffset        int  `json:"next_offset,omitempty"`
	HeadingsTruncated bool `json:"headings_truncated,omitempty"`
	LinksTruncated    bool `json:"links_truncated,omitempty"`

	// Section and SectionLevel echo the heading a section-addressed read resolved to, so a caller can confirm it got the section it meant.
	Section      string `json:"section,omitempty"`
	SectionLevel int    `json:"section_level,omitempty"`
}

type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	ID    string `json:"id,omitempty"`
	// Offset is the character position of this heading within Main, and is what makes a section addressable by name rather than by a byte offset the caller has to guess.
	Offset *int `json:"offset,omitempty"`
}

type Link struct {
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text"`
	Href string `json:"href"`
}

type Form struct {
	Ref      string        `json:"ref,omitempty"`
	Name     string        `json:"name,omitempty"`
	Action   string        `json:"action,omitempty"`
	Method   string        `json:"method,omitempty"`
	Controls []FormControl `json:"controls"`
}

type FormControl struct {
	Ref       string `json:"ref,omitempty"`
	Role      string `json:"role"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	Value     string `json:"value,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Required  bool   `json:"required,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type Table struct {
	Truncated bool       `json:"truncated,omitempty"`
	Caption   string     `json:"caption,omitempty"`
	Headers   []string   `json:"headers,omitempty"`
	Rows      [][]string `json:"rows"`
}

type Metadata struct {
	Description string            `json:"description,omitempty"`
	Canonical   string            `json:"canonical,omitempty"`
	Lang        string            `json:"lang,omitempty"`
	OpenGraph   map[string]string `json:"open_graph,omitempty"`
}
