// Package brwconfig reads brw.json, the per-machine defaults for brwd.
//
// Everything brwd takes is a flag, and a machine that wants a non-default
// daemon therefore has to carry those flags in whatever started it — a launchd
// plist, a systemd unit, a shell alias, a README line somebody pastes. Change
// one and you change it in every place it was written down, and the places
// disagree quietly. brw.json is the one place: defaults for this machine, and
// per-profile overrides for the daemons on it.
//
// It is the WEAKEST source, by design. A flag on the command line wins, then the
// environment, then the profile section, then the file's defaults, then brwd's
// built-in default. Anything else would make a config file able to override what
// an operator just typed.
package brwconfig

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// FileName is what brw looks for in the user config directory.
const FileName = "brw.json"

// File is a brw.json document. Decoding is strict, so a misspelled top-level
// key is a startup error rather than a setting that silently does nothing.
type File struct {
	// Defaults apply to every daemon on this machine. Keys are brwd flag names
	// without the leading dashes, values are what the flag would take.
	Defaults map[string]any `json:"defaults,omitempty"`
	// Profiles are per-daemon overrides, keyed by the --profile name the daemon
	// is started with. A daemon started without --profile reads only Defaults.
	//
	// The section is chosen by the profile the operator named on the command
	// line or in the environment, never by a "profile" key in Defaults: a file
	// that could select its own section would decide which daemon this process
	// is, which is the caller's decision.
	Profiles map[string]map[string]any `json:"profiles,omitempty"`
}

// notConfigurable names the flags a config file may NOT set, with the reason.
//
// Two kinds: flags that say what this invocation IS rather than how the daemon
// is configured (putting "mcp": true in a file would turn every brwd on the
// machine into a stdio server), and the diagnostic overrides that disable a
// safety check. An operator can still pass those; what they cannot do is leave
// one switched on in a file and forget it is there.
var notConfigurable = map[string]string{
	"mcp":                              "names what this invocation is, not how the daemon is configured; a file that set it would turn every brwd on the machine into a stdio server",
	"print-system-prompt":              "prints and exits, so it is not a setting",
	"login":                            "is a one-off headed sign-in run, not a standing configuration",
	"unsafe-real-profile":              "disables the guard against launching Chrome on your real browser profile; it has to be typed, not left in a file",
	"unsafe-allow-default-profile-cdp": "bypasses the profile policy for diagnostics; it has to be typed, not left in a file",
	"config":                           "would let a config file name another config file",
}

// Load reads a brw.json. An empty path uses DefaultPath. A missing file is not
// an error: it returns nil, and the daemon runs on flags, environment and
// built-in defaults exactly as it did before this existed. A path the operator
// named explicitly and that does not exist IS an error — they meant it.
func Load(path string) (*File, string, error) {
	explicit := strings.TrimSpace(path) != ""
	resolved := strings.TrimSpace(path)
	if !explicit {
		var err error
		resolved, err = DefaultPath()
		if err != nil {
			return nil, "", err
		}
	}
	data, err := readTrusted(resolved)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil, "", nil
		}
		return nil, resolved, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, resolved, fmt.Errorf("parse %s: %w", resolved, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, resolved, fmt.Errorf("parse %s: it contains more than one JSON document", resolved)
	}
	return &file, resolved, nil
}

// maxConfigBytes bounds the file. A brw.json is a few hundred bytes of flag
// names; anything approaching this is not one.
const maxConfigBytes = 1 << 20

// readTrusted reads brw.json only when nothing but its owner could have written
// it.
//
// The file decides what every brwd on this machine does — chrome-arg,
// proxy-server, ignore-https-errors, allowed-domains, plugin-dir,
// recipe-provider-url, profile-policy — so another local account that can write
// it can redirect every browser brw drives. The repo already gates the recipe
// provider token, the consent key, credential files and recipe directories the
// same way. Refused rather than warned about: a warning on a daemon's stderr at
// boot is read by nobody.
//
// Writability is the check, not readability: brw.json holds no secret, and
// requiring 0600 would reject the ordinary 0644 a person's editor writes.
func readTrusted(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is mode %#o; it must not be group- or world-writable, because it decides what every brwd on this machine does", path, info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Re-checked on the open handle: the path could have been swapped between
	// the Lstat and the Open.
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s changed before it was read", path)
	}
	if runtime.GOOS != "windows" && opened.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is mode %#o; it must not be group- or world-writable, because it decides what every brwd on this machine does", path, opened.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxConfigBytes)
	}
	return data, nil
}

