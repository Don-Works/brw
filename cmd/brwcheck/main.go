package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
)

type suiteFile struct {
	Version   int        `json:"version"`
	Scenarios []scenario `json:"scenarios"`
}

type scenario struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Requires    []string `json:"requires,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Actions     []step   `json:"actions"`
	HumanAssist string   `json:"human_assist,omitempty"`
}

type step struct {
	Optional          bool                   `json:"optional,omitempty"`
	Open              *openStep              `json:"open,omitempty"`
	WaitFor           *waitForStep           `json:"wait_for,omitempty"`
	WaitForTab        *tabStep               `json:"wait_for_tab,omitempty"`
	FocusTab          *tabStep               `json:"focus_tab,omitempty"`
	Snapshot          *snapshotStep          `json:"snapshot,omitempty"`
	Find              *findStep              `json:"find,omitempty"`
	Read              *readStep              `json:"read,omitempty"`
	Click             *targetStep            `json:"click,omitempty"`
	Type              *typeStep              `json:"type,omitempty"`
	Fill              *fillStep              `json:"fill,omitempty"`
	UploadFile        *uploadFileStep        `json:"upload_file,omitempty"`
	Select            *selectStep            `json:"select,omitempty"`
	Press             *pressStep             `json:"press,omitempty"`
	Scroll            *scrollStep            `json:"scroll,omitempty"`
	Screenshot        *screenshotStep        `json:"screenshot,omitempty"`
	ScreenshotElement *screenshotElementStep `json:"screenshot_element,omitempty"`
	Commit            *targetStep            `json:"commit,omitempty"`
	AssertVisible     *assertRefStep         `json:"assert_visible,omitempty"`
	AssertHidden      *assertRefStep         `json:"assert_hidden,omitempty"`
	AssertText        *assertTextStep        `json:"assert_text,omitempty"`
	AssertValue       *assertValueStep       `json:"assert_value,omitempty"`
	Assert            *assertStep            `json:"assert,omitempty"`
	Cookies           *cookiesStep           `json:"cookies,omitempty"`
	GroupTabs         *groupTabsStep         `json:"group_tabs,omitempty"`
	UngroupTabs       *ungroupTabsStep       `json:"ungroup_tabs,omitempty"`
	ListTabGroups     *listTabGroupsStep     `json:"list_tab_groups,omitempty"`
	ListTabs          *listTabsStep          `json:"list_tabs,omitempty"`
	CloseTab          *tabStep               `json:"close_tab,omitempty"`
	WindowResize      *windowResizeStep      `json:"window_resize,omitempty"`
	WindowBounds      *windowBoundsStep      `json:"window_bounds,omitempty"`
	Hover             *targetStep            `json:"hover,omitempty"`
	ClickXY           *clickXYStep           `json:"click_xy,omitempty"`
	Drag              *dragStep              `json:"drag,omitempty"`
	MouseDown         *mousePointStep        `json:"mouse_down,omitempty"`
	MouseUp           *mousePointStep        `json:"mouse_up,omitempty"`
	Batch             *batchStep             `json:"batch,omitempty"`
}

// The tab-group, window, pointer and batch steps below exist because the suite
// drove 18 of the 69 registered tools. Everything an agent uses to manage tabs,
// size a window, aim the pointer at a coordinate, or run several actions in one
// call was covered only by Go tests against a fake controller, which cannot
// catch a tool that marshals correctly and then fails against real Chrome.

type groupTabsStep struct {
	Tabs    []string `json:"tabs,omitempty"`
	Title   string   `json:"title,omitempty"`
	Color   string   `json:"color,omitempty"`
	GroupID string   `json:"group_id,omitempty"`
}

type ungroupTabsStep struct {
	Tabs []string `json:"tabs,omitempty"`
}

type listTabGroupsStep struct {
	MinGroups     int    `json:"min_groups,omitempty"`
	WantTitle     string `json:"want_title,omitempty"`
	WantColor     string `json:"want_color,omitempty"`
	AbsentTitle   string `json:"absent_title,omitempty"`
	MinMemberTabs int    `json:"min_member_tabs,omitempty"`
}

type listTabsStep struct {
	MinTabs      int    `json:"min_tabs,omitempty"`
	WantURL      string `json:"want_url,omitempty"`
	RequireLease bool   `json:"require_lease,omitempty"`
}

type windowResizeStep struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type windowBoundsStep struct {
	MinWidth  int `json:"min_width,omitempty"`
	MinHeight int `json:"min_height,omitempty"`
	WantWidth int `json:"want_width,omitempty"`
}

type clickXYStep struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type dragStep struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type mousePointStep struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button,omitempty"`
}

type batchStep struct {
	Steps     []map[string]any `json:"steps"`
	MinOK     int              `json:"min_ok,omitempty"`
	WantError bool             `json:"want_error,omitempty"`
}

