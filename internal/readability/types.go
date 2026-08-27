package readability

// PageRead is one semantic read of a page. The prose lives in Main only: the
// wire payload used to carry the identical string in both "text" and "main",
// which doubled the cost of every read for no added information.
type PageRead struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Main     string    `json:"main"`
	Headings []Heading `json:"headings"`
	Links    []Link    `json:"links"`
	Forms    []Form    `json:"forms"`
	Tables   []Table   `json:"tables"`
	Metadata Metadata  `json:"metadata"`

	// Paging metadata, set by Window when a read is bounded. Absent on an
	// unbounded read that returned the whole document.
	MainTotalChars    int  `json:"main_total_chars,omitempty"`
	MainTruncated     bool `json:"main_truncated,omitempty"`
	NextOffset        int  `json:"next_offset,omitempty"`
	HeadingsTruncated bool `json:"headings_truncated,omitempty"`
	LinksTruncated    bool `json:"links_truncated,omitempty"`

	// Section and SectionLevel echo the heading a section-addressed read
	// resolved to, so a caller can confirm it got the section it meant.
	Section      string `json:"section,omitempty"`
	SectionLevel int    `json:"section_level,omitempty"`
}

type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	ID    string `json:"id,omitempty"`
	// Offset is the character position of this heading within Main, and is what
	// makes a section addressable by name rather than by a byte offset the
	// caller has to guess.
	//
	// A pointer so that three states stay distinct. nil means offsets were not
	// computed at all — an older brw upstream in proxy mode returns headings
	// without them, and treating that as 0 made every section resolve to the
	// start of the document and silently return the whole page. A negative
	// value means the heading lies outside the extracted prose and cannot be
	// addressed. Zero or more is a real position.
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
	Caption string     `json:"caption,omitempty"`
	Headers []string   `json:"headers,omitempty"`
	Rows    [][]string `json:"rows"`
}

type Metadata struct {
	Description string            `json:"description,omitempty"`
	Canonical   string            `json:"canonical,omitempty"`
	Lang        string            `json:"lang,omitempty"`
	OpenGraph   map[string]string `json:"open_graph,omitempty"`
}
