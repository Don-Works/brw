package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/Don-Works/brw/internal/devtools"
)

// The developer observations get verbs of their own because the shell is where
// a human looks at a page they are working on: `brw a11y` then `brw highlight
// @e12` is the whole review loop, without a model in it.

func devtoolsVerbs() []verb {
	return []verb{
		{
			name:    "vitals",
			summary: "report the page's Core Web Vitals",
			method:  http.MethodPost,
			path:    "/api/page/vitals",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.IntVar(&opts.settleMS, "settle", 0, "ms to let the performance timeline drain (default 250, max 5000)")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				return request{Body: map[string]any{"settle_ms": opts.settleMS}}, nil
			},
			render: renderVitals,
		},
		{
			name:    "a11y",
			summary: "audit the page for accessibility failures",
			method:  http.MethodPost,
			path:    "/api/page/a11y",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.StringVar(&opts.tags, "tags", "", "comma-separated axe tags to limit the run, e.g. wcag2aa")
				fs.StringVar(&opts.rule, "rule", "", "audit only this axe rule id, e.g. color-contrast")
				fs.IntVar(&opts.limit, "limit", 0, "describe at most this many failing rules")
			},
			build: func(opts *options, args []string) (request, error) {
				if err := noArgs(args); err != nil {
					return request{}, err
				}
				body := map[string]any{}
				if tags := commaList(opts.tags); len(tags) > 0 {
					body["tags"] = tags
				}
				if rule := strings.TrimSpace(opts.rule); rule != "" {
					body["rules"] = []string{rule}
				}
				if opts.limit > 0 {
					body["max_rules"] = opts.limit
				}
				return request{Body: body}, nil
			},
			render: renderAudit,
		},
		{
			name:    "highlight",
			usage:   "@<ref>",
			summary: "outline an element on screen, or --clear to remove it",
			method:  http.MethodPost,
			path:    "/api/page/highlight",
			flags: func(fs *flag.FlagSet, opts *options) {
				fs.BoolVar(&opts.clear, "clear", false, "remove every brw highlight and draw nothing")
				fs.StringVar(&opts.color, "color", "", "outline colour: "+strings.Join(devtools.HighlightColorNames(), ", "))
				fs.StringVar(&opts.label, "label", "", "caption drawn above the box")
				fs.IntVar(&opts.durationMS, "for", 0, "remove the overlay automatically after this many ms")
				fs.BoolVar(&opts.scroll, "scroll", false, "scroll the element into view")
			},
			build: func(opts *options, args []string) (request, error) {
				body := map[string]any{}
				if opts.clear {
					if err := noArgs(args); err != nil {
						return request{}, err
					}
					body["clear"] = true
					return request{Body: body}, nil
				}
				ref, err := refArg(args)
				if err != nil {
					return request{}, err
				}
				body["ref"] = ref
				body["scroll"] = opts.scroll
				if opts.color != "" {
					body["color"] = opts.color
				}
				if opts.label != "" {
					body["label"] = opts.label
				}
				if opts.durationMS > 0 {
					body["duration_ms"] = opts.durationMS
				}
				return request{Body: body}, nil
			},
			render: renderHighlight,
		},
	}
}

