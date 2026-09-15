package profileroster

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/setup"
)

// Create adds a dedicated direct-CDP profile at ~/.brw/profiles/<slug>.
// Re-creating an existing slug is a no-op that reports Created=false.
func Create(req CreateRequest) (CreateResult, error) {
	slug, err := Slug(req.Name)
	if err != nil {
		return CreateResult{}, err
	}
	browser := strings.ToLower(strings.TrimSpace(req.Browser))
	if browser == "" {
		browser = setup.BrowserChromium
	}
	if _, known := setup.LookupBrowser(browser); !known {
		return CreateResult{}, setup.UnknownBrowserError(browser)
	}
	home := req.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return CreateResult{}, err
		}
	}
	goos := req.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	policyPath := req.PolicyPath
	if policyPath == "" {
		policyPath, err = setup.DefaultPolicyPath(home)
		if err != nil {
			return CreateResult{}, err
		}
	}
	existing, _, err := setup.LoadPolicyFile(policyPath)
	if err != nil {
		return CreateResult{}, err
	}
	if profile, ferr := existing.Find(slug); ferr == nil {
		return CreateResult{Profile: profile, HTTPAddr: profile.BridgeHTTPAddr}, nil
	}

	port := nextHTTPPort(existing)
	udd := "~/.brw/profiles/" + slug
	workspace := Workspace(slug)
	merged, _ := setup.Merge(existing, setup.PolicyRequest{
		Workspace:   workspace,
		Profile:     slug,
		Browser:     browser,
		Transport:   setup.TransportDirectCDP,
		BRWDPath:    req.BRWDPath,
		HTTPPort:    port,
		Home:        home,
		GOOS:        goos,
		UserDataDir: udd,
	})
	if acc := strings.TrimSpace(req.Account); acc != "" {
		for i := range merged.Profiles {
			if merged.Profiles[i].Name == slug && len(merged.Profiles[i].Pins) == 0 {
				merged.Profiles[i].Pins = []profilepolicy.Pin{{
					Label:   "Google",
					Origin:  "https://accounts.google.com",
					Account: acc,
				}}
			}
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
	if err := os.MkdirAll(profilepolicy.ExpandPath(udd), 0o700); err != nil {
		return CreateResult{}, err
	}

	if req.InstallService {
		brwd := req.BRWDPath
		if brwd == "" {
			brwd = "brwd"
		}
		params := setup.ServiceParams{
			GOOS:       goos,
			Workspace:  workspace,
			Profile:    slug,
			PolicyPath: policyPath,
			BRWDPath:   brwd,
			HTTPAddr:   hostPort(port),
			LogPath:    setup.DefaultLogPath(goos, home, slug),
			Home:       home,
		}
		if err := installService(params); err != nil {
			return CreateResult{}, fmt.Errorf("wrote policy but failed to install the daemon service: %w", err)
		}
	}

	return CreateResult{Profile: profile, Created: true, HTTPAddr: profile.BridgeHTTPAddr}, nil
}

func installService(params setup.ServiceParams) error {
	if err := os.MkdirAll(filepath.Dir(params.LogPath), 0o755); err != nil {
		return err
	}
	switch params.GOOS {
	case "darwin":
		plist := setup.LaunchAgentPlist(params)
		path := params.UnitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			return err
		}
		uid := os.Getuid()
		gui := "gui/" + strconv.Itoa(uid)
		target := gui + "/" + params.Label()
		_ = exec.Command("launchctl", "bootout", target).Run()
		if err := exec.Command("launchctl", "bootstrap", gui, path).Run(); err != nil {
			if err := exec.Command("launchctl", "load", "-w", path).Run(); err != nil {
				return fmt.Errorf("launchctl bootstrap %s: %w", params.Label(), err)
			}
		}
		return nil
	case "windows":
		script := setup.WindowsLauncherScript(params)
		path := params.UnitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(script), 0o644)
	default:
		unit := setup.SystemdUnit(params)
		path := params.UnitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "enable", "--now", params.Label()+".service").Run()
	}
}
