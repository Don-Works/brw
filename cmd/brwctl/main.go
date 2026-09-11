package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/mcp"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/recipe"
	"github.com/Don-Works/brw/internal/setup"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "setup":
		err = setupCommand(os.Args[2:])
	case "doctor":
		err = doctor(os.Args[2:])
	case "mcp-config":
		err = mcpConfig(os.Args[2:])
	case "remote-mcp-wrapper":
		err = remoteMCPWrapper(os.Args[2:])
	case "macos-policy":
		err = macOSPolicy(os.Args[2:])
	case "pack-extension":
		err = packExtension(os.Args[2:])
	case "update-xml":
		err = updateXML(os.Args[2:])
	case "daemons":
		err = daemons(os.Args[2:])
	case "recipe":
		err = recipeCommand(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: brwctl <command> [options]

commands:
  setup           take this machine to a working, connected brw bridge (idempotent, no sudo)
  doctor          verify profile policy, app install, and brw extension state
  mcp-config      print an MCP server config for a policy profile/transport
  remote-mcp-wrapper
                  write an SSH stdio wrapper for a remote brw bridge daemon
  macos-policy    write a Chrome ExtensionSettings .mobileconfig
  pack-extension  pack the brw Chrome extension as a CRX using installed Chrome
  update-xml      write a Chrome extension update manifest XML
  daemons         list configured bridge profile-daemons + probe each /health (JSON)
  recipe          draft, validate or atomically install a private deterministic recipe`)
}

func recipeCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: brwctl recipe <draft|validate|install> [options]")
	}
	switch args[0] {
	case "draft":
		return recipeDraft(args[1:])
	case "validate":
		return recipeValidate(args[1:])
	case "install":
		return recipeInstall(args[1:])
	default:
		return fmt.Errorf("unknown recipe command %q (want draft, validate or install)", args[0])
	}
}

type recipeCommandResult struct {
	OK      bool   `json:"ok"`
	Action  string `json:"action"`
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

func recipeValidate(args []string) error {
	fs := flag.NewFlagSet("recipe validate", flag.ContinueOnError)
	var source string
	fs.StringVar(&source, "file", "", "recipe JSON path, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if source == "" || fs.NArg() != 0 {
		return errors.New("recipe validate requires exactly one --file")
	}
	data, err := readRecipeSource(source, false)
	if err != nil {
		return err
	}
	value, err := recipe.Parse(data)
	if err != nil {
		return err
	}
	digest, err := recipe.Digest(value)
	if err != nil {
		return err
	}
	writeJSON(os.Stdout, recipeCommandResult{OK: true, Action: "validated", ID: value.ID, Version: value.Version, Digest: digest})
	return nil
}

func recipeInstall(args []string) error {
	fs := flag.NewFlagSet("recipe install", flag.ContinueOnError)
	var source, root string
	fs.StringVar(&source, "file", "", "recipe JSON path, or - for stdin")
	fs.StringVar(&root, "root", os.Getenv("BRW_RECIPE_ROOT"), "owner-only private recipe directory outside every Git checkout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if source == "" || fs.NArg() != 0 {
		return errors.New("recipe install requires exactly one --file")
	}
	if root == "" {
		var err error
		root, err = defaultPrivateRecipeRoot()
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(root) {
		return errors.New("recipe root must be absolute")
	}
	if source != "-" {
		absoluteSource, err := filepath.Abs(source)
		if err != nil {
			return err
		}
		if err := recipe.EnsureOutsideGitCheckout(absoluteSource); err != nil {
			return fmt.Errorf("recipe draft must not live in a Git checkout: %w", err)
		}
	}
	data, err := readRecipeSource(source, true)
	if err != nil {
		return err
	}
	value, err := recipe.Parse(data)
	if err != nil {
		return err
	}
	result, err := installPrivateRecipe(root, value)
	if err != nil {
		return err
	}
	writeJSON(os.Stdout, result)
	return nil
}

func defaultPrivateRecipeRoot() (string, error) {
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configRoot, "brw", "private-recipes"), nil
}

func readRecipeSource(source string, requirePrivate bool) ([]byte, error) {
	const limit = 1 << 20
	if source == "-" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, limit+1))
		if err != nil {
			return nil, err
		}
		if len(data) > limit {
			return nil, errors.New("recipe exceeds 1 MiB")
		}
		return data, nil
	}
	before, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recipe source must be a non-symlink regular file")
	}
	if requirePrivate && before.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("recipe source permissions %o are too broad; require 0600 or stricter", before.Mode().Perm())
	}
	if before.Size() > limit {
		return nil, errors.New("recipe exceeds 1 MiB")
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("recipe source changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("recipe exceeds 1 MiB")
	}
	return data, nil
}

func installPrivateRecipe(root string, value recipe.Recipe) (recipeCommandResult, error) {
	if err := recipe.Validate(value); err != nil {
		return recipeCommandResult{}, err
	}
	if err := recipe.PreparePrivateDirectory(root); err != nil {
		return recipeCommandResult{}, err
	}
	digest, err := recipe.Digest(value)
	if err != nil {
		return recipeCommandResult{}, err
	}
	catalog, err := recipe.LoadDirectory(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		return recipeCommandResult{}, err
	}
	if _, err := catalog.Fetch(context.Background(), value.ID, value.Version, digest); err == nil {
		return recipeCommandResult{OK: true, Action: "deduped", ID: value.ID, Version: value.Version, Digest: digest}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return recipeCommandResult{}, fmt.Errorf("immutable recipe %s@%s already exists with different content; create a new semantic version: %w", value.ID, value.Version, err)
	}

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return recipeCommandResult{}, err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(root, ".recipe-*.tmp")
	if err != nil {
		return recipeCommandResult{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return recipeCommandResult{}, err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return recipeCommandResult{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return recipeCommandResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return recipeCommandResult{}, err
	}
	destination := filepath.Join(root, value.ID+"@"+value.Version+".json")
	if err := os.Link(temporaryPath, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return recipeCommandResult{}, fmt.Errorf("immutable recipe path already exists; create a new semantic version: %w", err)
		}
		return recipeCommandResult{}, err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return recipeCommandResult{}, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return recipeCommandResult{}, err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return recipeCommandResult{}, err
	}
	if err := directory.Close(); err != nil {
		return recipeCommandResult{}, err
	}
	return recipeCommandResult{OK: true, Action: "installed", ID: value.ID, Version: value.Version, Digest: digest}, nil
}

// daemonRecord is one configured browser-profile bridge daemon, as emitted by
// `brwctl daemons`. It is the discovery contract a gateway (e.g. mcplexer)
// consumes to register one namespace per brw profile-daemon. http_addr/ws_addr
// are the daemon's loopback control + extension-bridge addresses; identity is the
// live /health identity when the daemon is reachable.
type daemonRecord struct {
	Name        string                `json:"name"`
	Kind        string                `json:"kind,omitempty"`
	Workspace   string                `json:"workspace,omitempty"`
	Profile     string                `json:"profile"`
	HTTPAddr    string                `json:"http_addr"`
	WSAddr      string                `json:"ws_addr"`
	ExtensionID string                `json:"extension_id,omitempty"`
	Reachable   bool                  `json:"reachable"`
	Identity    *brwidentity.Identity `json:"identity,omitempty"`
	Error       string                `json:"error,omitempty"`
}

// daemons enumerates every extension-bridge profile in the profile policy and
// probes each profile's daemon /health, emitting a JSON array of daemonRecord.
// It iterates policy.Profiles directly (not ResolveProfile) because the point is
// to list ALL configured daemons, not resolve one for a workspace.
func daemons(args []string) error {
	fs := flag.NewFlagSet("daemons", flag.ContinueOnError)
	var policyPath string
	var timeout time.Duration
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.DurationVar(&timeout, "timeout", 3*time.Second, "per-daemon /health probe timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}

	records := make([]daemonRecord, 0, len(policy.Profiles))
	for _, profile := range policy.Profiles {
		if !profile.ExtensionBridgeAllowed {
			continue
		}
		records = append(records, probeDaemon(profile, timeout))
	}

	writeJSON(os.Stdout, records)
	return nil
}

// probeDaemon builds the discovery record for one bridge profile: it derives the
// daemon's loopback addresses and extension id from the profile, then GETs the
// daemon's /health to fill in reachability and live identity. A probe failure is
// recorded (reachable=false + error), never fatal — an offline daemon must still
// appear in the listing so a consumer can decide whether to register it.
func probeDaemon(profile profilepolicy.Profile, timeout time.Duration) daemonRecord {
	extID := profile.BridgeExtensionID
	if extID == "" {
		extID = profilepolicy.DefaultBridgeExtensionID
	}
	httpURL := defaultBridgeHTTPURL(profile)
	rec := daemonRecord{
		Name:        profile.Name,
		Kind:        profile.Kind,
		Profile:     profile.Name,
		HTTPAddr:    httpURL,
		WSAddr:      defaultBridgeWSAddr(profile),
		ExtensionID: extID,
	}
	ctrl, cerr := httpclient.New(httpURL, timeout)
	if cerr != nil {
		rec.Error = cerr.Error()
		return rec
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	health, herr := ctrl.Health(ctx)
	if herr != nil {
		rec.Error = herr.Error()
		return rec
	}
	rec.Reachable = true
	if !health.Identity.Empty() {
		id := health.Identity
		rec.Identity = &id
		rec.Workspace = health.Identity.Workspace
	}
	return rec
}

// doctorWarning is a named, non-fatal finding. The name is the stable handle a
// consumer matches on; the message is what a human reads.
type doctorWarning struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// doctorResult is the `brwctl doctor` JSON contract. Fields are only ever added
// to it: an existing consumer keeps reading the keys it already knows.
type doctorResult struct {
	Profile                  string              `json:"profile"`
	Kind                     string              `json:"kind"`
	AppDir                   string              `json:"app_dir"`
	ProfilePolicyPath        string              `json:"profile_policy_path,omitempty"`
	ChromeProfileDir         string              `json:"chrome_profile_dir"`
	BridgeExtensionID        string              `json:"bridge_extension_id,omitempty"`
	BridgeExtensionInstalled *bool               `json:"bridge_extension_installed,omitempty"`
	BridgeExtensionSource    string              `json:"bridge_extension_source,omitempty"`
	Transport                string              `json:"transport,omitempty"`
	Capabilities             *setup.Capabilities `json:"capabilities,omitempty"`
	Warnings                 []doctorWarning     `json:"warnings,omitempty"`
	OK                       bool                `json:"ok"`
	Failures                 []string            `json:"failures,omitempty"`
}

// doctorRequest is what doctorReport needs. Policy is optional: `brwctl setup`
// passes the policy it has just merged in memory so --dry-run can verify a
// configuration that is not on disk yet.
type doctorRequest struct {
	Workspace  string
	Profile    string
	PolicyPath string
	AppDir     string
	Home       string
	Policy     *profilepolicy.Policy
}

func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var profileName, workspaceName, policyPath, appDir string
	fs.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace profile name")
	fs.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&appDir, "app-dir", defaultAppDir(), "brw app install directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" && workspaceName == "" {
		return errors.New("--profile or --workspace is required")
	}
	home, _ := os.UserHomeDir()
	report, err := doctorReport(doctorRequest{
		Workspace:  workspaceName,
		Profile:    profileName,
		PolicyPath: policyPath,
		AppDir:     appDir,
		Home:       home,
	})
	if err != nil {
		return err
	}
	writeJSON(os.Stdout, report)
	if !report.OK {
		return errors.New("doctor failed")
	}
	return nil
}

func doctorReport(req doctorRequest) (doctorResult, error) {
	policyPath := req.PolicyPath
	policy := profilepolicy.Policy{}
	if req.Policy != nil {
		policy = *req.Policy
	} else {
		if policyPath == "" {
			discovered, err := profilepolicy.Discover("")
			if err != nil {
				return doctorResult{}, err
			}
			policyPath = discovered
		}
		loaded, err := profilepolicy.Load(policyPath)
		if err != nil {
			return doctorResult{}, err
		}
		policy = loaded
	}
	profile, err := policy.ResolveProfile(req.Workspace, req.Profile)
	if err != nil {
		return doctorResult{}, err
	}

	report := doctorResult{
		Profile:           profile.Name,
		Kind:              profile.Kind,
		AppDir:            req.AppDir,
		ProfilePolicyPath: policyPath,
	}
	var failures []string
	for _, rel := range []string{
		"bin/brwd",
		"bin/brwcheck",
		"bin/brw-devtools-mcp",
		"extension/manifest.json",
	} {
		path := filepath.Join(req.AppDir, rel)
		if _, err := os.Stat(path); err != nil {
			failures = append(failures, "missing "+path)
		}
	}
	// The app-directory copy of the policy is what `make install-mac` syncs for
	// a remote push; it is not the policy this run loaded, and a machine set up
	// by `brwctl setup` legitimately has none. Report it, do not fail on it.
	appPolicy := filepath.Join(req.AppDir, "config", "browser-profiles.json")
	if _, err := os.Stat(appPolicy); err != nil {
		report.Warnings = append(report.Warnings, doctorWarning{
			Name:    "app_dir_policy_copy_missing",
			Message: "no policy copy at " + appPolicy + "; the policy in use is " + policyPath,
			Detail:  "only needed when pushing this install to another machine",
		})
	}

	profileDir := filepath.Join(profilepolicy.ExpandPath(profile.UserDataDir), profile.ProfileDirectory)
	report.ChromeProfileDir = profileDir
	if _, err := os.Stat(profileDir); err != nil {
		failures = append(failures, "missing Chrome profile dir "+profileDir)
	}

	if profile.ExtensionBridgeAllowed {
		// Match the daemon: an unconfigured bridge already trusts the published
		// extension id, so doctor must verify against the same id rather than
		// failing a policy that simply did not repeat it.
		id := profile.BridgeExtensionID
		if id == "" {
			id = profilepolicy.DefaultBridgeExtensionID
		}
		report.BridgeExtensionID = id
		installed, source, err := chromeExtensionInstalled(profileDir, id)
		report.BridgeExtensionInstalled = &installed
		if source != "" {
			report.BridgeExtensionSource = source
		}
		if err != nil {
			failures = append(failures, err.Error())
		} else if !installed {
			failures = append(failures, "brw extension "+id+" is not installed in "+profileDir)
		}
	}

	// Name the lane and its capability gap. Both transports are complete
	// browsers, but incognito, HttpOnly cookies and download routing exist on
	// one and Chrome tab groups on the other, and nothing else tells the user
	// which one their install chose.
	if transport := setup.ResolvedTransport(profile); transport != "" {
		capabilities := setup.CapabilitiesFor(transport)
		report.Transport = transport
		report.Capabilities = &capabilities
	}

	if state := setup.DetectClaudeInChrome(setup.ClaudeConfigPath(req.Home)); state.Enabled {
		report.Warnings = append(report.Warnings, doctorWarning{
			Name:    setup.ClaudeInChromeWarning,
			Message: "Claude Code's own Chrome integration is enabled; run /chrome in Claude Code and turn it off, or the agent sees two browser tool sets and may drive the wrong browser",
			Detail:  strings.Join(state.Signals, ", ") + " in " + state.Path,
		})
	}

	report.OK = len(failures) == 0
	report.Failures = failures
	return report, nil
}

// mcpConfigRequest is one resolved MCP server derivation. `brwctl setup` builds
// the same request so the command it registers with an MCP client is byte-for-byte
// what `brwctl mcp-config` prints.
type mcpConfigRequest struct {
	Workspace  string
	Profile    string
	Transport  string
	PolicyPath string
	Mode       string
}

// mcpServerSpec is the launchable form of an MCP server entry.
type mcpServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Mode    string
	Profile profilepolicy.Profile
}

// configJSON renders the spec as the mcpServers block an MCP client config file
// expects, for the clients setup cannot drive through a CLI.
func (s mcpServerSpec) configJSON() string {
	entry := map[string]any{
		"command": s.Command,
		"args":    s.Args,
	}
	if len(s.Env) > 0 {
		entry["env"] = s.Env
	}
	data, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{s.Name: entry}}, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// deriveMCPServer resolves a profile and transport against a policy and returns
// the exact command, arguments and environment an MCP client must run.
func deriveMCPServer(policy profilepolicy.Policy, req mcpConfigRequest) (mcpServerSpec, error) {
	profile, err := policy.ResolveProfile(req.Workspace, req.Profile)
	if err != nil {
		return mcpServerSpec{}, err
	}
	transport, err := policy.ResolveTransport(req.Workspace, req.Transport)
	if err != nil {
		return mcpServerSpec{}, err
	}
	mode := req.Mode
	if mode == "auto" {
		if profile.DirectCDPAllowed {
			mode = "direct"
		} else if profile.ExtensionBridgeAllowed {
			mode = "bridge"
		} else {
			return mcpServerSpec{}, fmt.Errorf("profile %q has no allowed runtime mode", profile.Name)
		}
	}
	if mode == "bridge" && !profile.ExtensionBridgeAllowed {
		return mcpServerSpec{}, fmt.Errorf("profile %q is not allowed for bridge mode", profile.Name)
	}
	if mode == "upstream-http" && !profile.ExtensionBridgeAllowed && !profile.DirectCDPAllowed {
		return mcpServerSpec{}, fmt.Errorf("profile %q is not allowed for upstream HTTP mode", profile.Name)
	}
	if mode == "direct" && !profile.DirectCDPAllowed {
		return mcpServerSpec{}, fmt.Errorf("profile %q is not allowed for direct mode", profile.Name)
	}

	envPolicyPath := req.PolicyPath
	if transport.Kind == "ssh-stdio" {
		envPolicyPath = remotePolicyPath(transport)
	}
	envOut := runtimeEnv(req.Workspace, profile.Name, envPolicyPath)
	runtimeOut := runtimeArgs(mode, profile)
	argsOut := append([]string{}, runtimeOut...)
	command := ""
	switch transport.Kind {
	case "stdio":
		command = transport.Command
		if command == "" {
			command = "brwd"
		}
	case "ssh-stdio":
		host := transport.Host
		if transport.User != "" {
			host = transport.User + "@" + host
		}
		command = transport.Command
		if command == "" {
			command = "ssh"
		}
		commandArgs := append([]string{}, transport.CommandArgs...)
		remoteArgs := append([]string{remoteBinary(transport, "brwd")}, runtimeOut...)
		argsOut = append(commandArgs, host, shellJoin(append(shellEnv(envOut), remoteArgs...)))
		envOut = nil
	default:
		return mcpServerSpec{}, fmt.Errorf("transport %q has unsupported kind %q for stdio MCP config", transport.Name, transport.Kind)
	}

	name := "brw"
	if transport.Name != "local" {
		name += "-" + transport.Name
	}
	return mcpServerSpec{Name: name, Command: command, Args: argsOut, Env: envOut, Mode: mode, Profile: profile}, nil
}

func mcpConfig(args []string) error {
	fs := flag.NewFlagSet("mcp-config", flag.ContinueOnError)
	var profileName, workspaceName, transportName, policyPath, mode string
	fs.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace profile name")
	fs.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	fs.StringVar(&transportName, "transport", os.Getenv("BRW_TRANSPORT"), "workspace transport name")
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&mode, "mode", "auto", "auto, direct, bridge, or upstream-http")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" && workspaceName == "" {
		return errors.New("--profile or --workspace is required")
	}
	if transportName == "" && workspaceName == "" {
		transportName = "local"
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	spec, err := deriveMCPServer(policy, mcpConfigRequest{
		Workspace:  workspaceName,
		Profile:    profileName,
		Transport:  transportName,
		PolicyPath: policyPath,
		Mode:       mode,
	})
	if err != nil {
		return err
	}
	entry := map[string]any{
		"command": spec.Command,
		"args":    spec.Args,
	}
	if len(spec.Env) > 0 {
		entry["env"] = spec.Env
	}
	writeJSON(os.Stdout, map[string]any{"mcpServers": map[string]any{spec.Name: entry}})
	return nil
}

type repeatFlag []string

func (r *repeatFlag) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

func (r *repeatFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

type remoteMCPWrapperOptions struct {
	Host                  string
	User                  string
	RemoteBRWD            string
	RemoteHTTP            string
	MCPTools              string
	SSH                   string
	ConnectTimeout        string
	ConnectionAttempts    string
	ServerAliveInterval   string
	ServerAliveCountMax   string
	KnownHosts            string
	StrictHostKeyChecking string
	IdentityFile          string
	Compression           bool
	LogPath               string
	LogMaxBytes           string
	SSHOptions            []string
}

func remoteMCPWrapper(args []string) error {
	fs := flag.NewFlagSet("remote-mcp-wrapper", flag.ContinueOnError)
	var opts remoteMCPWrapperOptions
	var output string
	var sshOptions repeatFlag
	appDir := defaultAppDir()
	fs.StringVar(&opts.Host, "host", os.Getenv("BRW_REMOTE_HOST"), "SSH host where the browser daemon runs")
	fs.StringVar(&opts.User, "user", os.Getenv("BRW_REMOTE_USER"), "optional SSH user; omit when --host already includes user@")
	fs.StringVar(&opts.RemoteBRWD, "remote-brwd", envDefault("BRW_REMOTE_BRWD", "brwd"), "remote brwd path; ~/ is expanded by the remote shell")
	fs.StringVar(&opts.RemoteHTTP, "remote-http", envDefault("BRW_REMOTE_HTTP", "http://127.0.0.1:17310"), "remote loopback HTTP API used by the stdio wrapper")
	fs.StringVar(&opts.MCPTools, "mcp-tools", envDefault("BRW_MCP_TOOLS", "auto"), "MCP tool surface: all, core, minimal, or auto")
	fs.StringVar(&opts.SSH, "ssh", envDefault("BRW_SSH", "ssh"), "local SSH executable")
	fs.StringVar(&opts.ConnectTimeout, "connect-timeout", envDefault("BRW_CONNECT_TIMEOUT", "5"), "SSH ConnectTimeout seconds")
	fs.StringVar(&opts.ConnectionAttempts, "connection-attempts", envDefault("BRW_CONNECTION_ATTEMPTS", "1"), "SSH ConnectionAttempts for the initial connection; raise for flaky links")
	fs.StringVar(&opts.ServerAliveInterval, "server-alive-interval", envDefault("BRW_SERVER_ALIVE_INTERVAL", "30"), "SSH ServerAliveInterval seconds; 0 disables keepalives. Detects a dropped link instead of hanging the MCP client")
	fs.StringVar(&opts.ServerAliveCountMax, "server-alive-count-max", envDefault("BRW_SERVER_ALIVE_COUNT_MAX", "3"), "SSH ServerAliveCountMax: missed keepalives before the link is dropped")
	fs.StringVar(&opts.KnownHosts, "known-hosts", filepath.Join(appDir, "ssh", "known_hosts"), "dedicated known_hosts path for this wrapper")
	fs.StringVar(&opts.StrictHostKeyChecking, "strict-host-key-checking", envDefault("BRW_STRICT_HOST_KEY_CHECKING", "accept-new"), "SSH StrictHostKeyChecking value: yes, accept-new, or no")
	fs.StringVar(&opts.IdentityFile, "identity-file", os.Getenv("BRW_IDENTITY_FILE"), "optional SSH identity file; when set, only this key is offered (IdentitiesOnly=yes) to avoid agent key churn / lockout")
	fs.BoolVar(&opts.Compression, "compression", false, "enable SSH compression; helps text payloads on slow links, skip on fast links / PNG screenshots")
	fs.StringVar(&opts.LogPath, "log", filepath.Join(appDir, "remote-mcp.log"), "stderr log path for SSH and remote brwd startup messages")
	fs.StringVar(&opts.LogMaxBytes, "log-max-bytes", envDefault("BRW_LOG_MAX_BYTES", "5242880"), "rotate the stderr log at launch when it reaches this size in bytes; 0 disables rotation")
	fs.Var(&sshOptions, "ssh-option", "extra ssh -o option; repeatable, for example ProxyJump=bastion")
	fs.StringVar(&output, "output", "", "output wrapper path; stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts.SSHOptions = sshOptions
	if opts.Host == "" {
		return errors.New("--host is required")
	}
	if opts.User != "" && strings.Contains(opts.Host, "@") {
		return errors.New("--user cannot be used when --host already includes user@host")
	}
	switch opts.StrictHostKeyChecking {
	case "yes", "accept-new", "no":
	default:
		return errors.New("--strict-host-key-checking must be yes, accept-new, or no")
	}
	if !mcp.ValidToolProfile(opts.MCPTools) {
		return fmt.Errorf("--mcp-tools must be one of %s", strings.Join(mcp.ToolProfileNames(), ", "))
	}
	// Validate the numeric SSH/log knobs so the generated POSIX script never
	// emits a value that ssh rejects or that the log-rotation guard mishandles.
	// 0 is a meaningful "disabled" value for keepalives and log rotation, but
	// OpenSSH rejects ConnectionAttempts=0, so it must be strictly positive.
	for _, nf := range []struct{ name, value string }{
		{"server-alive-interval", opts.ServerAliveInterval},
		{"server-alive-count-max", opts.ServerAliveCountMax},
		{"log-max-bytes", opts.LogMaxBytes},
	} {
		if n, err := strconv.Atoi(nf.value); err != nil || n < 0 {
			return fmt.Errorf("--%s must be a non-negative integer", nf.name)
		}
	}
	if n, err := strconv.Atoi(opts.ConnectionAttempts); err != nil || n < 1 {
		return errors.New("--connection-attempts must be a positive integer")
	}
	if opts.RemoteBRWD == "" {
		return errors.New("--remote-brwd is required")
	}
	if opts.RemoteHTTP == "" {
		return errors.New("--remote-http is required")
	}
	if opts.KnownHosts == "" {
		return errors.New("--known-hosts is required")
	}
	if opts.LogPath == "" {
		return errors.New("--log is required")
	}
	return writeOutput(output, []byte(remoteMCPWrapperScript(opts)), 0o755)
}

func macOSPolicy(args []string) error {
	fs := flag.NewFlagSet("macos-policy", flag.ContinueOnError)
	var profileName, workspaceName, policyPath, updateURL, installMode, output string
	fs.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace profile name")
	fs.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&updateURL, "update-url", "", "Chrome extension update URL")
	fs.StringVar(&installMode, "install-mode", "normal_installed", "normal_installed or force_installed")
	fs.StringVar(&output, "output", "", "output .mobileconfig path; stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" && workspaceName == "" {
		return errors.New("--profile or --workspace is required")
	}
	if updateURL == "" {
		return errors.New("--update-url is required")
	}
	if installMode != "normal_installed" && installMode != "force_installed" {
		return errors.New("--install-mode must be normal_installed or force_installed")
	}

	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.ResolveProfile(workspaceName, profileName)
	if err != nil {
		return err
	}
	id := profile.BridgeExtensionID
	if id == "" {
		return errors.New("bridge_extension_id is required in profile policy for managed Chrome policy generation")
	}
	data := []byte(chromePolicyMobileconfig(id, installMode, updateURL))
	return writeOutput(output, data, 0o644)
}

func packExtension(args []string) error {
	fs := flag.NewFlagSet("pack-extension", flag.ContinueOnError)
	var chromePath, extensionDir, keyPath, outDir string
	fs.StringVar(&chromePath, "chrome-path", "", "Chrome executable path")
	fs.StringVar(&extensionDir, "extension-dir", "extension", "extension directory")
	fs.StringVar(&keyPath, "key", "", "optional Chrome extension signing key path")
	fs.StringVar(&outDir, "out-dir", "dist/extension", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	chrome, err := cdp.FindChrome(chromePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	absExt, err := filepath.Abs(extensionDir)
	if err != nil {
		return err
	}
	cmdArgs := []string{"--pack-extension=" + absExt}
	if keyPath != "" {
		absKey, err := filepath.Abs(keyPath)
		if err != nil {
			return err
		}
		cmdArgs = append(cmdArgs, "--pack-extension-key="+absKey)
	}
	cmd := exec.Command(chrome, cmdArgs...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pack extension: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	src := absExt + ".crx"
	dst := filepath.Join(outDir, "brw.crx")
	if err := copyFile(src, dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	fmt.Fprintf(os.Stderr, "wrote %s\n", dst)
	return nil
}

func updateXML(args []string) error {
	fs := flag.NewFlagSet("update-xml", flag.ContinueOnError)
	var profileName, workspaceName, policyPath, crxURL, version, output string
	fs.StringVar(&profileName, "profile", os.Getenv("BRW_PROFILE"), "workspace profile name")
	fs.StringVar(&workspaceName, "workspace", os.Getenv("BRW_WORKSPACE"), "workspace binding name for default/restricted profiles")
	fs.StringVar(&policyPath, "profile-policy", os.Getenv("BRW_PROFILE_POLICY"), "profile policy JSON path")
	fs.StringVar(&crxURL, "crx-url", "", "absolute URL to brw.crx")
	fs.StringVar(&version, "version", extensionVersion("extension/manifest.json"), "extension version")
	fs.StringVar(&output, "output", "", "output XML path; stdout when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if profileName == "" && workspaceName == "" {
		return errors.New("--profile or --workspace is required")
	}
	if crxURL == "" {
		return errors.New("--crx-url is required")
	}
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return err
	}
	profile, err := policy.ResolveProfile(workspaceName, profileName)
	if err != nil {
		return err
	}
	id := profile.BridgeExtensionID
	if id == "" {
		return errors.New("bridge_extension_id is required in profile policy for extension update XML")
	}
	data := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<gupdate xmlns="http://www.google.com/update2/response" protocol="2.0">
  <app appid="%s">
    <updatecheck codebase="%s" version="%s" />
  </app>
</gupdate>
`, xmlEscape(id), xmlEscape(crxURL), xmlEscape(version)))
	return writeOutput(output, data, 0o644)
}

