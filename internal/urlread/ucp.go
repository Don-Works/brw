package urlread

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

const ucpProbeMaxBytes = 64 << 10

// UCPProfile summarizes bounded declarations without asserting protocol support.
type UCPProfile struct {
	Status          string              `json:"status"`
	Version         string              `json:"version,omitempty"`
	Capabilities    []UCPCapability     `json:"capabilities,omitempty"`
	Transports      []string            `json:"transports,omitempty"`
	PaymentHandlers []UCPPaymentHandler `json:"payment_handlers,omitempty"`
	Truncated       bool                `json:"truncated,omitempty"`
}

// UCPCapability identifies one declared capability edition.
type UCPCapability struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// UCPPaymentHandler identifies one declared payment handler without configuration.
type UCPPaymentHandler struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

type ucpDeclaration struct {
	Version   string `json:"version"`
	Transport string `json:"transport"`
	ID        string `json:"id"`
}

func parseUCPProfile(body []byte, oversized bool) *UCPProfile {
	if oversized || len(body) > ucpProbeMaxBytes {
		return &UCPProfile{Status: "unavailable", Truncated: true}
	}
	var doc struct {
		UCP *struct {
			Version         string                      `json:"version"`
			Capabilities    map[string][]ucpDeclaration `json:"capabilities"`
			Services        map[string][]ucpDeclaration `json:"services"`
			PaymentHandlers map[string][]ucpDeclaration `json:"payment_handlers"`
		} `json:"ucp"`
	}
	if !utf8.Valid(body) || json.Unmarshal(body, &doc) != nil || doc.UCP == nil || strings.TrimSpace(doc.UCP.Version) == "" {
		return &UCPProfile{Status: "invalid"}
	}
	out := &UCPProfile{Status: "parsed"}
	bound := func(value string) string {
		value = strings.TrimSpace(value)
		if utf8.RuneCountInString(value) > 160 {
			out.Truncated = true
			return string([]rune(value)[:160])
		}
		return value
	}
	out.Version = bound(doc.UCP.Version)
	caps := make(map[UCPCapability]bool)
	for name, entries := range doc.UCP.Capabilities {
		name = bound(name)
		if name == "" {
			return &UCPProfile{Status: "invalid"}
		}
		for _, entry := range entries {
			caps[UCPCapability{Name: name, Version: bound(entry.Version)}] = true
		}
	}
	for entry := range caps {
		out.Capabilities = append(out.Capabilities, entry)
	}
	sort.Slice(out.Capabilities, func(i, j int) bool {
		a, b := out.Capabilities[i], out.Capabilities[j]
		if a.Name == b.Name {
			return a.Version < b.Version
		}
		return a.Name < b.Name
	})
	if len(out.Capabilities) > 12 {
		out.Capabilities = out.Capabilities[:12]
		out.Truncated = true
	}
	transports := make(map[string]bool)
	for _, entries := range doc.UCP.Services {
		for _, entry := range entries {
			if value := bound(entry.Transport); value != "" {
				transports[value] = true
			}
		}
	}
	for value := range transports {
		out.Transports = append(out.Transports, value)
	}
	sort.Strings(out.Transports)
	if len(out.Transports) > 8 {
		out.Transports = out.Transports[:8]
		out.Truncated = true
	}
	handlers := make(map[UCPPaymentHandler]bool)
	for name, entries := range doc.UCP.PaymentHandlers {
		name = bound(name)
		if name == "" {
			return &UCPProfile{Status: "invalid"}
		}
		for _, entry := range entries {
			handlers[UCPPaymentHandler{Name: name, ID: bound(entry.ID)}] = true
		}
	}
	for entry := range handlers {
		out.PaymentHandlers = append(out.PaymentHandlers, entry)
	}
	sort.Slice(out.PaymentHandlers, func(i, j int) bool {
		a, b := out.PaymentHandlers[i], out.PaymentHandlers[j]
		if a.Name == b.Name {
			return a.ID < b.ID
		}
		return a.Name < b.Name
	})
	if len(out.PaymentHandlers) > 8 {
		out.PaymentHandlers = out.PaymentHandlers[:8]
		out.Truncated = true
	}
	return out
}
