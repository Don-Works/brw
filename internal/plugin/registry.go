package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Don-Works/brw/internal/credential"
)

// Registry is the daemon's credential.Resolver. The assertion is here so a
// change to either side is a compile error rather than a runtime nil.
var _ credential.Resolver = (*Registry)(nil)

// Same for the browser backend: the registry IS the BrowserProvider a daemon
// holds, so a change to either side is a compile error.
var _ BrowserProvider = (*Registry)(nil)

// Status is what an operator (never a page, never a page tool) can see about a
// loaded plugin. It names the backend kind but not the argv or the directory:
// the daemon's own filesystem layout is not part of the control-plane answer.
type Status struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Description    string   `json:"description"`
	Capabilities   []string `json:"capabilities"`
	CredentialKind string   `json:"credential_kind,omitempty"`
	BrowserKind    string   `json:"browser_kind,omitempty"`
	Revoked        bool     `json:"revoked"`
}

type loadedPlugin struct {
	manifest    Manifest
	kind        string
	browserKind string
	credential  credentialProvider
	browser     browserProvider
	// browserCredential is the REFERENCE the browser provider needs, never a
	// value. It is resolved through the credential.read holder at the moment of
	// each call and wiped when that call returns, so no provider credential is
	// retained for the life of a session.
	browserCredential string
	revoked           bool
}

// Registry holds the plugins one daemon loaded. It implements
// credential.Resolver and BrowserProvider, and those two are the ONLY runtime
// surfaces a grant produces: a plugin is asked for a secret or for a browser
// and has no other way in. Both are consulted per call, so a revoke takes
// effect on the next step rather than on the next restart.
type Registry struct {
	mu      sync.RWMutex
	plugins []*loadedPlugin
}

// Empty returns a registry with nothing loaded. A daemon started without
// --plugin-dir uses it, so every credential reference fails closed with
// ErrNoProvider instead of the caller having to nil-check a resolver.
func Empty() *Registry { return &Registry{} }

// MaxPlugins bounds one directory. The point of a plugin directory is a handful
// of operator-reviewed entries, not a corpus.
const MaxPlugins = 32

// Load reads every *.json manifest directly inside root.
//
// It refuses a directory or manifest another local user could write: brw does
// not sandbox a plugin, so who can write the manifest is the whole trust
// boundary. That covers the mode, the owner, and every ancestor of the
// directory, because a 0700 directory inside a world-writable parent is one
// rename away from being somebody else's directory.
func Load(root string) (*Registry, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Empty(), nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("plugin directory does not exist")
		}
		return nil, fmt.Errorf("read plugin directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("plugin directory is not a directory")
	}
	if err := refuseSharedWrite(info.Mode().Perm(), "plugin directory"); err != nil {
		return nil, err
	}
	if err := refuseForeignOwner(info, "plugin directory"); err != nil {
		return nil, err
	}
	if err := refuseWritableAncestors(absolute, "plugin directory"); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, fmt.Errorf("read plugin directory: %w", err)
	}
	registry := Empty()
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) > MaxPlugins {
		return nil, fmt.Errorf("plugin directory holds more than %d manifests", MaxPlugins)
	}
	seen := map[string]bool{}
	credentialHolder := ""
	browserHolder := ""
	for _, name := range names {
		path := filepath.Join(absolute, name)
		manifest, err := loadManifestFile(path)
		if err != nil {
			return nil, fmt.Errorf("plugin manifest %s: %w", name, err)
		}
		if seen[manifest.ID] {
			return nil, fmt.Errorf("plugin manifest %s: plugin id %q is already loaded", name, manifest.ID)
		}
		seen[manifest.ID] = true
		loaded := &loadedPlugin{manifest: manifest}
		if slices.Contains(manifest.Capabilities, CapabilityCredentialRead) {
			if credentialHolder != "" {
				// Two vaults make "which one answered?" unanswerable from a
				// failure, and a silently shadowed provider ends with the wrong
				// password typed into the right box.
				return nil, fmt.Errorf("plugin manifest %s: %q already holds %s; only one plugin may", name, credentialHolder, CapabilityCredentialRead)
			}
			credentialHolder = manifest.ID
			provider, err := newCredentialProvider(*manifest.Credential)
			if err != nil {
				return nil, fmt.Errorf("plugin manifest %s: %w", name, err)
			}
			loaded.credential = provider
			loaded.kind = manifest.Credential.Kind
		}
		if slices.Contains(manifest.Capabilities, CapabilityBrowserProvider) {
			if browserHolder != "" {
				// Same reason as the credential half: two backends make "which
				// browser am I driving?" unanswerable from a failure, and a
				// silently shadowed provider ends with a recipe running against
				// the wrong browser entirely.
				return nil, fmt.Errorf("plugin manifest %s: %q already holds %s; only one plugin may", name, browserHolder, CapabilityBrowserProvider)
			}
			browserHolder = manifest.ID
			provider, err := newBrowserProvider(*manifest.Browser)
			if err != nil {
				return nil, fmt.Errorf("plugin manifest %s: %w", name, err)
			}
			loaded.browser = provider
			loaded.browserKind = manifest.Browser.Kind
			loaded.browserCredential = manifest.Browser.Credential
		}
		registry.plugins = append(registry.plugins, loaded)
	}
	return registry, nil
}