func chromeExtensionInstalled(profileDir, id string) (bool, string, error) {
	for _, name := range []string{"Preferences", "Secure Preferences"} {
		path := filepath.Join(profileDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, "", err
		}
		var prefs struct {
			Extensions struct {
				Settings map[string]json.RawMessage `json:"settings"`
			} `json:"extensions"`
		}
		if err := json.Unmarshal(data, &prefs); err != nil {
			continue
		}
		if _, ok := prefs.Extensions.Settings[id]; ok {
			return true, path, nil
		}
	}
	return false, "", nil
}

func runtimeArgs(mode string, profile profilepolicy.Profile) []string {
	args := []string{"--mcp", "--http", "off"}
	if mode == "bridge" {
		args = append([]string{"--bridge"}, args...)
		args = append(args, "--bridge-addr", defaultBridgeWSAddr(profile))
	}
	if mode == "upstream-http" {
		args = append(args, "--upstream-http", defaultBridgeHTTPURL(profile))
	}
	return args
}

func defaultBridgeWSAddr(profile profilepolicy.Profile) string {
	if strings.TrimSpace(profile.BridgeWSAddr) != "" {
		return strings.TrimSpace(profile.BridgeWSAddr)
	}
	return "127.0.0.1:17311"
}

func defaultBridgeHTTPURL(profile profilepolicy.Profile) string {
	addr := strings.TrimSpace(profile.BridgeHTTPAddr)
	if addr == "" {
		addr = "127.0.0.1:17310"
	}
	if strings.Contains(addr, "://") {
		return addr
	}
	return "http://" + addr
}

