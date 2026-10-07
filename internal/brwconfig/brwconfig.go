// Package brwconfig reads brw.json, the per-machine defaults for brwd.
package brwconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// FileName is what brw looks for in the user config directory.
const FileName = "brw.json"

// File is a brw.json document.
type File struct {
	// Defaults apply to every daemon on this machine.
	Defaults map[string]any `json:"defaults,omitempty"`
	// Profiles are per-daemon overrides, keyed by the --profile name the daemon is started with.
	Profiles map[string]map[string]any `json:"profiles,omitempty"`
}

var notConfigurable = map[string]string{
	"mcp":                              "names what this invocation is, not how the daemon is configured; a file that set it would turn every brwd on the machine into a stdio server",
	"print-system-prompt":              "prints and exits, so it is not a setting",
	"login":                            "is a one-off headed sign-in run, not a standing configuration",
	"unsafe-real-profile":              "disables the guard against launching Chrome on your real browser profile; it has to be typed, not left in a file",
	"unsafe-allow-default-profile-cdp": "bypasses the profile policy for diagnostics; it has to be typed, not left in a file",
	"config":                           "would let a config file name another config file",
}

// Load reads a brw.json.
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, resolved, fmt.Errorf("parse %s: %w", resolved, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("it contains more than one JSON document")
		}
		return nil, resolved, fmt.Errorf("parse %s: %w", resolved, err)
	}
	return &file, resolved, nil
}

const maxConfigBytes = 1 << 20

func readTrusted(path string) ([]byte, error) {

	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
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

// Applied is one setting the file supplied, for the daemon's startup log.
type Applied struct {
	Flag    string
	Value   string
	Section string
}

func (a Applied) String() string { return fmt.Sprintf("%s=%s (%s)", a.Flag, a.Value, a.Section) }

// Apply sets flags from the file for every setting the command line and the environment did not already decide.
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

	type section struct {
		name   string
		values map[string]any
	}
	var sections []section
	if name := strings.TrimSpace(profile); name != "" {
		if values, ok := f.Profiles[name]; ok {
			sections = append(sections, section{name: "profiles." + name, values: values})
		}
	}
	sections = append(sections, section{name: "defaults", values: f.Defaults})

	applied := []Applied{}
	done := map[string]bool{}
	for _, section := range sections {
		for _, name := range slices.Sorted(maps.Keys(section.values)) {
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

			if setOnCommandLine[name] {
				continue
			}

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

func flagStrings(value any) ([]string, error) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, nil
	case bool:
		return []string{strconv.FormatBool(typed)}, nil

	case int:
		return []string{strconv.Itoa(typed)}, nil
	case int64:
		return []string{strconv.FormatInt(typed, 10)}, nil
	case float64:
		if typed != math.Trunc(typed) {
			return nil, fmt.Errorf("%v is not a whole number, and no brwd flag takes a fraction", typed)
		}
		if typed < -0x1p63 || typed >= 0x1p63 {
			return nil, fmt.Errorf("%v is outside the range of a signed 64-bit integer", typed)
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
