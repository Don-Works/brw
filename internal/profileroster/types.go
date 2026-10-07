// Package profileroster is the operator's view of the browser profiles in a policy: which sites each brw-owned profile is signed in to, creating a new isolated profile, and copying one site's cookies from one brw-owned profile into another.
package profileroster

import (
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Well is one profile column on the board.
type Well struct {
	Name        string              `json:"name"`
	Account     string              `json:"account,omitempty"`
	Namespace   string              `json:"namespace"`
	Workspace   string              `json:"workspace"`
	Transport   string              `json:"transport"`
	Reachable   bool                `json:"reachable"`
	DaemonError string              `json:"daemon_error,omitempty"`
	AcceptsDrop bool                `json:"accepts_drop"`
	OffersDrag  bool                `json:"offers_drag"`
	UserDataDir string              `json:"user_data_dir,omitempty"`
	HTTPAddr    string              `json:"http_addr,omitempty"`
	Chips       []Chip              `json:"chips"`
	Pins        []profilepolicy.Pin `json:"pins,omitempty"`
}

// Chip is one site session on a well.
type Chip struct {
	Domain  string   `json:"domain"`
	Account string   `json:"account,omitempty"`
	Health  string   `json:"health"`
	Names   []string `json:"names,omitempty"`
	Pinned  bool     `json:"pinned,omitempty"`
}

// Board is the whole roster.
type Board struct {
	Wells []Well `json:"wells"`
}

// CopyResult reports a copy or move.
type CopyResult struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Domain  string `json:"domain"`
	Mode    string `json:"mode"`
	Copied  int    `json:"copied"`
	Removed int    `json:"removed,omitempty"`
	Health  string `json:"health"`
}

// CreateRequest describes a new isolated profile.
type CreateRequest struct {
	Name       string
	Account    string
	Browser    string
	PolicyPath string
	Home       string
	GOOS       string
	BRWDPath   string
	Now        time.Time
}

// CreateResult is the profile that was created or already existed, and the commands that start its daemon.
type CreateResult struct {
	Profile        profilepolicy.Profile `json:"profile"`
	Created        bool                  `json:"created"`
	HTTPAddr       string                `json:"http_addr"`
	RunCommand     string                `json:"run_command"`
	ServiceCommand string                `json:"service_command"`
}

const (
	HealthSignedIn         = "signed-in"
	HealthExpired          = "expired"
	HealthCopiedUnverified = "copied-unverified"
	HealthMissing          = "missing"
	HealthUnknown          = "unknown"
)