func runtimeEnv(workspace, profile, policyPath string) map[string]string {
	env := map[string]string{}
	if workspace != "" {
		env["BRW_WORKSPACE"] = workspace
	}
	if profile != "" {
		env["BRW_PROFILE"] = profile
	}
	if policyPath != "" {
		env["BRW_PROFILE_POLICY"] = policyPath
	}
	return env
}

func remoteBinary(t profilepolicy.Transport, name string) string {
	if t.AppDir == "" {
		return name
	}
	return filepath.Join(t.AppDir, "bin", name)
}

func remotePolicyPath(t profilepolicy.Transport) string {
	if t.AppDir == "" {
		return ""
	}
	return filepath.Join(t.AppDir, "config", "browser-profiles.json")
}

func remoteMCPWrapperScript(opts remoteMCPWrapperOptions) string {
	target := opts.Host
	if opts.User != "" {
		target = opts.User + "@" + target
	}
	remoteCommand := shellJoin([]string{
		opts.RemoteBRWD,
		"--upstream-http", opts.RemoteHTTP,
		"--mcp",
		"--http", "off",
		"--mcp-tools", opts.MCPTools,
	})

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n\n")
	fmt.Fprintf(&b, "BRW_SSH=${BRW_SSH:-%s}\n", quoteScript(opts.SSH))
	fmt.Fprintf(&b, "BRW_REMOTE=${BRW_REMOTE:-%s}\n", quoteScript(target))
	fmt.Fprintf(&b, "BRW_CONNECT_TIMEOUT=${BRW_CONNECT_TIMEOUT:-%s}\n", quoteScript(opts.ConnectTimeout))
	fmt.Fprintf(&b, "BRW_CONNECTION_ATTEMPTS=${BRW_CONNECTION_ATTEMPTS:-%s}\n", quoteScript(opts.ConnectionAttempts))
	fmt.Fprintf(&b, "BRW_SERVER_ALIVE_INTERVAL=${BRW_SERVER_ALIVE_INTERVAL:-%s}\n", quoteScript(opts.ServerAliveInterval))
	fmt.Fprintf(&b, "BRW_SERVER_ALIVE_COUNT_MAX=${BRW_SERVER_ALIVE_COUNT_MAX:-%s}\n", quoteScript(opts.ServerAliveCountMax))
	fmt.Fprintf(&b, "BRW_KNOWN_HOSTS=${BRW_KNOWN_HOSTS:-%s}\n", quoteScript(opts.KnownHosts))
	fmt.Fprintf(&b, "BRW_STRICT_HOST_KEY_CHECKING=${BRW_STRICT_HOST_KEY_CHECKING:-%s}\n", quoteScript(opts.StrictHostKeyChecking))
	fmt.Fprintf(&b, "BRW_LOG=${BRW_LOG:-%s}\n", quoteScript(opts.LogPath))
	fmt.Fprintf(&b, "BRW_LOG_MAX_BYTES=${BRW_LOG_MAX_BYTES:-%s}\n\n", quoteScript(opts.LogMaxBytes))
	b.WriteString("mkdir -p \"$(dirname \"$BRW_KNOWN_HOSTS\")\" \"$(dirname \"$BRW_LOG\")\"\n\n")
	// Rotate the stderr log at launch once it grows past the cap (single
	// generation). Stops an unattended reconnect loop from filling the disk.
	b.WriteString("if [ \"$BRW_LOG_MAX_BYTES\" -gt 0 ] && [ -f \"$BRW_LOG\" ]; then\n")
	b.WriteString("  brw_log_size=$(wc -c < \"$BRW_LOG\" 2>/dev/null || echo 0)\n")
	b.WriteString("  if [ \"$brw_log_size\" -ge \"$BRW_LOG_MAX_BYTES\" ]; then\n")
	b.WriteString("    mv -f \"$BRW_LOG\" \"$BRW_LOG.1\" 2>/dev/null || :\n")
	b.WriteString("  fi\n")
	b.WriteString("fi\n\n")
	b.WriteString("exec \"$BRW_SSH\" \\\n")
	b.WriteString("  -o BatchMode=yes \\\n")
	// No TTY: keeps the binary MCP stdio stream clean even if the operator's
	// ssh_config forces RequestTTY.
	b.WriteString("  -o RequestTTY=no \\\n")
	b.WriteString("  -o ConnectTimeout=\"$BRW_CONNECT_TIMEOUT\" \\\n")
	b.WriteString("  -o ConnectionAttempts=\"$BRW_CONNECTION_ATTEMPTS\" \\\n")
	// Keepalives so a silently dropped link (sleep / NAT rebind / wifi switch)
	// fails the wrapper promptly instead of hanging the MCP client forever.
	b.WriteString("  -o ServerAliveInterval=\"$BRW_SERVER_ALIVE_INTERVAL\" \\\n")
	b.WriteString("  -o ServerAliveCountMax=\"$BRW_SERVER_ALIVE_COUNT_MAX\" \\\n")
	b.WriteString("  -o UserKnownHostsFile=\"$BRW_KNOWN_HOSTS\" \\\n")
	b.WriteString("  -o StrictHostKeyChecking=\"$BRW_STRICT_HOST_KEY_CHECKING\" \\\n")
	if opts.IdentityFile != "" {
		fmt.Fprintf(&b, "  -o IdentityFile=%s \\\n", quoteScript(opts.IdentityFile))
		b.WriteString("  -o IdentitiesOnly=yes \\\n")
	}
	if opts.Compression {
		b.WriteString("  -o Compression=yes \\\n")
	}
	for _, opt := range opts.SSHOptions {
		if opt == "" {
			continue
		}
		fmt.Fprintf(&b, "  -o %s \\\n", quoteScript(opt))
	}
	b.WriteString("  \"$BRW_REMOTE\" \\\n")
	fmt.Fprintf(&b, "  %s \\\n", quoteScript(remoteCommand))
	b.WriteString("  2>>\"$BRW_LOG\"\n")
	return b.String()
}

