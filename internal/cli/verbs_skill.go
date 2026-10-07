package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Don-Works/brw/internal/agentskill"
)

func skillVerbs() []verb {
	return []verb{
		{
			name:    "skill",
			usage:   "[document]",
			summary: "print the agent skill the daemon serves, stamped with its build",
			method:  http.MethodGet,
			path:    "/api/skill",
			build: func(_ *options, args []string) (request, error) {
				if len(args) > 1 {
					return request{}, fmt.Errorf("expected at most one document path, got %d", len(args))
				}
				query := url.Values{}
				if len(args) == 1 {
					query.Set("document", strings.TrimSpace(args[0]))
				}
				return request{Query: query}, nil
			},
			render: renderSkill,
		},
	}
}

func renderSkill(w io.Writer, _ *options, body []byte) error {
	var doc agentskill.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	version := doc.Version
	if version == "" {
		version = "unknown (the daemon did not report a build)"
	}
	fmt.Fprintf(w, "<!-- brw skill %s, served by brwd %s (%s) -->\n", doc.Path, version, doc.Source)
	if len(doc.Documents) > 1 {
		fmt.Fprintf(w, "<!-- other documents: %s -->\n", strings.Join(doc.Documents, ", "))
	}
	_, err := io.WriteString(w, doc.Content)
	return err
}
