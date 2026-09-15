package profileroster

import (
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Well is one profile column on the board.
type Well struct {
	Name        string              `json:"name"`
	DisplayName string              `json:"display_name,omitempty"`
	Account     string              `json:"account,omitempty"`
	Namespace   string              `json:"namespace"`
	Workspace   string              `json:"workspace"`
	Transport   string              `json:"transport"`
	Reachable   bool                `json:"reachable"`
	DaemonError string              `json:"daemon_error,omitempty"`
	AcceptsDrop bool                `json:"accepts_drop"`
	SourceOnly  bool                `json:"source_only"`
	UserDataDir string              `json:"user_data_dir,omitempty"`
	HTTPAddr    string              `json:"http_addr,omitempty"`
	Chips       []Chip              `json:"chips"`
	Pins        []profilepolicy.Pin `json:"pins,omitempty"`
}

// Chip is one site session on a well. No cookie values.
type Chip struct {
	Domain  string   `json:"domain"`
	Account string   `json:"account,omitempty"`
	Health  string   `json:"health"`
	Names   []string `json:"names,omitempty"`
	Pinned  bool     `json:"pinned,omitempty"`
}

// Board is the full visual-editor payload.
type Board struct {
	Wells []Well `json:"wells"`
}

// CopyResult is what a domain copy/move returns to a UI. Values never appear.
type CopyResult struct {
	BatchID string `json:"batch_id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Domain  string `json:"domain"`
	Mode    string `json:"mode"`
	Copied  int    `json:"copied"`
	Health  string `json:"health"`
}

// CreateRequest is how an operator adds a well.
type CreateRequest struct {
	Name           string
	Account        string
	Browser        string
	PolicyPath     string
	Home           string
	GOOS           string
	BRWDPath       string
	InstallService bool
	Now            time.Time
}

// CreateResult is the well that was created or already existed.
type CreateResult struct {
	Profile  profilepolicy.Profile `json:"profile"`
	Created  bool                  `json:"created"`
	HTTPAddr string                `json:"http_addr"`
}

const (
	HealthSignedIn         = "signed-in"
	HealthExpired          = "expired"
	HealthCopiedUnverified = "copied-unverified"
	HealthMissing          = "missing"
	HealthUnknown          = "unknown"
)

// CookieMetas is a convenience alias so HTTP handlers do not import browser
// just to name the redacted cookie type.
type CookieMetas = []browser.CookieMeta
