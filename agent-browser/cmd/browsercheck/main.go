package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/revitt/agent-browser/internal/browser"
	"github.com/revitt/agent-browser/internal/readability"
	"github.com/revitt/agent-browser/internal/snapshot"
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
	var benchFixture string
	flag.BoolVar(&benchMode, "bench", false, "run direct-CDP speed benchmark against local fixtures")
	flag.BoolVar(&benchJSON, "bench-json", false, "emit benchmark scorecard as JSON")
	flag.StringVar(&benchFixture, "bench-fixture", "custom-combobox.html", "fixture file under tests/fixtures/")
	flag.StringVar(&suitePath, "suite", "tests/scenarios/core.json", "scenario suite JSON path")
	flag.StringVar(&baseURL, "base-url", envDefault("AGENT_BROWSER_URL", "http://127.0.0.1:17310"), "agent-browserd HTTP base URL")
	flag.StringVar(&repoRootFlag, "repo-root", envDefault("AGENT_BROWSER_REPO_ROOT", ""), "repo/share root containing tests and fixtures")
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
			Fixture:  benchFixture,
			JSON:     benchJSON,
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
	if err := c.health(); err != nil {
		fatal(fmt.Errorf("agent-browserd is not ready at %s: %w", baseURL, err))
	}

	selected := parseOnly(only)
	r := &runner{client: c, repoRoot: root}
	var passed, skipped, failed int
	for _, sc := range suite.Scenarios {
		if len(selected) > 0 && !selected[sc.ID] {
			continue
		}
		if reason := skipReason(sc, includeNetwork, includeAuth, includeManual); reason != "" {
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
	default:
		return errors.New("empty or unknown step")
	}
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
	if strings.HasPrefix(raw, "${REPO_ROOT}/") {
		rel := strings.TrimPrefix(raw, "${REPO_ROOT}/")
		return fileURL(filepath.Join(r.repoRoot, rel))
	}
	return raw
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

func (c *apiClient) health() error {
	var result map[string]any
	return c.getJSON("/health", &result)
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
	return suite, nil
}

func skipReason(sc scenario, includeNetwork, includeAuth, includeManual bool) string {
	for _, req := range sc.Requires {
		switch req {
		case "network":
			if !includeNetwork {
				return "requires --include-network"
			}
		case "auth":
			if !includeAuth {
				return "requires --include-auth"
			}
		case "manual":
			if !includeManual {
				return "requires --include-manual"
			}
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
	fmt.Fprintf(os.Stderr, "browsercheck: %v\n", err)
	os.Exit(1)
}