func defaultAppDir() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "brw")
	}
	return filepath.Join(home, ".local", "share", "brw")
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		if isShellAssignment(arg) {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, quoteRemote(arg))
	}
	return strings.Join(quoted, " ")
}

func isShellAssignment(s string) bool {
	name, _, ok := strings.Cut(s, "=")
	return ok && strings.HasPrefix(name, "BRW_")
}

func shellEnv(env map[string]string) []string {
	keys := []string{"BRW_WORKSPACE", "BRW_PROFILE", "BRW_PROFILE_POLICY"}
	assignments := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := env[key]
		if !ok || value == "" {
			continue
		}
		assignments = append(assignments, key+"="+quoteRemote(value))
	}
	return assignments
}

func quoteRemote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.HasPrefix(s, "~/") {
		homePath := "$HOME/" + strings.TrimPrefix(s, "~/")
		return `"` + strings.ReplaceAll(homePath, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteScript(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func chromePolicyMobileconfig(extensionID, installMode, updateURL string) string {
	identifier := "org.donworks.brw.chrome-extension"
	uuid1 := randomHex()
	uuid2 := randomHex()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
  <key>PayloadIdentifier</key>
  <string>%s</string>
  <key>PayloadUUID</key>
  <string>%s</string>
  <key>PayloadDisplayName</key>
  <string>brw</string>
  <key>PayloadDescription</key>
  <string>Installs brw for approved Chrome profiles.</string>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key>
      <string>com.google.Chrome</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
      <key>PayloadIdentifier</key>
      <string>%s.settings</string>
      <key>PayloadUUID</key>
      <string>%s</string>
      <key>PayloadDisplayName</key>
      <string>Google Chrome ExtensionSettings</string>
      <key>ExtensionSettings</key>
      <dict>
        <key>%s</key>
        <dict>
          <key>installation_mode</key>
          <string>%s</string>
          <key>update_url</key>
          <string>%s</string>
        </dict>
      </dict>
    </dict>
  </array>
</dict>
</plist>
`, identifier, uuid1, identifier, uuid2, xmlEscape(extensionID), xmlEscape(installMode), xmlEscape(updateURL))
}

func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	return strings.ToUpper(hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" + hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:]))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func extensionVersion(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "0.1.0"
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version == "" {
		return "0.1.0"
	}
	return manifest.Version
}

func writeJSON(w io.Writer, value any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func writeOutput(path string, data []byte, mode os.FileMode) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