func loadManifestFile(path string) (Manifest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Manifest{}, err
	}
	if err := refuseSharedWrite(info.Mode().Perm(), "manifest"); err != nil {
		return Manifest{}, err
	}
	if err := refuseForeignOwner(info, "manifest"); err != nil {
		return Manifest{}, err
	}
	if info.Size() > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

func refuseSharedWrite(mode fs.FileMode, what string) error {
	if mode&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or other; brw does not sandbox a plugin, so anyone who can write it chooses what the daemon runs", what)
	}
	return nil
}

// refuseForeignOwner refuses a path some other local user owns. The mode alone
// is not the boundary: a 0755 directory owned by another user still lets that
// user drop a manifest in, and brwd would run its argv as brwd's own user. root
// is allowed because a system install such as /etc/brw/plugins is a legitimate
// deployment, and root can replace the daemon binary regardless.
func refuseForeignOwner(info fs.FileInfo, what string) error {
	owner, ok := fileOwner(info)
	if !ok {
		return nil
	}
	if owner == os.Getuid() || owner == 0 {
		return nil
	}
	return fmt.Errorf("%s is owned by uid %d rather than by the daemon's user or root; brw does not sandbox a plugin, so its owner chooses what the daemon runs", what, owner)
}

// refuseWritableAncestors walks from path's parent to the filesystem root.
//
// Checking only the leaf is not the trust boundary docs/plugins.md claims: a
// 0700 plugin directory inside a world-writable parent can be renamed away and
// replaced wholesale by anyone who can write that parent, and the replacement
// passes every check on the leaf. The sticky bit is the exception, because it
// is the flag that stops a non-owner renaming or unlinking an entry, which is
// what makes a shared temporary directory usable as a parent at all.
func refuseWritableAncestors(path, what string) error {
	current := filepath.Dir(filepath.Clean(path))
	for {
		info, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("read %s ancestor %s: %w", what, current, err)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&fs.ModeSticky == 0 {
			return fmt.Errorf("%s ancestor %s is writable by group or other; anyone who can write it can replace the directory beneath it", what, current)
		}
		if err := refuseForeignOwner(info, fmt.Sprintf("%s ancestor %s", what, current)); err != nil {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func (r *Registry) Plugins() []Status {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.plugins))
	for _, loaded := range r.plugins {
		out = append(out, Status{
			ID:             loaded.manifest.ID,
			Name:           loaded.manifest.Name,
			Version:        loaded.manifest.Version,
			Description:    loaded.manifest.Description,
			Capabilities:   append([]string(nil), loaded.manifest.Capabilities...),
			CredentialKind: loaded.kind,
			BrowserKind:    loaded.browserKind,
			Revoked:        loaded.revoked,
		})
	}
	return out
}

// ErrPluginNotLoaded names a revoke against an id this daemon never loaded, so
// a typo does not read as a successful revocation.
var ErrPluginNotLoaded = errors.New("no plugin with that id is loaded")

// Revoke drops a plugin's grants for the life of this process. It narrows what
// brw can do, which is why it is reachable from the control plane while
// granting is not: granting stays an operator action against the filesystem.
func (r *Registry) Revoke(id string) error {
	if r == nil {
		return ErrPluginNotLoaded
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, loaded := range r.plugins {
		if loaded.manifest.ID == id {
			loaded.revoked = true
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrPluginNotLoaded, id)
}

// ProbeProvider implements credential.Prober: it reports the same refusal
// Resolve would, without asking a provider for anything. The recipe runner
// calls it before step one, so a daemon with no provider — or one whose
// provider was revoked a second ago — refuses the whole run rather than
// stopping at the password field with the username already typed.
func (r *Registry) ProbeProvider() error {
	if r == nil {
		return credential.ErrNoProvider
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, loaded := range r.plugins {
		if loaded.credential == nil {
			continue
		}
		if loaded.revoked {
			return fmt.Errorf("%w: plugin %q no longer holds %s", credential.ErrProviderRevoked, loaded.manifest.ID, CapabilityCredentialRead)
		}
		return nil
	}
	return credential.ErrNoProvider
}

// OpenBrowserSession implements BrowserProvider: it asks the granted plugin for
// a browser and returns the session with the release function that gives it
// back.
//
// Every failure mode is closed. No provider, a revoked provider, a provider
// error, an unparseable envelope and an unresolvable credential all return an
// error. None of them degrades to "carry on with a local browser", because the
// operator who configured a provider asked for the browser to be somewhere
// else, and silently launching Chrome here instead would run their flow on a
// machine and an IP they did not choose.
//
// The credential is resolved per call and wiped when the call returns, so a
// provider key is never retained for the life of a session. The consequence is
// stated rather than hidden: a credential.read grant revoked between open and
// release makes the release fail, and the caller is told which session was left
// with the provider.
func (r *Registry) OpenBrowserSession(ctx context.Context) (BrowserSession, func(context.Context) error, error) {
	holder, err := r.browserHolder()
	if err != nil {
		return BrowserSession{}, nil, err
	}
	secret, err := r.browserCredential(ctx, holder)
	if err != nil {
		return BrowserSession{}, nil, err
	}
	defer secret.Wipe()
	session, err := holder.browser.open(ctx, secret)
	if err != nil {
		return BrowserSession{}, nil, fmt.Errorf("plugin %q: %w", holder.manifest.ID, err)
	}
	session.ProviderID = holder.manifest.ID
	release := func(releaseCtx context.Context) error {
		// Deliberately NOT re-checking revocation: a session already open has to
		// be returnable, or revoking a plugin leaks the browser it lent. Revoke
		// stops the NEXT open, which is what narrowing a grant means here.
		releaseSecret, err := r.browserCredential(releaseCtx, holder)
		if err != nil {
			return fmt.Errorf("plugin %q still holds session %q: %w", holder.manifest.ID, session.SessionID, err)
		}
		defer releaseSecret.Wipe()
		if err := holder.browser.release(releaseCtx, session.SessionID, releaseSecret); err != nil {
			return fmt.Errorf("plugin %q: %w", holder.manifest.ID, err)
		}
		return nil
	}
	return session, release, nil
}

// ProbeBrowserProvider reports the same refusal OpenBrowserSession would,
// without asking a provider for anything. It lets a daemon decide at startup
// whether it has a remote backend at all.
func (r *Registry) ProbeBrowserProvider() error {
	_, err := r.browserHolder()
	return err
}

func (r *Registry) browserHolder() (*loadedPlugin, error) {
	if r == nil {
		return nil, ErrNoBrowserProvider
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, loaded := range r.plugins {
		if loaded.browser == nil {
			continue
		}
		if loaded.revoked {
			return nil, fmt.Errorf("%w: plugin %q no longer holds %s", ErrBrowserProviderRevoked, loaded.manifest.ID, CapabilityBrowserProvider)
		}
		return loaded, nil
	}
	return nil, ErrNoBrowserProvider
}

// browserCredential resolves the provider's declared reference through the
// credential.read holder. A provider that declares no credential gets none:
// a stand-in endpoint on the operator's own machine needs no key, and inventing
// one would make the common case need two plugins.
func (r *Registry) browserCredential(ctx context.Context, holder *loadedPlugin) (credential.Secret, error) {
	if holder.browserCredential == "" {
		return credential.Secret{}, nil
	}
	secret, err := r.Resolve(ctx, holder.browserCredential)
	if err != nil {
		return credential.Secret{}, fmt.Errorf("resolve the browser provider's credential reference: %w", err)
	}
	return secret, nil
}

// Resolve implements credential.Resolver.
//
// Every failure mode here is closed: no provider, a revoked provider, an
// invalid reference and a provider error all return an error. None of them
// degrades to an empty value, a prompt, or the literal reference text, because
// each of those would type something into a password field.
func (r *Registry) Resolve(ctx context.Context, reference string) (credential.Secret, error) {
	if err := credential.ValidateReference(reference); err != nil {
		return credential.Secret{}, err
	}
	if r == nil {
		return credential.Secret{}, credential.ErrNoProvider
	}
	r.mu.RLock()
	var holder *loadedPlugin
	revoked := false
	for _, loaded := range r.plugins {
		if loaded.credential != nil {
			holder, revoked = loaded, loaded.revoked
			break
		}
	}
	r.mu.RUnlock()
	if holder == nil {
		return credential.Secret{}, credential.ErrNoProvider
	}
	if revoked {
		return credential.Secret{}, fmt.Errorf("%w: plugin %q no longer holds %s", credential.ErrProviderRevoked, holder.manifest.ID, CapabilityCredentialRead)
	}
	return holder.credential.resolve(ctx, reference)
}