func commaList(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func renderVitals(w io.Writer, _ *options, body []byte) error {
	var vitals devtools.Vitals
	if err := json.Unmarshal(body, &vitals); err != nil {
		return err
	}
	if vitals.Title != "" || vitals.URL != "" {
		fmt.Fprintf(w, "%s  %s\n\n", vitals.Title, vitals.URL)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "LCP\t%s\t%s\t%s\n", millis(vitals.LCPMS), vitals.Ratings["lcp"], vitals.LCPElement)
	fmt.Fprintf(tw, "CLS\t%s\t%s\t%d shift(s)\n", score(vitals.CLS), vitals.Ratings["cls"], vitals.CLSShifts)
	fmt.Fprintf(tw, "INP\t%s\t%s\t%d interaction(s)\n", millis(vitals.INPMS), vitals.Ratings["inp"], vitals.Interactions)
	fmt.Fprintf(tw, "TTFB\t%s\t%s\t\n", millis(vitals.TTFBMS), vitals.Ratings["ttfb"])
	fmt.Fprintf(tw, "FCP\t%s\t\t\n", millis(vitals.FCPMS))
	fmt.Fprintf(tw, "load\t%s\t\t%s\n", millis(vitals.LoadMS), vitals.NavigationType)
	_ = tw.Flush()
	if len(vitals.Unavailable) > 0 {
		fmt.Fprintf(w, "\nnot observable here: %s\n", strings.Join(vitals.Unavailable, ", "))
	}
	if vitals.Note != "" {
		fmt.Fprintf(w, "\n%s\n", vitals.Note)
	}
	return nil
}

// millis prints an absent metric as "—" rather than 0, because a page that has
// not painted yet and a page that painted instantly are different pages.
func millis(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.0fms", *value)
}

// score prints the unitless metrics on the same rule: a browser that cannot
// observe layout shift is not a browser that saw none.
func score(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%.4f", *value)
}

func renderAudit(w io.Writer, _ *options, body []byte) error {
	var result devtools.AuditResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if result.Title != "" || result.URL != "" {
		fmt.Fprintf(w, "%s  %s\n", result.Title, result.URL)
	}
	fmt.Fprintf(w, "%s: %d rule(s) failed across %d element(s); %d need review, %d passed\n\n",
		result.Engine, result.Violations, result.ViolationNodes, result.Incomplete, result.Passes)
	if len(result.Rules) == 0 {
		fmt.Fprintln(w, "no failures")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, rule := range result.Rules {
			// refs and targets are parallel but both carry omitempty, and this
			// decodes whatever the daemon sent rather than what the summarizer
			// produced. Index the fallback only when it is there.
			refs := make([]string, 0, len(rule.Refs))
			for i, ref := range rule.Refs {
				switch {
				case ref != "":
					refs = append(refs, "@"+ref)
				case i < len(rule.Targets) && rule.Targets[i] != "":
					refs = append(refs, rule.Targets[i])
				default:
					refs = append(refs, "?")
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", rule.Impact, rule.ID, rule.Nodes,
				strings.Join(refs, " "), truncate(rule.Help, maxNameChars))
		}
		_ = tw.Flush()
	}
	if result.Truncated {
		fmt.Fprintln(w, "\n— more rules failed than were listed; raise --limit or read the artifact")
	}
	if result.Artifact != nil {
		fmt.Fprintf(w, "\nfull report: %s (%d bytes) — brw artifact read %s\n",
			result.Artifact.ID, result.Artifact.SizeBytes, result.Artifact.ID)
	}
	if result.PageEffects != "" {
		fmt.Fprintf(w, "\nleft in the page: %s\n", result.PageEffects)
	}
	if result.Note != "" {
		fmt.Fprintf(w, "\n%s\n", result.Note)
	}
	return nil
}

func renderHighlight(w io.Writer, _ *options, body []byte) error {
	var result devtools.HighlightResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	if result.Cleared || result.Active == 0 && len(result.Marked) == 0 {
		fmt.Fprintln(w, "no highlight on the page")
		if result.Note != "" {
			fmt.Fprintf(w, "%s\n", result.Note)
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, mark := range result.Marked {
		switch {
		case !mark.Found:
			fmt.Fprintf(tw, "@%s\tnot on the page\t\n", mark.Ref)
		case mark.InViewport:
			fmt.Fprintf(tw, "@%s\tmarked\t%.0fx%.0f at %.0f,%.0f\n", mark.Ref, mark.Width, mark.Height, mark.X, mark.Y)
		default:
			fmt.Fprintf(tw, "@%s\tmarked, off screen\t%.0fx%.0f at %.0f,%.0f\n", mark.Ref, mark.Width, mark.Height, mark.X, mark.Y)
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%s\n", result.Reversible)
	if result.ExpiresMS > 0 {
		fmt.Fprintf(w, "clears itself in %dms\n", result.ExpiresMS)
	}
	return nil
}