type assertRefStep struct {
	Ref       string `json:"ref,omitempty"`
	Target    string `json:"target,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type assertTextStep struct {
	Ref       string `json:"ref,omitempty"`
	Target    string `json:"target,omitempty"`
	Text      string `json:"text"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type assertValueStep struct {
	Ref       string `json:"ref,omitempty"`
	Target    string `json:"target,omitempty"`
	Value     string `json:"value"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// assertStep drives brw_assert. The assertion is carried as the raw object a
// caller would send, so the suite exercises the published field names against
// real Chrome rather than a Go struct that happens to marshal. want_error is the
// other half of the contract: a deterministic check that cannot fail is not one.
type assertStep struct {
	Assertion map[string]any `json:"assertion"`
	WantError bool           `json:"want_error,omitempty"`
}

// cookiesStep drives brw_cookies over the daemon's HTTP API: set a cookie
// (with attributes), list applicable cookies, or delete by name, with list-side
// assertions. Values support ${HTTP_FIXTURES}-style expansion like other URLs.
type cookiesStep struct {
	Action   string  `json:"action"`
	URL      string  `json:"url,omitempty"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Name     string  `json:"name,omitempty"`
	Value    string  `json:"value,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
	Expires  float64 `json:"expires,omitempty"`

	// List assertions.
	MinCount    int      `json:"min_count,omitempty"`
	Require     []string `json:"require,omitempty"`      // cookie names that must be present
	Absent      []string `json:"absent,omitempty"`       // cookie names that must NOT be present
	HTTPOnlyOf  []string `json:"http_only_of,omitempty"` // names that must be present AND http_only
	TabID       string   `json:"tab_id,omitempty"`
	ExpectValue string   `json:"expect_value,omitempty"` // with name: the set/read-back value must match exactly
}

type openStep struct {
	URL    string `json:"url"`
	SaveAs string `json:"save_as,omitempty"`
}

type waitForStep struct {
	Condition string `json:"condition"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

type tabStep struct {
	ID            string `json:"id,omitempty"`
	Target        string `json:"target,omitempty"`
	URLContains   string `json:"url_contains,omitempty"`
	TitleContains string `json:"title_contains,omitempty"`
	NewOnly       bool   `json:"new_only,omitempty"`
	SaveAs        string `json:"save_as,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
	Focus         bool   `json:"focus,omitempty"`
}

type snapshotStep struct {
	Mode          string         `json:"mode,omitempty"`
	Query         string         `json:"query,omitempty"`
	Role          string         `json:"role,omitempty"`
	Text          string         `json:"text,omitempty"`
	Limit         int            `json:"limit,omitempty"`
	ViewportOnly  *bool          `json:"viewport_only,omitempty"`
	IncludeHidden bool           `json:"include_hidden,omitempty"`
	IncludeAX     *bool          `json:"include_ax,omitempty"`
	Since         string         `json:"since,omitempty"`
	MaxBytes      int            `json:"max_bytes,omitempty"`
	MinElements   int            `json:"min_elements,omitempty"`
	Require       []elementMatch `json:"require,omitempty"`
}

type findStep struct {
	Query         string         `json:"query,omitempty"`
	Role          string         `json:"role,omitempty"`
	Text          string         `json:"text,omitempty"`
	Limit         int            `json:"limit,omitempty"`
	ViewportOnly  *bool          `json:"viewport_only,omitempty"`
	IncludeHidden bool           `json:"include_hidden,omitempty"`
	MinElements   int            `json:"min_elements,omitempty"`
	Require       []elementMatch `json:"require,omitempty"`
}

type elementMatch struct {
	Role         string `json:"role,omitempty"`
	Name         string `json:"name,omitempty"`
	NameContains string `json:"name_contains,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Type         string `json:"type,omitempty"`
	HrefContains string `json:"href_contains,omitempty"`
	SaveAs       string `json:"save_as,omitempty"`
	Visible      *bool  `json:"visible,omitempty"`
	Sensitive    *bool  `json:"sensitive,omitempty"`
	ValueEmpty   *bool  `json:"value_empty,omitempty"`
}

type readStep struct {
	TitleContains    string         `json:"title_contains,omitempty"`
	AnyTitleContains []string       `json:"any_title_contains,omitempty"`
	URLContains      string         `json:"url_contains,omitempty"`
	MainContains     []string       `json:"main_contains,omitempty"`
	AnyMainContains  []string       `json:"any_main_contains,omitempty"`
	MinHeadings      int            `json:"min_headings,omitempty"`
	MinLinks         int            `json:"min_links,omitempty"`
	MinForms         int            `json:"min_forms,omitempty"`
	MinTables        int            `json:"min_tables,omitempty"`
	Metadata         metadataAssert `json:"metadata,omitempty"`
	Forms            []formAssert   `json:"forms,omitempty"`
}

type formAssert struct {
	NameContains   string         `json:"name_contains,omitempty"`
	MinControls    int            `json:"min_controls,omitempty"`
	RequireControl *controlAssert `json:"require_control,omitempty"`
}

type controlAssert struct {
	NameContains string `json:"name_contains,omitempty"`
	Sensitive    *bool  `json:"sensitive,omitempty"`
	ValueEmpty   *bool  `json:"value_empty,omitempty"`
}

type metadataAssert struct {
	DescriptionContains string `json:"description_contains,omitempty"`
	CanonicalContains   string `json:"canonical_contains,omitempty"`
	Lang                string `json:"lang,omitempty"`
}

type targetStep struct {
	Target string        `json:"target,omitempty"`
	Ref    string        `json:"ref,omitempty"`
	Match  *elementMatch `json:"match,omitempty"`
}

type typeStep struct {
	Target string        `json:"target,omitempty"`
	Ref    string        `json:"ref,omitempty"`
	Match  *elementMatch `json:"match,omitempty"`
	Text   string        `json:"text"`
}

type fillStep struct {
	Target  string        `json:"target,omitempty"`
	Ref     string        `json:"ref,omitempty"`
	Query   string        `json:"query,omitempty"`
	Role    string        `json:"role,omitempty"`
	Match   *elementMatch `json:"match,omitempty"`
	Text    string        `json:"text"`
	Replace *bool         `json:"replace,omitempty"`
}

type uploadFileStep struct {
	Target string        `json:"target,omitempty"`
	Ref    string        `json:"ref,omitempty"`
	Query  string        `json:"query,omitempty"`
	Role   string        `json:"role,omitempty"`
	Match  *elementMatch `json:"match,omitempty"`
	Path   string        `json:"path,omitempty"`
	Paths  []string      `json:"paths,omitempty"`
}

type selectStep struct {
	Target string        `json:"target,omitempty"`
	Ref    string        `json:"ref,omitempty"`
	Match  *elementMatch `json:"match,omitempty"`
	Value  string        `json:"value"`
}

type pressStep struct {
	Key string `json:"key"`
}

type scrollStep struct {
	Direction string `json:"direction"`
}

type screenshotStep struct {
	MinBytes int  `json:"min_bytes,omitempty"`
	Optional bool `json:"optional,omitempty"`
}

type screenshotElementStep struct {
	Target   string        `json:"target,omitempty"`
	Ref      string        `json:"ref,omitempty"`
	Match    *elementMatch `json:"match,omitempty"`
	MinBytes int           `json:"min_bytes,omitempty"`
	Optional bool          `json:"optional,omitempty"`
}

type findResult struct {
	Elements []snapshot.Element `json:"elements"`
}

type runner struct {
	client   *apiClient
	repoRoot string
	refs     map[string]string
	tabRefs  map[string]string
	preClick map[string]bool
	tabID    string

	// httpFixtures lazily serves tests/fixtures over loopback HTTP so cookie
	// scenarios get a real http-origin (file:// pages cannot hold cookies).
	httpFixturesOnce sync.Once
	httpFixturesURL  string
	httpFixturesLn   net.Listener
}

type apiClient struct {
	base string
	http *http.Client
}

func main() {
	var suitePath string
	var baseURL string
	var includeNetwork bool
	var includeAuth bool
	var includeManual bool
	var only string
	var repoRootFlag string
	var benchMode bool
	var benchJSON bool
	var benchOnly string
	var benchOut string
	flag.BoolVar(&benchMode, "bench", false, "measure per-command cost against the local fixture suite")
	flag.BoolVar(&benchJSON, "bench-json", false, "emit the benchmark record as JSON on stdout")
	flag.StringVar(&benchOnly, "bench-only", "", "restrict the benchmark to one flow id")
	flag.StringVar(&benchOut, "bench-out", "", "write the machine-readable benchmark record to this path")
	var evalMode bool
	var evalJSON bool
	var evalOnly string
	var evalOut string
	var evalVerify bool
	var evalJudge bool
	flag.BoolVar(&evalMode, "eval", false, "run the agent-level evaluations against the local fixture suite")
	flag.BoolVar(&evalJSON, "eval-json", false, "emit the evaluation report as JSON on stdout")
	flag.StringVar(&evalOnly, "eval-only", "", "restrict the evaluation to one task id")
	flag.StringVar(&evalOut, "eval-out", "", "write the machine-readable evaluation report to this path")
	flag.BoolVar(&evalVerify, "eval-verify", false, "also run every task sabotaged and require it to be graded a failure")
	flag.BoolVar(&evalJudge, "eval-judge", false, "add the optional LLM judge over the end-state check (needs ANTHROPIC_API_KEY)")
	flag.StringVar(&suitePath, "suite", "tests/scenarios/core.json", "scenario suite JSON path")
	flag.StringVar(&baseURL, "base-url", envDefault("BRW_URL", "http://127.0.0.1:17310"), "brwd HTTP base URL")
	flag.StringVar(&repoRootFlag, "repo-root", envDefault("BRW_REPO_ROOT", ""), "repo/share root containing tests and fixtures")
	flag.BoolVar(&includeNetwork, "include-network", false, "run scenarios that require public network")
	flag.BoolVar(&includeAuth, "include-auth", false, "run authenticated/profile scenarios")
	flag.BoolVar(&includeManual, "include-manual", false, "run manual human-assist scenarios")
	flag.StringVar(&only, "only", "", "comma-separated scenario ids to run")
	flag.Parse()

	root := repoRootFlag
	if root == "" {
		var err error
		root, err = findRepoRoot()
		if err != nil {
			fatal(err)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		fatal(err)
	}
	if benchMode {
		if err := runBench(benchOptions{
			RepoRoot: root,
			Only:     benchOnly,
			JSON:     benchJSON,
			OutPath:  benchOut,
		}); err != nil {
			fatal(err)
		}
		return
	}
	if evalMode {
		if err := runAgentEval(evalOptions{
			RepoRoot: root,
			Only:     evalOnly,
			JSON:     evalJSON,
			OutPath:  evalOut,
			Verify:   evalVerify,
			Judge:    evalJudge,
		}); err != nil {
			fatal(err)
		}
		return
	}
	if !filepath.IsAbs(suitePath) {
		suitePath = filepath.Join(root, suitePath)
	}
	suite, err := loadSuite(suitePath)
	if err != nil {
		fatal(err)
	}

	c := &apiClient{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 5 * time.Minute},
	}
	transport, err := c.health()
	if err != nil {
		fatal(fmt.Errorf("brwd is not ready at %s: %w", baseURL, err))
	}

	selected := parseOnly(only)
	r := &runner{client: c, repoRoot: root}
	var passed, skipped, failed int
	for _, sc := range suite.Scenarios {
		if len(selected) > 0 && !selected[sc.ID] {
			continue
		}
		if reason := skipReason(sc, includeNetwork, includeAuth, includeManual, transport); reason != "" {
			fmt.Printf("SKIP %-32s %s\n", sc.ID, reason)
			skipped++
			continue
		}
		if sc.HumanAssist != "" {
			fmt.Printf("NOTE %-32s %s\n", sc.ID, sc.HumanAssist)
		}
		r.refs = map[string]string{}
		r.tabRefs = map[string]string{}
		r.tabID = ""
		if err := r.runScenario(sc); err != nil {
			fmt.Printf("FAIL %-32s %v\n", sc.ID, err)
			failed++
			continue
		}
		fmt.Printf("PASS %-32s %s\n", sc.ID, sc.Name)
		passed++
	}
	fmt.Printf("\n%d passed, %d skipped, %d failed\n", passed, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func (r *runner) runScenario(sc scenario) error {
	existingTabs := r.captureTabIDs()
	r.preClick = nil
	defer func() {
		if err := r.cleanupScenarioTabs(existingTabs); err != nil {
			fmt.Printf("WARN %-32s tab cleanup failed: %v\n", sc.ID, err)
		}
	}()
	var total time.Duration
	var slowest time.Duration
	slowestStep := 0
	for i, st := range sc.Actions {
		start := time.Now()
		err := r.runStep(st)
		dur := time.Since(start)
		total += dur
		if dur > slowest {
			slowest = dur
			slowestStep = i + 1
		}
		if err != nil {
			if st.Optional {
				fmt.Printf("WARN %-32s optional step %d skipped: %v\n", sc.ID, i+1, err)
				continue
			}
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	fmt.Printf("TIME %-32s total=%dms steps=%d slowest=step%d@%dms\n",
		sc.ID, total.Milliseconds(), len(sc.Actions), slowestStep, slowest.Milliseconds())
	return nil
}

func (r *runner) cleanupScenarioTabs(existing map[string]bool) error {
	if existing == nil {
		return nil
	}
	tabs, err := r.client.listTabs()
	if err != nil {
		return err
	}
	var closeErrs []string
	for _, tab := range tabs {
		if existing[tab.ID] {
			continue
		}
		var result browser.ActionResult
		if err := r.client.postJSON("/api/browser/close", map[string]string{"id": tab.ID}, &result); err != nil {
			closeErrs = append(closeErrs, fmt.Sprintf("%s: %v", tab.ID, err))
		}
	}
	if len(closeErrs) > 0 {
		return errors.New(strings.Join(closeErrs, "; "))
	}
	return nil
}

func (r *runner) runStep(st step) error {
	switch {
	case st.Open != nil:
		var result browser.OpenResult
		if err := r.client.postJSON("/api/browser/open", map[string]string{"url": r.expandURL(st.Open.URL)}, &result); err != nil {
			return err
		}
		if st.Open.SaveAs != "" {
			r.tabRefs[st.Open.SaveAs] = result.Tab.ID
		}
		r.tabID = result.Tab.ID
		return nil
	case st.WaitFor != nil:
		req := map[string]any{"condition": expandVars(st.WaitFor.Condition)}
		r.addTabID(req)
		if st.WaitFor.TimeoutMS > 0 {
			req["timeout_ms"] = st.WaitFor.TimeoutMS
		}
		var result browser.ActionResult
		return r.client.postJSON("/api/page/wait_for", req, &result)
	case st.WaitForTab != nil:
		tab, err := r.waitForTab(*st.WaitForTab)
		if err != nil {
			return err
		}
		if st.WaitForTab.SaveAs != "" {
			r.tabRefs[st.WaitForTab.SaveAs] = tab.ID
		}
		if st.WaitForTab.Focus {
			r.tabID = tab.ID
			var result browser.ActionResult
			return r.client.postJSON("/api/browser/focus", map[string]string{"id": tab.ID}, &result)
		}
		return nil
	case st.FocusTab != nil:
		id, err := r.resolveTab(*st.FocusTab)
		if err != nil {
			return err
		}
		var result browser.ActionResult
		if err := r.client.postJSON("/api/browser/focus", map[string]string{"id": id}, &result); err != nil {
			return err
		}
		r.tabID = id
		return nil
	case st.Snapshot != nil:
		var snap snapshot.PageSnapshot
		if err := r.client.getJSON(r.withTabQuery(snapshotPath(*st.Snapshot)), &snap); err != nil {
			return err
		}
		return r.assertSnapshot(snap, *st.Snapshot)
	case st.Find != nil:
		var result findResult
		if err := r.client.getJSON(r.withTabQuery(findPath(*st.Find)), &result); err != nil {
			return err
		}
		return r.assertFind(result.Elements, *st.Find)
	case st.Read != nil:
		var read readability.PageRead
		if err := r.client.getJSON(r.withTabQuery("/api/page/read"), &read); err != nil {
			return err
		}
		return assertRead(read, *st.Read)
	case st.Click != nil:
		r.preClick = r.captureTabIDs()
		ref, err := r.resolveTarget(st.Click.Ref, st.Click.Target, st.Click.Match)
		if err != nil {
			return err
		}
		var result browser.ActionResult
		body := map[string]any{"ref": ref}
		r.addTabID(body)
		return r.client.postJSON("/api/page/click", body, &result)
	case st.Type != nil:
		ref, err := r.resolveTarget(st.Type.Ref, st.Type.Target, st.Type.Match)
		if err != nil {
			return err
		}
		var result browser.ActionResult
		body := map[string]any{"ref": ref, "text": expandVars(st.Type.Text)}
		r.addTabID(body)
		return r.client.postJSON("/api/page/type", body, &result)
	case st.Fill != nil:
		body := map[string]any{"text": expandVars(st.Fill.Text)}
		r.addTabID(body)
		if st.Fill.Query != "" || st.Fill.Role != "" {
			if st.Fill.Query != "" {
				body["query"] = expandVars(st.Fill.Query)
			}
			if st.Fill.Role != "" {
				body["role"] = st.Fill.Role
			}
		} else {
			ref, err := r.resolveTarget(st.Fill.Ref, st.Fill.Target, st.Fill.Match)
			if err != nil {
				return err
			}
			body["ref"] = ref
		}
		if st.Fill.Replace != nil {
			body["replace"] = *st.Fill.Replace
		}
		var result browser.ActionResult
		return r.client.postJSON("/api/page/fill", body, &result)
	case st.UploadFile != nil:
		body := map[string]any{}
		r.addTabID(body)
		if st.UploadFile.Path != "" {
			body["path"] = r.expandPath(st.UploadFile.Path)
		}
		if len(st.UploadFile.Paths) > 0 {
			paths := make([]string, 0, len(st.UploadFile.Paths))
			for _, path := range st.UploadFile.Paths {
				paths = append(paths, r.expandPath(path))
			}
			body["paths"] = paths
		}
		if st.UploadFile.Query != "" || st.UploadFile.Role != "" {
			if st.UploadFile.Query != "" {
				body["query"] = expandVars(st.UploadFile.Query)
			}
			if st.UploadFile.Role != "" {
				body["role"] = st.UploadFile.Role
			}
		} else {
			ref, err := r.resolveTarget(st.UploadFile.Ref, st.UploadFile.Target, st.UploadFile.Match)
			if err != nil {
				return err
			}
			body["ref"] = ref
		}
		var result browser.ActionResult
		return r.client.postJSON("/api/page/upload_file", body, &result)
	case st.Select != nil:
		ref, err := r.resolveTarget(st.Select.Ref, st.Select.Target, st.Select.Match)
		if err != nil {
			return err
		}
		var result browser.ActionResult
		body := map[string]any{"ref": ref, "value": expandVars(st.Select.Value)}
		r.addTabID(body)
		return r.client.postJSON("/api/page/select", body, &result)
	case st.Press != nil:
		var result browser.ActionResult
		body := map[string]any{"key": expandVars(st.Press.Key)}
		r.addTabID(body)
		return r.client.postJSON("/api/page/press", body, &result)
	case st.Scroll != nil:
		var result browser.ActionResult
		body := map[string]any{"direction": st.Scroll.Direction}
		r.addTabID(body)
		return r.client.postJSON("/api/page/scroll", body, &result)
	case st.Screenshot != nil:
		data, err := r.client.getBytes(r.withTabQuery("/api/visual/screenshot"))
		if err != nil {
			if st.Screenshot.Optional {
				fmt.Printf("WARN optional screenshot failed: %v\n", err)
				return nil
			}
			return err
		}
		if len(data) < st.Screenshot.MinBytes {
			return fmt.Errorf("screenshot too small: got %d bytes, want at least %d", len(data), st.Screenshot.MinBytes)
		}
		return nil
	case st.ScreenshotElement != nil:
		ref, err := r.resolveTarget(st.ScreenshotElement.Ref, st.ScreenshotElement.Target, st.ScreenshotElement.Match)
		if err != nil {
			return err
		}
		data, err := r.client.getBytes(r.withTabQuery("/api/visual/screenshot_element?ref=" + url.QueryEscape(ref)))
		if err != nil {
			if st.ScreenshotElement.Optional {
				fmt.Printf("WARN optional element screenshot failed: %v\n", err)
				return nil
			}
			return err
		}
		if len(data) < st.ScreenshotElement.MinBytes {
			return fmt.Errorf("element screenshot too small: got %d bytes, want at least %d", len(data), st.ScreenshotElement.MinBytes)
		}
		return nil
	case st.Commit != nil:
		ref, err := r.resolveTarget(st.Commit.Ref, st.Commit.Target, st.Commit.Match)
		if err != nil {
			return err
		}
		body := map[string]any{"ref": ref}
		r.addTabID(body)
		return r.client.postJSON("/api/page/commit", body, nil)
	case st.AssertVisible != nil:
		ref, err := r.resolveTarget(st.AssertVisible.Ref, st.AssertVisible.Target, nil)
		if err != nil {
			return err
		}
		timeout := st.AssertVisible.TimeoutMS
		if timeout == 0 {
			timeout = 5000
		}
		body := map[string]any{"ref": ref, "timeout_ms": timeout}
		r.addTabID(body)
		return r.client.postJSON("/api/page/assert_visible", body, nil)
	case st.AssertHidden != nil:
		ref, err := r.resolveTarget(st.AssertHidden.Ref, st.AssertHidden.Target, nil)
		if err != nil {
			return err
		}
		timeout := st.AssertHidden.TimeoutMS
		if timeout == 0 {
			timeout = 5000
		}
		body := map[string]any{"ref": ref, "timeout_ms": timeout}
		r.addTabID(body)
		return r.client.postJSON("/api/page/assert_hidden", body, nil)
	case st.AssertText != nil:
		ref, err := r.resolveTarget(st.AssertText.Ref, st.AssertText.Target, nil)
		if err != nil {
			return err
		}
		timeout := st.AssertText.TimeoutMS
		if timeout == 0 {
			timeout = 5000
		}
		body := map[string]any{"ref": ref, "text": st.AssertText.Text, "timeout_ms": timeout}
		r.addTabID(body)
		return r.client.postJSON("/api/page/assert_text", body, nil)
	case st.AssertValue != nil:
		ref, err := r.resolveTarget(st.AssertValue.Ref, st.AssertValue.Target, nil)
		if err != nil {
			return err
		}
		timeout := st.AssertValue.TimeoutMS
		if timeout == 0 {
			timeout = 5000
		}
		body := map[string]any{"ref": ref, "value": st.AssertValue.Value, "timeout_ms": timeout}
		r.addTabID(body)
		return r.client.postJSON("/api/page/assert_value", body, nil)
	case st.Assert != nil:
		return r.runAssertStep(*st.Assert)
	case st.Cookies != nil:
		return r.runCookiesStep(*st.Cookies)
	case st.GroupTabs != nil:
		return r.runGroupTabsStep(*st.GroupTabs)
	case st.UngroupTabs != nil:
		return r.runUngroupTabsStep(*st.UngroupTabs)
	case st.ListTabGroups != nil:
		return r.runListTabGroupsStep(*st.ListTabGroups)
	case st.ListTabs != nil:
		return r.runListTabsStep(*st.ListTabs)
	case st.CloseTab != nil:
		id, err := r.resolveTab(*st.CloseTab)
		if err != nil {
			return err
		}
		var result browser.ActionResult
		if err := r.client.postJSON("/api/browser/close", map[string]string{"id": id}, &result); err != nil {
			return err
		}
		if r.tabID == id {
			r.tabID = ""
		}
		return nil
	case st.WindowResize != nil:
		var result map[string]any
		return r.client.postJSON("/api/browser/resize_window", map[string]any{
			"width": st.WindowResize.Width, "height": st.WindowResize.Height,
		}, &result)
	case st.WindowBounds != nil:
		return r.runWindowBoundsStep(*st.WindowBounds)
	case st.Hover != nil:
		ref, err := r.resolveTarget(st.Hover.Ref, st.Hover.Target, st.Hover.Match)
		if err != nil {
			return err
		}
		body := map[string]any{"ref": ref}
		r.addTabID(body)
		var result browser.ActionResult
		return r.client.postJSON("/api/page/hover", body, &result)
	case st.ClickXY != nil:
		body := map[string]any{"x": st.ClickXY.X, "y": st.ClickXY.Y}
		r.addTabID(body)
		var result browser.ActionResult
		return r.client.postJSON("/api/page/click_xy", body, &result)
	case st.Drag != nil:
		// Saved keys resolve through the target parameter; resolveTarget
		// returns a ref verbatim, which would hand the daemon the key itself.
		from, err := r.resolveTarget("", st.Drag.From, nil)
		if err != nil {
			return err
		}
		to, err := r.resolveTarget("", st.Drag.To, nil)
		if err != nil {
			return err
		}
		body := map[string]any{
			"from": map[string]any{"ref": from},
			"to":   map[string]any{"ref": to},
		}
		r.addTabID(body)
		var result browser.ActionResult
		return r.client.postJSON("/api/page/drag", body, &result)
	case st.MouseDown != nil:
		return r.runMousePoint("/api/page/mouse_down", *st.MouseDown)
	case st.MouseUp != nil:
		return r.runMousePoint("/api/page/mouse_up", *st.MouseUp)
	case st.Batch != nil:
		return r.runBatchStep(*st.Batch)
	default:
		return errors.New("empty or unknown step")
	}
}

// runCookiesStep posts one brw_cookies action to the daemon and applies the
// step's list/set assertions to the result.
func (r *runner) runCookiesStep(st cookiesStep) (retErr error) {
	body := map[string]any{"action": st.Action}
	r.addTabID(body)
	if st.URL != "" {
		body["url"] = r.expandURL(st.URL)
	}
	for key, value := range map[string]any{
		"domain": st.Domain, "path": st.Path, "name": st.Name, "value": st.Value,
		"same_site": st.SameSite, "secure": st.Secure, "http_only": st.HTTPOnly,
	} {
		switch v := value.(type) {
		case string:
			if v != "" {
				body[key] = v
			}
		case bool:
			if v {
				body[key] = v
			}
		}
	}
	if st.Expires > 0 {
		body["expires"] = st.Expires
	}
	var result browser.CookieResult
	err := r.client.postJSON("/api/page/cookies", body, &result)
	if err != nil {
		// A scenario may be optional on transports lacking cookie support
		// (extension bridge); surface the daemon's refusal verbatim.
		return fmt.Errorf("cookies %s: %w", st.Action, err)
	}
	switch st.Action {
	case "list":
		return assertCookieList(result, st)
	case "set":
		if result.Cookie == nil {
			return fmt.Errorf("cookies set %q: daemon did not read the stored cookie back", st.Name)
		}
		if result.Cookie.Name != st.Name {
			return fmt.Errorf("cookies set read back %q, want %q", result.Cookie.Name, st.Name)
		}
		if st.ExpectValue != "" && result.Cookie.Value != st.ExpectValue {
			return fmt.Errorf("cookies set %q value = %q, want %q", st.Name, result.Cookie.Value, st.ExpectValue)
		}
		if st.HTTPOnly && !result.Cookie.HTTPOnly {
			return fmt.Errorf("cookies set %q: stored cookie is not http_only", st.Name)
		}
		return nil
	case "delete":
		if result.RemainingSameName != 0 {
			return fmt.Errorf("cookies delete %q: %d same-name cookie(s) remain", st.Name, result.RemainingSameName)
		}
		return nil
	}
	return fmt.Errorf("cookies step has unknown action %q", st.Action)
}

func assertCookieList(result browser.CookieResult, want cookiesStep) error {
	if result.Count < want.MinCount {
		return fmt.Errorf("cookie list has %d cookies, want at least %d", result.Count, want.MinCount)
	}
	present := map[string]browser.Cookie{}
	for _, c := range result.Cookies {
		present[c.Name] = c
	}
	for _, name := range want.Require {
		if _, ok := present[name]; !ok {
			return fmt.Errorf("cookie list is missing required cookie %q (have %d cookies)", name, result.Count)
		}
	}
	for _, name := range want.Absent {
		if _, ok := present[name]; ok {
			return fmt.Errorf("cookie %q still present after deletion/scrub", name)
		}
	}
	for _, name := range want.HTTPOnlyOf {
		c, ok := present[name]
		if !ok {
			return fmt.Errorf("cookie list is missing cookie %q required to be http_only", name)
		}
		if !c.HTTPOnly {
			return fmt.Errorf("cookie %q is not http_only (got http_only=false)", name)
		}
	}
	return nil
}

func (r *runner) assertSnapshot(snap snapshot.PageSnapshot, want snapshotStep) error {
	if len(snap.Elements) < want.MinElements {
		return fmt.Errorf("snapshot has %d elements, want at least %d", len(snap.Elements), want.MinElements)
	}
	for _, match := range want.Require {
		el, ok := findElement(snap.Elements, match)
		if !ok {
			return fmt.Errorf("missing element %s", describeMatch(match))
		}
		if match.SaveAs != "" {
			r.refs[match.SaveAs] = el.Ref
		}
	}
	return nil
}

func (r *runner) assertFind(elements []snapshot.Element, want findStep) error {
	if len(elements) < want.MinElements {
		return fmt.Errorf("find returned %d elements, want at least %d", len(elements), want.MinElements)
	}
	for _, match := range want.Require {
		el, ok := findElement(elements, match)
		if !ok {
			return fmt.Errorf("find missing element %s", describeMatch(match))
		}
		if match.SaveAs != "" {
			r.refs[match.SaveAs] = el.Ref
		}
	}
	return nil
}

func snapshotPath(step snapshotStep) string {
	values := url.Values{}
	addQuery(values, "mode", step.Mode)
	addQuery(values, "query", expandVars(step.Query))
	addQuery(values, "role", step.Role)
	addQuery(values, "text", expandVars(step.Text))
	addIntQuery(values, "limit", step.Limit)
	addBoolQuery(values, "viewport_only", step.ViewportOnly)
	if step.IncludeHidden {
		values.Set("include_hidden", "true")
	}
	addBoolQuery(values, "include_ax", step.IncludeAX)
	addQuery(values, "since", step.Since)
	addIntQuery(values, "max_bytes", step.MaxBytes)
	return pathWithQuery("/api/page/snapshot", values)
}

func findPath(step findStep) string {
	values := url.Values{}
	addQuery(values, "query", expandVars(step.Query))
	addQuery(values, "role", step.Role)
	addQuery(values, "text", expandVars(step.Text))
	addIntQuery(values, "limit", step.Limit)
	addBoolQuery(values, "viewport_only", step.ViewportOnly)
	if step.IncludeHidden {
		values.Set("include_hidden", "true")
	}
	return pathWithQuery("/api/page/find", values)
}

func pathWithQuery(path string, values url.Values) string {
	if encoded := values.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

func (r *runner) withTabQuery(path string) string {
	if r.tabID == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "tab_id=" + url.QueryEscape(r.tabID)
}

func (r *runner) addTabID(body map[string]any) {
	if r.tabID != "" {
		body["tab_id"] = r.tabID
	}
}

func addQuery(values url.Values, key, value string) {
	if value != "" {
		values.Set(key, value)
	}
}

func addIntQuery(values url.Values, key string, value int) {
	if value > 0 {
		values.Set(key, strconv.Itoa(value))
	}
}

func addBoolQuery(values url.Values, key string, value *bool) {
	if value != nil {
		values.Set(key, strconv.FormatBool(*value))
	}
}

func (r *runner) resolveTarget(ref, target string, match *elementMatch) (string, error) {
	if ref != "" {
		return ref, nil
	}
	if target != "" {
		if saved, ok := r.refs[target]; ok {
			return saved, nil
		}
		if strings.HasPrefix(target, "e") {
			return target, nil
		}
		return "", fmt.Errorf("unknown target %q", target)
	}
	if match == nil {
		return "", errors.New("target, ref, or match is required")
	}
	var snap snapshot.PageSnapshot
	if err := r.client.getJSON(r.withTabQuery("/api/page/snapshot"), &snap); err != nil {
		return "", err
	}
	el, ok := findElement(snap.Elements, *match)
	if !ok {
		return "", fmt.Errorf("missing element %s", describeMatch(*match))
	}
	if match.SaveAs != "" {
		r.refs[match.SaveAs] = el.Ref
	}
	return el.Ref, nil
}

func (r *runner) waitForTab(step tabStep) (browser.Tab, error) {
	timeout := time.Duration(step.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		tabs, err := r.client.listTabs()
		if err == nil {
			if tab, ok := matchTab(tabs, step, r.preClick); ok {
				return tab, nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return browser.Tab{}, err
			}
			return browser.Tab{}, fmt.Errorf("timed out waiting for tab %s", describeTabStep(step))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (r *runner) resolveTab(step tabStep) (string, error) {
	if step.ID != "" {
		return step.ID, nil
	}
	if step.Target != "" {
		if id, ok := r.tabRefs[step.Target]; ok {
			return id, nil
		}
		return "", fmt.Errorf("unknown tab target %q", step.Target)
	}
	tab, err := r.waitForTab(step)
	if err != nil {
		return "", err
	}
	return tab.ID, nil
}

func (r *runner) captureTabIDs() map[string]bool {
	tabs, err := r.client.listTabs()
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, tab := range tabs {
		out[tab.ID] = true
	}
	return out
}

func matchTab(tabs []browser.Tab, step tabStep, exclude map[string]bool) (browser.Tab, bool) {
	urlContains := expandVars(step.URLContains)
	titleContains := expandVars(step.TitleContains)
	var matched browser.Tab
	found := false
	for _, tab := range tabs {
		if step.NewOnly && exclude != nil && exclude[tab.ID] {
			continue
		}
		if step.ID != "" && tab.ID != step.ID {
			continue
		}
		if urlContains != "" && !containsFold(tab.URL, urlContains) {
			continue
		}
		if titleContains != "" && !containsFold(tab.Title, titleContains) {
			continue
		}
		matched = tab
		found = true
	}
	return matched, found
}

func findElement(elements []snapshot.Element, match elementMatch) (snapshot.Element, bool) {
	for _, el := range elements {
		if match.Role != "" && !equalFold(el.Role, match.Role) {
			continue
		}
		if match.Name != "" && !equalFold(el.Name, match.Name) {
			continue
		}
		if match.NameContains != "" && !containsFold(el.Name, match.NameContains) {
			continue
		}
		if match.Tag != "" && !equalFold(el.Tag, match.Tag) {
			continue
		}
		if match.Type != "" && !equalFold(el.Type, match.Type) {
			continue
		}
		if match.HrefContains != "" && !containsFold(el.Href, match.HrefContains) {
			continue
		}
		if match.Visible != nil && el.Visible != *match.Visible {
			continue
		}
		if match.Sensitive != nil && el.Sensitive != *match.Sensitive {
			continue
		}
		if match.ValueEmpty != nil {
			isEmpty := el.Value == ""
			if *match.ValueEmpty != isEmpty {
				continue
			}
		}
		return el, true
	}
	return snapshot.Element{}, false
}

func assertRead(read readability.PageRead, want readStep) error {
	if want.TitleContains != "" && !containsFold(read.Title, want.TitleContains) {
		return fmt.Errorf("title %q does not contain %q", read.Title, want.TitleContains)
	}
	if len(want.AnyTitleContains) > 0 && !containsAny(read.Title, want.AnyTitleContains) {
		return fmt.Errorf("title %q does not contain any of %v", read.Title, want.AnyTitleContains)
	}
	if want.URLContains != "" && !containsFold(read.URL, want.URLContains) {
		return fmt.Errorf("url %q does not contain %q", read.URL, want.URLContains)
	}
	for _, text := range want.MainContains {
		if !containsFold(read.Main, text) {
			return fmt.Errorf("main content does not contain %q", text)
		}
	}
	if len(want.AnyMainContains) > 0 && !containsAny(read.Main, want.AnyMainContains) {
		return fmt.Errorf("main content does not contain any of %v", want.AnyMainContains)
	}
	if len(read.Headings) < want.MinHeadings {
		return fmt.Errorf("read has %d headings, want at least %d", len(read.Headings), want.MinHeadings)
	}
	if len(read.Links) < want.MinLinks {
		return fmt.Errorf("read has %d links, want at least %d", len(read.Links), want.MinLinks)
	}
	if len(read.Forms) < want.MinForms {
		return fmt.Errorf("read has %d forms, want at least %d", len(read.Forms), want.MinForms)
	}
	if len(read.Tables) < want.MinTables {
		return fmt.Errorf("read has %d tables, want at least %d", len(read.Tables), want.MinTables)
	}
	if want.Metadata.DescriptionContains != "" && !containsFold(read.Metadata.Description, want.Metadata.DescriptionContains) {
		return fmt.Errorf("metadata description %q does not contain %q", read.Metadata.Description, want.Metadata.DescriptionContains)
	}
	if want.Metadata.CanonicalContains != "" && !containsFold(read.Metadata.Canonical, want.Metadata.CanonicalContains) {
		return fmt.Errorf("metadata canonical %q does not contain %q", read.Metadata.Canonical, want.Metadata.CanonicalContains)
	}
	if want.Metadata.Lang != "" && !equalFold(read.Metadata.Lang, want.Metadata.Lang) {
		return fmt.Errorf("metadata lang %q does not equal %q", read.Metadata.Lang, want.Metadata.Lang)
	}
	for _, fa := range want.Forms {
		found := false
		for _, form := range read.Forms {
			if fa.NameContains != "" && !containsFold(form.Name, fa.NameContains) {
				continue
			}
			if len(form.Controls) < fa.MinControls {
				continue
			}
			if fa.RequireControl != nil {
				ctrlMatch := false
				for _, ctrl := range form.Controls {
					if fa.RequireControl.NameContains != "" && !containsFold(ctrl.Name, fa.RequireControl.NameContains) {
						continue
					}
					if fa.RequireControl.Sensitive != nil && ctrl.Sensitive != *fa.RequireControl.Sensitive {
						continue
					}
					if fa.RequireControl.ValueEmpty != nil {
						isEmpty := ctrl.Value == ""
						if *fa.RequireControl.ValueEmpty != isEmpty {
							continue
						}
					}
					ctrlMatch = true
					break
				}
				if !ctrlMatch {
					continue
				}
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("missing form assertion %v", fa)
		}
	}
	return nil
}

func (r *runner) expandURL(raw string) string {
	raw = expandVars(raw)
	if strings.HasPrefix(raw, "${FIXTURES}/") {
		rel := strings.TrimPrefix(raw, "${FIXTURES}/")
		return fileURL(filepath.Join(r.repoRoot, "tests", "fixtures", rel))
	}
	if strings.HasPrefix(raw, "${HTTP_FIXTURES}/") {
		rel := strings.TrimPrefix(raw, "${HTTP_FIXTURES}/")
		base, err := r.httpFixturesBase()
		if err != nil {
			// A scenario URL that cannot be served is a suite setup failure;
			// returning the raw marker keeps the error identifiable.
			fmt.Printf("WARN http fixture server unavailable: %v\n", err)
			return raw
		}
		return base + "/" + rel
	}
	if strings.HasPrefix(raw, "${REPO_ROOT}/") {
		rel := strings.TrimPrefix(raw, "${REPO_ROOT}/")
		return fileURL(filepath.Join(r.repoRoot, rel))
	}
	return raw
}

// httpFixturesBase lazily starts a loopback HTTP server rooted at
// tests/fixtures and returns its base URL. Cookie scenarios need a real
// http(s) origin — Chrome does not store cookies for file:// pages — so the
// same fixture directory is also served over http://127.0.0.1:<ephemeral>.
// The listener intentionally lives for the process lifetime: brwcheck is
// short-lived and scenario cleanup only closes tabs, never this server.
func (r *runner) httpFixturesBase() (string, error) {
	var err error
	r.httpFixturesOnce.Do(func() {
		root := filepath.Join(r.repoRoot, "tests", "fixtures")
		ln, listenErr := net.Listen("tcp", "127.0.0.1:0")
		if listenErr != nil {
			err = listenErr
			return
		}
		r.httpFixturesLn = ln
		r.httpFixturesURL = "http://" + ln.Addr().String()
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			// Serve only files inside the fixtures root; no directory listing.
			rel := strings.TrimPrefix(path.Clean(req.URL.Path), "/")
			if rel == "" || strings.HasPrefix(rel, "..") {
				http.NotFound(w, req)
				return
			}
			http.ServeFile(w, req, filepath.Join(root, filepath.FromSlash(rel)))
		})
		go func() { _ = http.Serve(ln, mux) }()
	})
	if err != nil {
		return "", err
	}
	return r.httpFixturesURL, nil
}

func (r *runner) expandPath(raw string) string {
	raw = expandVars(raw)
	if strings.HasPrefix(raw, "${FIXTURES}/") {
		rel := strings.TrimPrefix(raw, "${FIXTURES}/")
		return filepath.Join(r.repoRoot, "tests", "fixtures", rel)
	}
	if strings.HasPrefix(raw, "${REPO_ROOT}/") {
		rel := strings.TrimPrefix(raw, "${REPO_ROOT}/")
		return filepath.Join(r.repoRoot, rel)
	}
	return raw
}

// health confirms the daemon is up and reports which transport it drives the
// browser over. /health already carries identity.transport; this used to decode
// the payload and throw it away, so the runner had no way to tell a scenario
// that cannot work here from one that is broken.
func (c *apiClient) health() (string, error) {
	var result struct {
		Identity struct {
			Transport string `json:"transport"`
		} `json:"identity"`
	}
	if err := c.getJSON("/health", &result); err != nil {
		return "", err
	}
	return result.Identity.Transport, nil
}

func (c *apiClient) listTabs() ([]browser.Tab, error) {
	var tabs []browser.Tab
	if err := c.getJSON("/api/browser/tabs", &tabs); err != nil {
		return nil, err
	}
	return tabs, nil
}

func (c *apiClient) getJSON(path string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, dst)
}

func (c *apiClient) postJSON(path string, body any, dst any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, dst)
}

func (c *apiClient) doJSON(req *http.Request, dst any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(data)))
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func (c *apiClient) getBytes(path string) ([]byte, error) {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %s: %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func loadSuite(path string) (suiteFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return suiteFile{}, err
	}
	var suite suiteFile
	if err := json.Unmarshal(data, &suite); err != nil {
		return suiteFile{}, err
	}
	if suite.Version != 1 {
		return suiteFile{}, fmt.Errorf("unsupported suite version %d", suite.Version)
	}
	if err := validateSuiteRequirements(suite); err != nil {
		return suiteFile{}, err
	}
	return suite, nil
}

// transportRequirements are the capability requirements a scenario may declare,
// each read off the transport's own properties rather than off its name.
//
// Naming lanes was the earlier spelling and it did not survive a third lane:
// every scenario that said "direct-cdp" was skipped against a daemon reporting
// chrome-opt-in-cdp, which is a lane with the same browser-target CDP, so the
// suite reported the whole lane as untested and a skip is not a failure. A
// property is true of a lane or it is not, and a lane added to brwidentity
// answers every one of these without a suite edit.
var transportRequirements = map[string]struct {
	explain string
	has     func(brwidentity.TransportCapabilities) bool
}{
	"cdp-session": {
		explain: "a DevTools Protocol session that survives between calls",
		has:     func(c brwidentity.TransportCapabilities) bool { return c.CDPSession },
	},
	"browser-target": {
		explain: "the CDP browser target (incognito contexts, browser-level cookies)",
		has:     func(c brwidentity.TransportCapabilities) bool { return c.BrowserTarget },
	},
	"extension-apis": {
		explain: "chrome.* extension APIs (tab groups)",
		has:     func(c brwidentity.TransportCapabilities) bool { return c.ExtensionAPIs },
	},
	"download-routing": {
		explain: "deciding at runtime where a completed download lands",
		has:     func(c brwidentity.TransportCapabilities) bool { return c.RuntimeDownloadRouting },
	},
}

// runFlagRequirements are the requirements answered by how brwcheck was
// invoked rather than by the lane.
var runFlagRequirements = map[string]string{
	"network": "--include-network",
	"auth":    "--include-auth",
	"manual":  "--include-manual",
}

// validateSuiteRequirements refuses a suite naming a requirement brwcheck does
// not know. It is a hard error and not a skip on purpose: a skip for a typo, or
// for a lane name this no longer understands, reads as "nothing to run here"
// and hides exactly the coverage gap it causes.
func validateSuiteRequirements(suite suiteFile) error {
	for _, sc := range suite.Scenarios {
		for _, req := range sc.Requires {
			if _, ok := transportRequirements[req]; ok {
				continue
			}
			if _, ok := runFlagRequirements[req]; ok {
				continue
			}
			known := make([]string, 0, len(transportRequirements)+len(runFlagRequirements))
			for name := range transportRequirements {
				known = append(known, name)
			}
			for name := range runFlagRequirements {
				known = append(known, name)
			}
			sort.Strings(known)
			return fmt.Errorf("scenario %s requires %q, which is not a requirement brwcheck knows; use one of %s", sc.ID, req, strings.Join(known, ", "))
		}
	}
	return nil
}

// skipReason reports why a scenario cannot run here, or "" to run it.
//
// A scenario naming a capability the lane does not have is SKIPPED, not failed.
// Some capabilities exist on a subset of lanes by construction: incognito
// contexts and HttpOnly cookie access need the CDP browser target the extension
// APIs do not expose, Chrome tab groups exist only in the extension APIs, and
// routing downloads is refused on a browser its user is signed into. Running
// the suite against a bridged daemon used to report "1 failed" for a capability
// that was never going to be there, which buries a real regression among
// expected noise.
func skipReason(sc scenario, includeNetwork, includeAuth, includeManual bool, transport string) string {
	flags := map[string]bool{"network": includeNetwork, "auth": includeAuth, "manual": includeManual}
	for _, req := range sc.Requires {
		if flag, ok := runFlagRequirements[req]; ok {
			if !flags[req] {
				return "requires " + flag
			}
			continue
		}
		rule, ok := transportRequirements[req]
		if !ok {
			// loadSuite refuses these, so reaching here means the suite was not
			// loaded through it. Say so rather than running a scenario whose
			// requirement nothing checked.
			return fmt.Sprintf("requires %q, which brwcheck does not classify", req)
		}
		// An empty transport means the daemon did not report one; run the
		// scenario rather than silently skipping the whole suite.
		if transport == "" {
			continue
		}
		caps, known := brwidentity.Capabilities(transport)
		if !known {
			return fmt.Sprintf("daemon reports transport %s, which brw does not classify, so %q cannot be checked", transport, req)
		}
		if !rule.has(caps) {
			return fmt.Sprintf("requires %s, which the %s transport does not have", rule.explain, transport)
		}
	}
	return ""
}

func parseOnly(raw string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not find repo root containing go.mod")
}

func fileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if containsFold(haystack, needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func describeMatch(match elementMatch) string {
	data, _ := json.Marshal(match)
	return string(data)
}

func describeTabStep(step tabStep) string {
	data, _ := json.Marshal(step)
	return string(data)
}

func expandVars(raw string) string {
	for {
		start := strings.Index(raw, "${ENV:")
		if start < 0 {
			return raw
		}
		end := strings.Index(raw[start:], "}")
		if end < 0 {
			return raw
		}
		end += start
		body := raw[start+len("${ENV:") : end]
		name := body
		fallback := ""
		if idx := strings.Index(body, ":"); idx >= 0 {
			name = body[:idx]
			fallback = body[idx+1:]
		}
		value := os.Getenv(name)
		if value == "" {
			value = fallback
		}
		raw = raw[:start] + value + raw[end+1:]
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "brwcheck: %v\n", err)
	os.Exit(1)
}