// DefaultPath is where brw looks when no path is given.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve the user config directory: %w", err)
	}
	return filepath.Join(dir, "brw", FileName), nil
}

// Applied is one setting the file supplied, for the daemon's startup log. An
// operator debugging "why is this daemon headless" needs to see that a file did
// it, and which section.
type Applied struct {
	Flag    string
	Value   string
	Section string
}

func (a Applied) String() string { return fmt.Sprintf("%s=%s (%s)", a.Flag, a.Value, a.Section) }

// Apply sets flags from the file for every setting the command line and the
// environment did not already decide.
//
// setOnCommandLine is the set of flag names the operator actually typed;
// lookupEnv is the environment. Both are parameters rather than globals so the
// precedence can be tested as a table instead of by mutating the process.
func (f *File) Apply(fs *flag.FlagSet, profile string, setOnCommandLine map[string]bool, lookupEnv func(string) (string, bool)) ([]Applied, error) {
	if f == nil {
		return nil, nil
	}
	if fs == nil {
		return nil, errors.New("brwconfig needs a flag set to apply to")
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	// The profile section is applied first and the defaults second, and a flag
	// already set by the section is not overwritten: a per-daemon override that
	// the machine default could undo would not be an override.
	sections := []struct {
		name   string
		values map[string]any
	}{}
	if name := strings.TrimSpace(profile); name != "" {
		if values, ok := f.Profiles[name]; ok {
			sections = append(sections, struct {
				name   string
				values map[string]any
			}{name: "profiles." + name, values: values})
		}
	}
	sections = append(sections, struct {
		name   string
		values map[string]any
	}{name: "defaults", values: f.Defaults})

	applied := []Applied{}
	done := map[string]bool{}
	for _, section := range sections {
		for _, name := range sortedKeys(section.values) {
			if done[name] {
				continue
			}
			if reason, blocked := notConfigurable[name]; blocked {
				return nil, fmt.Errorf("%s in %s: this flag %s", name, section.name, reason)
			}
			definition := fs.Lookup(name)
			if definition == nil {
				return nil, fmt.Errorf("%s in %s: brwd has no such flag", name, section.name)
			}
			done[name] = true
			// The command line wins outright.
			if setOnCommandLine[name] {
				continue
			}
			// Then the environment. brwd reads its environment as each flag's
			// DEFAULT, so by the time Apply runs an env-configured flag already
			// holds the env value and looks untouched — which is why this has to
			// ask the environment rather than compare against the zero value.
			if env := EnvFor(name); env != "" {
				if value, ok := lookupEnv(env); ok && strings.TrimSpace(value) != "" {
					continue
				}
			}
			values, err := flagStrings(section.values[name])
			if err != nil {
				return nil, fmt.Errorf("%s in %s: %w", name, section.name, err)
			}
			for _, value := range values {
				if err := definition.Value.Set(value); err != nil {
					return nil, fmt.Errorf("%s in %s: %w", name, section.name, err)
				}
			}
			applied = append(applied, Applied{Flag: name, Value: strings.Join(values, ","), Section: section.name})
		}
	}
	return applied, nil
}

// flagStrings converts a JSON value into the strings a flag's Set takes. A JSON
// array is several Set calls, which is what a repeatable flag (--extension,
// --chrome-arg) means.
func flagStrings(value any) ([]string, error) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, nil
	case bool:
		return []string{strconv.FormatBool(typed)}, nil
	// A JSON document only ever yields float64, but the same function is called
	// from Go with an int in a test fixture, and a converter that rejects one is
	// a converter nobody can call directly.
	case int:
		return []string{strconv.Itoa(typed)}, nil
	case int64:
		return []string{strconv.FormatInt(typed, 10)}, nil
	case float64:
		if typed != math.Trunc(typed) {
			return nil, fmt.Errorf("%v is not a whole number, and no brwd flag takes a fraction", typed)
		}
		return []string{strconv.FormatInt(int64(typed), 10)}, nil
	case []any:
		var out []string
		for _, item := range typed {
			converted, err := flagStrings(item)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		}
		if len(out) == 0 {
			return nil, errors.New("an empty list sets nothing; remove the key instead")
		}
		return out, nil
	case nil:
		return nil, errors.New("null is not a value; remove the key instead")
	default:
		return nil, fmt.Errorf("%T is not a value any brwd flag takes", value)
	}
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
