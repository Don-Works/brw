package usageview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

const maxFiles = 128
const maxBytes = 256 << 20
const maxRecords = 250000
const maxGroups = 1024

type Options struct {
	Path      string
	Since     time.Time
	Layer     string
	Profile   string
	Operation string
	Limit     int
}

type Counter struct {
	Total   int64 `json:"total"`
	Samples int   `json:"samples"`
}

type Group struct {
	Layer             string  `json:"layer"`
	Operation         string  `json:"operation"`
	Mode              string  `json:"mode"`
	Profile           string  `json:"profile"`
	Scope             string  `json:"scope"`
	Representation    string  `json:"representation"`
	Records           int     `json:"records"`
	Errors            int     `json:"errors"`
	DurationSamples   int     `json:"duration_samples"`
	TotalMS           float64 `json:"total_ms"`
	P50MS             float64 `json:"p50_ms"`
	P95MS             float64 `json:"p95_ms"`
	MaxMS             float64 `json:"max_ms"`
	InputBytes        Counter `json:"input_bytes"`
	OutputBytes       Counter `json:"output_bytes"`
	InputEstimate     Counter `json:"estimated_input_tokens_chars4"`
	OutputEstimate    Counter `json:"estimated_output_tokens_chars4"`
	ProviderInput     Counter `json:"provider_input_tokens"`
	ProviderOutput    Counter `json:"provider_output_tokens"`
	ProviderCached    Counter `json:"provider_cached_input_tokens"`
	ProviderReasoning Counter `json:"provider_reasoning_tokens"`
	durations         []float64
}

type Report struct {
	Schema          string    `json:"schema"`
	GeneratedAt     time.Time `json:"generated_at"`
	Since           time.Time `json:"since"`
	Files           int       `json:"files"`
	ScannedRecords  int       `json:"scanned_records"`
	MatchedRecords  int       `json:"matched_records"`
	InvalidRecords  int       `json:"invalid_records"`
	IncompleteLines int       `json:"incomplete_lines"`
	ReadErrors      int       `json:"read_errors"`
	Bounded         bool      `json:"bounded"`
	TotalGroups     int       `json:"total_groups"`
	Groups          []Group   `json:"groups"`
	Notes           []string  `json:"notes"`
}

func DefaultPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "brw", "usage"), nil
}

func Read(ctx context.Context, opts Options) (Report, error) {
	r := Report{Schema: "brw.usage/1", GeneratedAt: time.Now().UTC(), Since: opts.Since, Groups: []Group{}, Notes: []string{
		"Rows are separate measurement boundaries. Do not add HTTP, MCP, CLI or reader rows together as model context or tool-call totals.",
		"Token estimates use the recorded chars/4 estimator, exclude binary where measured, and are not provider billing or the host model's complete context.",
		"A counter with zero samples is unknown. Provider token counters appear only where reported; cached/reasoning counts may be subsets of input/output.",
	}}
	files, discoveryBounded, err := usageFiles(opts.Path)
	r.Bounded = discoveryBounded
	if err != nil {
		return r, err
	}
	if len(files) > maxFiles {
		files = files[:maxFiles]
		r.Bounded = true
	}
	groups := map[string]*Group{}
	seen := []os.FileInfo{}
	remaining := int64(maxBytes)
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if remaining <= 0 || r.ScannedRecords >= maxRecords {
			r.Bounded = true
			break
		}
		f, err := os.Open(path)
		if err != nil {
			r.ReadErrors++
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			r.ReadErrors++
			continue
		}
		duplicate := false
		for _, prev := range seen {
			if os.SameFile(prev, info) {
				duplicate = true
				break
			}
		}
		if duplicate {
			f.Close()
			continue
		}
		seen = append(seen, info)
		r.Files++
		n := info.Size()
		if n > remaining {
			n = remaining
			r.Bounded = true
		}
		remaining -= n
		reader := bufio.NewReaderSize(io.LimitReader(f, n), 64<<10)
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return r, err
			}
			line, readErr := boundedLine(reader)
			if errors.Is(readErr, errLongLine) {
				r.InvalidRecords++
				r.Bounded = true
				break
			}
			if len(line) > 0 && line[len(line)-1] != '\n' {
				r.IncompleteLines++
				break
			}
			if len(line) > 0 {
				r.ScannedRecords++
				var event map[string]json.RawMessage
				if json.Unmarshal(line, &event) != nil {
					r.InvalidRecords++
				} else if include(event, opts, &r) {
					layer, operation := label(event, "layer"), label(event, "operation")
					mode, profile := label(event, "mode"), label(event, "profile")
					scope, representation := label(event, "scope"), label(event, "representation")
					key := strings.Join([]string{layer, operation, mode, profile, scope, representation}, "\x00")
					g := groups[key]
					if g == nil && len(groups) < maxGroups {
						g = &Group{Layer: layer, Operation: operation, Mode: mode, Profile: profile, Scope: scope, Representation: representation}
						groups[key] = g
					}
					if g == nil {
						r.Bounded = true
					} else {
						accumulate(g, event)
						r.MatchedRecords++
					}
				}
			}
			if r.ScannedRecords >= maxRecords {
				r.Bounded = true
				break
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					r.ReadErrors++
				}
				break
			}
		}
		f.Close()
	}
	for _, g := range groups {
		sort.Float64s(g.durations)
		g.DurationSamples = len(g.durations)
		if len(g.durations) > 0 {
			g.P50MS = percentile(g.durations, .5)
			g.P95MS = percentile(g.durations, .95)
			g.MaxMS = g.durations[len(g.durations)-1]
		}
		r.Groups = append(r.Groups, *g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		a, b := r.Groups[i], r.Groups[j]
		if a.OutputBytes.Total != b.OutputBytes.Total {
			return a.OutputBytes.Total > b.OutputBytes.Total
		}
		if a.TotalMS != b.TotalMS {
			return a.TotalMS > b.TotalMS
		}
		return strings.Join([]string{a.Layer, a.Operation, a.Mode, a.Profile, a.Scope, a.Representation}, "\x00") < strings.Join([]string{b.Layer, b.Operation, b.Mode, b.Profile, b.Scope, b.Representation}, "\x00")
	})
	r.TotalGroups = len(r.Groups)
	if opts.Limit > 0 && len(r.Groups) > opts.Limit {
		r.Groups = r.Groups[:opts.Limit]
	}
	return r, nil
}

