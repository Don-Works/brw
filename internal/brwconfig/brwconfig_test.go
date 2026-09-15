package brwconfig

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testFlags registers a small stand-in for brwd's flag set: one flag of each
// kind the precedence rule has to hold for, named exactly as brwd names them so
// the env table applies unchanged.
type testValues struct {
	http     string
	headless bool
	inflight int
	idle     time.Duration
	args     repeatable
}

type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

func testFlags(values *testValues) *flag.FlagSet {
	fs := flag.NewFlagSet("brwd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&values.http, "http", "127.0.0.1:17310", "")
	fs.BoolVar(&values.headless, "headless", false, "")
	fs.IntVar(&values.inflight, "bridge-max-inflight", 6, "")
	fs.DurationVar(&values.idle, "idle-exit", 0, "")
	fs.Var(&values.args, "chrome-arg", "")
	return fs
}

// TestPrecedenceIsFlagThenEnvThenProfileThenDefaults is the whole contract, as
// a table. Every row is one flag with the same value available from several
// sources; the winner has to be the strongest source present.
func TestPrecedenceIsFlagThenEnvThenProfileThenDefaults(t *testing.T) {
	file := &File{
		Defaults: map[string]any{"http": "127.0.0.1:19000", "headless": true, "bridge-max-inflight": 3},
		Profiles: map[string]map[string]any{
			"work": {"http": "127.0.0.1:19100", "idle-exit": "30m"},
		},
	}

	cases := []struct {
		name          string
		profile       string
		commandLine   []string
		env           map[string]string
		wantHTTP      string
		wantHeadless  bool
		wantInflight  int
		wantIdle      time.Duration
		wantAppliedNo []string
	}{
		{
			name:         "nothing but the file",
			wantHTTP:     "127.0.0.1:19000",
			wantHeadless: true,
			wantInflight: 3,
		},
		{
			name:         "the profile section beats the machine defaults",
			profile:      "work",
			wantHTTP:     "127.0.0.1:19100",
			wantHeadless: true,
			wantInflight: 3,
			wantIdle:     30 * time.Minute,
		},
		{
			name:          "the environment beats the file",
			profile:       "work",
			env:           map[string]string{"BRW_HTTP_ADDR": "127.0.0.1:19200"},
			wantHTTP:      "127.0.0.1:19200",
			wantHeadless:  true,
			wantInflight:  3,
			wantIdle:      30 * time.Minute,
			wantAppliedNo: []string{"http"},
		},
		{
			name:          "the command line beats everything",
			profile:       "work",
			commandLine:   []string{"-http", "127.0.0.1:19300"},
			env:           map[string]string{"BRW_HTTP_ADDR": "127.0.0.1:19200"},
			wantHTTP:      "127.0.0.1:19300",
			wantHeadless:  true,
			wantInflight:  3,
			wantIdle:      30 * time.Minute,
			wantAppliedNo: []string{"http"},
		},
		{
			name:         "an unknown profile falls back to the machine defaults",
			profile:      "personal",
			wantHTTP:     "127.0.0.1:19000",
			wantHeadless: true,
			wantInflight: 3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var values testValues
			fs := testFlags(&values)
			if err := fs.Parse(tc.commandLine); err != nil {
				t.Fatalf("parse: %v", err)
			}
			set := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
			// brwd reads its environment as each flag's default, so the fixture
			// has to do the same or the env row would prove nothing.
			for name, env := range EnvTable() {
				if value, ok := tc.env[env]; ok && !set[name] {
					if definition := fs.Lookup(name); definition != nil {
						if err := definition.Value.Set(value); err != nil {
							t.Fatalf("seed %s from %s: %v", name, env, err)
						}
					}
				}
			}

			applied, err := file.Apply(fs, tc.profile, set, func(key string) (string, bool) {
				value, ok := tc.env[key]
				return value, ok
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if values.http != tc.wantHTTP {
				t.Errorf("http = %q, want %q", values.http, tc.wantHTTP)
			}
			if values.headless != tc.wantHeadless {
				t.Errorf("headless = %t, want %t", values.headless, tc.wantHeadless)
			}
			if values.inflight != tc.wantInflight {
				t.Errorf("bridge-max-inflight = %d, want %d", values.inflight, tc.wantInflight)
			}
			if values.idle != tc.wantIdle {
				t.Errorf("idle-exit = %s, want %s", values.idle, tc.wantIdle)
			}
			for _, name := range tc.wantAppliedNo {
				for _, setting := range applied {
					if setting.Flag == name {
						t.Errorf("the file was reported as supplying %s, which a stronger source decided", name)
					}
				}
			}
		})
	}
}

// TestEveryFlagWithAnEnvironmentVariableIsProtected is the enumeration that
// makes the precedence rule hold for all of them and not just the ones somebody
// wrote a case for. A flag missing from the env table has its environment value
// silently overwritten by the file, and nothing anywhere would say so.
func TestEveryFlagWithAnEnvironmentVariableIsProtected(t *testing.T) {
	table := EnvTable()
	if len(table) < 40 {
		t.Fatalf("the env table has %d entries; brwd reads far more than that", len(table))
	}
	denied := NotConfigurable()
	for name, env := range table {
		if _, blocked := denied[name]; blocked {
			// A flag no file may set cannot have its environment value
			// overwritten by one; the deny-list test covers it instead.
			continue
		}
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("brwd", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			var got string
			fs.StringVar(&got, name, "from-the-environment", "")

			file := &File{Defaults: map[string]any{name: "from-the-file"}}
			applied, err := file.Apply(fs, "", map[string]bool{}, func(key string) (string, bool) {
				if key == env {
					return "from-the-environment", true
				}
				return "", false
			})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got != "from-the-environment" {
				t.Fatalf("%s: the file overrode %s", name, env)
			}
			if len(applied) != 0 {
				t.Fatalf("%s: the file reported supplying a value the environment decided", name)
			}
		})
	}
}

func TestApplyRefusesWhatAFileMayNotSet(t *testing.T) {
	for name, reason := range NotConfigurable() {
		var values testValues
		fs := testFlags(&values)
		// The flag has to exist on the set, or the refusal would be the
		// unknown-flag one rather than the deny-list one.
		fs.Bool(name, false, "")
		file := &File{Defaults: map[string]any{name: true}}
		_, err := file.Apply(fs, "", map[string]bool{}, func(string) (string, bool) { return "", false })
		if err == nil {
			t.Errorf("a config file was allowed to set %s", name)
			continue
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), reason) {
			t.Errorf("the refusal for %s does not say why: %v", name, err)
		}
	}
}

