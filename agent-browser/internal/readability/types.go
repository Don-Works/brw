package readability

type PageRead struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Main     string    `json:"main"`
	Headings []Heading `json:"headings"`
	Links    []Link    `json:"links"`
	Forms    []Form    `json:"forms"`
	Tables   []Table   `json:"tables"`
	Metadata Metadata  `json:"metadata"`
}

type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	ID    string `json:"id,omitempty"`
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
	Ref      string `json:"ref,omitempty"`
	Role     string `json:"role"`
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Value    string `json:"value,omitempty"`
	Required bool   `json:"required,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
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
