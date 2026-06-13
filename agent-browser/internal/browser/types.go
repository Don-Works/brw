package browser

import "time"

type Config struct {
	ChromePath       string
	UserDataDir      string
	ProfileDirectory string
	RemoteURL        string
	Port             int
	Extensions       []string
	ChromeArgs       []string
	Timeout          time.Duration
}

type Tab struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

type OpenResult struct {
	Tab Tab `json:"tab"`
}

type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type Screenshot struct {
	MIMEType string `json:"mime_type"`
	Data     []byte `json:"-"`
	Base64   string `json:"base64,omitempty"`
}