func TestApplyRefusesAKeyThatIsNotAFlag(t *testing.T) {
	var values testValues
	fs := testFlags(&values)
	file := &File{Defaults: map[string]any{"headles": true}}
	_, err := file.Apply(fs, "", map[string]bool{}, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "headles") {
		t.Fatalf("a misspelled key was accepted or not named: %v", err)
	}
}

func TestApplyTakesAListForARepeatableFlag(t *testing.T) {
	var values testValues
	fs := testFlags(&values)
	file := &File{Defaults: map[string]any{"chrome-arg": []any{"--disable-features=X", "--lang=en-GB"}}}
	if _, err := file.Apply(fs, "", map[string]bool{}, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Join(values.args, " ") != "--disable-features=X --lang=en-GB" {
		t.Fatalf("chrome-arg = %v", values.args)
	}
}

func TestApplyRefusesAValueNoFlagTakes(t *testing.T) {
	for name, value := range map[string]any{
		"a fraction":  1.5,
		"null":        nil,
		"an object":   map[string]any{"nested": true},
		"empty list":  []any{},
		"nested list": []any{map[string]any{}},
	} {
		var values testValues
		fs := testFlags(&values)
		file := &File{Defaults: map[string]any{"http": value}}
		if _, err := file.Apply(fs, "", map[string]bool{}, func(string) (string, bool) { return "", false }); err == nil {
			t.Errorf("%s was accepted as a flag value", name)
		}
	}
}

func TestLoadIsQuietWhenThereIsNoFileAndLoudWhenOneWasNamed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	file, path, err := Load("")
	if err != nil || file != nil {
		t.Fatalf("a machine with no brw.json: file=%v path=%q err=%v", file, path, err)
	}

	missing := filepath.Join(dir, "does-not-exist.json")
	if _, _, err := Load(missing); err == nil {
		t.Fatal("a config file named explicitly and absent was accepted")
	}

	good := filepath.Join(dir, FileName)
	if err := os.WriteFile(good, []byte(`{"defaults":{"headless":true},"profiles":{"work":{"http":"127.0.0.1:19100"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, path, err := Load(good)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != good || loaded.Defaults["headless"] != true || loaded.Profiles["work"]["http"] != "127.0.0.1:19100" {
		t.Fatalf("loaded = %+v (%s)", loaded, path)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"defalts":{"headless":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "defalts") {
		t.Fatalf("a misspelled top-level key was accepted or not named: %v", err)
	}
}

// TestLoadFindsTheFileInTheUserConfigDirectory: the whole point is that a
// machine needs no flags anywhere, so the default location has to work.
func TestLoadFindsTheFileInTheUserConfigDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"defaults":{"idle-exit":"20m"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil || found != path {
		t.Fatalf("Load found %q, want %q", found, path)
	}
	var values testValues
	fs := testFlags(&values)
	if _, err := loaded.Apply(fs, "", map[string]bool{}, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if values.idle != 20*time.Minute {
		t.Fatalf("idle-exit = %s", values.idle)
	}
}
