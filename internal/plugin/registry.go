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
	Revoked        bool     `json:"revoked"`
}

type loadedPlugin struct {
	manifest   Manifest
	kind       string
	credential credentialProvider
	revoked    bool
}

// Registry holds the plugins one daemon loaded. It implements
// credential.Resolver, and that is the ONLY runtime surface a grant produces:
// resolution is consulted per call, so a revoke takes effect on the next step
// rather than on the next restart.
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
// It refuses a group- or other-writable directory or manifest: brw does not
// sandbox a plugin, so who can write the manifest is the whole trust boundary,
// and a writable one means anyone on the machine chooses what brwd executes.
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
