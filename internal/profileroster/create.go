package profileroster

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// Create adds a direct-CDP profile with its own user-data-dir under
// ~/.brw/profiles/<slug> and its own control port. It writes the policy and the
// directory and nothing else: starting or installing the daemon is left to the
// commands the result names. Re-creating an existing slug changes nothing and
// reports Created=false.
func Create(req CreateRequest) (CreateResult, error) {
	slug, err := Slug(req.Name)
	if err != nil {
		return CreateResult{}, err
	}
	browserName := strings.ToLower(strings.TrimSpace(req.Browser))
	if browserName == "" {
		browserName = setup.BrowserChromium
	}
	if _, known := setup.LookupBrowser(browserName); !known {
		return CreateResult{}, setup.UnknownBrowserError(browserName)
	}
	home := req.Home
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return CreateResult{}, err
		}
	}
	goos := req.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	policyPath := req.PolicyPath
	if policyPath == "" {
		if policyPath, err = setup.DefaultPolicyPath(home); err != nil {
			return CreateResult{}, err
		}
	}
	existing, _, err := setup.LoadPolicyFile(policyPath)
	if err != nil {
		return CreateResult{}, err
	}
	workspace := Workspace(slug)
	if profile, ferr := existing.Find(slug); ferr == nil {
		return createResult(profile, false, req, policyPath, workspace), nil
	}

	udd := "~/.brw/profiles/" + slug
	if isRealProfile(expandHome(udd, home)) {
		return CreateResult{}, refuse("%s resolves into a daily browser profile directory", udd)
	}
	port := nextHTTPPort(existing)
	merged, _ := setup.Merge(existing, setup.PolicyRequest{
		Workspace: workspace,
		Profile:   slug,
		Browser:   browserName,
		Transport: setup.TransportDirectCDP,
		BRWDPath:  req.BRWDPath,
		HTTPPort:  port,
		Home:      home,
		GOOS:      goos,
	})
	for i := range merged.Profiles {
		p := &merged.Profiles[i]
		if p.Name != slug {
			continue
		}
		p.UserDataDir = udd
		p.ProfileDirectory = "Default"
		p.BridgeHTTPAddr = hostPort(port)
		if acc := strings.TrimSpace(req.Account); acc != "" {
			p.Pins = []profilepolicy.Pin{{Label: "Google", Origin: googleAccountsOrigin, Account: acc}}
		}
	}
	profile, err := merged.Find(slug)
	if err != nil {
		return CreateResult{}, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	if _, err := setup.WritePolicy(policyPath, merged, now); err != nil {
		return CreateResult{}, err
	}
	if err := os.MkdirAll(expandHome(udd, home), 0o700); err != nil {
		return CreateResult{}, err
	}
	return createResult(profile, true, req, policyPath, workspace), nil
}

func createResult(profile profilepolicy.Profile, created bool, req CreateRequest, policyPath, workspace string) CreateResult {
	brwd := req.BRWDPath
	if brwd == "" {
		brwd = "brwd"
	}
	params := setup.ServiceParams{
		Workspace:  workspace,
		Profile:    profile.Name,
		PolicyPath: policyPath,
		BRWDPath:   brwd,
		HTTPAddr:   profile.BridgeHTTPAddr,
	}
	service := []string{"brwctl", "setup", "--transport", setup.TransportDirectCDP,
		"--profile", profile.Name, "--workspace", workspace, "--profile-policy", policyPath}
	if port := portOf(profile.BridgeHTTPAddr); port > 0 {
		service = append(service, "--http-port", strconv.Itoa(port))
	}
	if profile.Kind != "" {
		service = append(service, "--browser", profile.Kind)
	}
	return CreateResult{
		Profile:        profile,
		Created:        created,
		HTTPAddr:       profile.BridgeHTTPAddr,
		RunCommand:     setup.Command(params.Args()),
		ServiceCommand: setup.Command(service),
	}
}

func expandHome(path, home string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok && home != "" {
		return home + string(os.PathSeparator) + rest
	}
	return profilepolicy.ExpandPath(path)
}

func nextHTTPPort(policy profilepolicy.Policy) int {
	high := setup.DefaultHTTPPort
	for _, p := range policy.Profiles {
		for _, addr := range []string{p.BridgeHTTPAddr, p.BridgeWSAddr} {
			if n := portOf(addr); n > high {
				high = n
			}
		}
	}
	return high + 2 - high%2
}

func portOf(addr string) int {
	addr = strings.TrimSuffix(strings.TrimSpace(addr), "/")
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return 0
	}
	return n
}

func hostPort(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}