func include(event map[string]json.RawMessage, opts Options, report *Report) bool {
	var ts string
	if json.Unmarshal(event["ts"], &ts) != nil {
		report.InvalidRecords++
		return false
	}
	stamp, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		report.InvalidRecords++
		return false
	}
	if stamp.Before(opts.Since) {
		return false
	}
	for key, want := range map[string]string{"layer": opts.Layer, "profile": opts.Profile, "operation": opts.Operation} {
		if want != "" && label(event, key) != want {
			return false
		}
	}
	return label(event, "operation") != "unknown" && label(event, "layer") != "unknown"
}

func label(event map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(event[key], &value) == nil && usagelog.SafeID(value) != "" {
		return usagelog.SafeID(value)
	}
	return "unknown"
}

func number(event map[string]json.RawMessage, key string) (int64, bool) {
	var value int64
	raw := event[key]
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, false
	}
	return value, true
}

func accumulate(g *Group, e map[string]json.RawMessage) {
	g.Records++
	if outcome := label(e, "outcome"); outcome != "ok" && outcome != "success" {
		g.Errors++
	}
	if us, ok := number(e, "duration_us"); ok {
		g.durations = append(g.durations, float64(us)/1000)
		g.TotalMS += float64(us) / 1000
	} else if ms, ok := number(e, "duration_ms"); ok {
		g.durations = append(g.durations, float64(ms))
		g.TotalMS += float64(ms)
	}
	for key, c := range map[string]*Counter{"input_bytes": &g.InputBytes, "output_bytes": &g.OutputBytes, "estimated_input_tokens_chars4": &g.InputEstimate, "estimated_output_tokens_chars4": &g.OutputEstimate, "provider_input_tokens": &g.ProviderInput, "provider_output_tokens": &g.ProviderOutput, "provider_cached_input_tokens": &g.ProviderCached, "provider_reasoning_tokens": &g.ProviderReasoning} {
		if value, ok := number(e, key); ok {
			if value > math.MaxInt64-c.Total {
				c.Total = math.MaxInt64
			} else {
				c.Total += value
			}
			c.Samples++
		}
	}
}

func percentile(values []float64, p float64) float64 {
	return values[int(math.Ceil(float64(len(values))*p))-1]
}

var errLongLine = errors.New("usage record exceeds one MiB")

func boundedLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > 1<<20 {
			return nil, errLongLine
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func usageFiles(path string) ([]string, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("usage path must not be a symbolic link")
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, false, errors.New("usage path must be a regular file or directory")
		}
		return []string{path}, false, nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer directory.Close()
	entries, readErr := directory.ReadDir(4097)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, false, readErr
	}
	bounded := len(entries) > 4096
	if bounded {
		entries = entries[:4096]
	}
	type candidate struct {
		path  string
		stamp time.Time
	}
	candidates := []candidate{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !ledgerName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, candidate{filepath.Join(path, entry.Name()), info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].stamp.Equal(candidates[j].stamp) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].stamp.After(candidates[j].stamp)
	})
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.path
	}
	return files, bounded, nil
}

func ledgerName(name string) bool {
	for _, suffix := range []string{".ndjson", ".jsonl"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
		at := strings.LastIndex(name, suffix+".")
		if at >= 0 {
			n, err := strconv.Atoi(name[at+len(suffix)+1:])
			if err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}
