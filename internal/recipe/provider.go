package recipe

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Match struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Origins     []string `json:"origins"`
	Risk        string   `json:"risk"`
	Digest      string   `json:"digest"`
	Score       float64  `json:"score"`
}

// Provider is the private boundary. Search returns disclosure-safe metadata;
// Fetch returns one exact immutable version after the caller pins its digest.
type Provider interface {
	Search(context.Context, string, string, int) ([]Match, error)
	Fetch(context.Context, string, string, string) (Recipe, error)
}

// Embedder lets a private deployment attach its own embedding model. brw never
// sends recipe content to a model or vendor by itself.
type Embedder interface {
	Embed(context.Context, string) ([]float64, error)
}

type Catalog struct {
	embedder       Embedder
	entries        []catalogEntry
	idf            map[string]float64
	postings       map[string][]int
	originPostings map[string][]int
	byIdentity     map[string]int
	latestByID     map[string]int
}

type catalogEntry struct {
	recipe Recipe
	digest string
	doc    string
	terms  map[string]float64
	vector []float64
}

type DirectoryConfig struct {
	Root           string
	RepositoryRoot string
	Embedder       Embedder
}

// DirectoryProvider keeps a modest, owner-only local recipe collection live
// without requiring a browser-daemon restart after every new immutable version.
// It fingerprints file metadata on each discovery/fetch and only rebuilds the
// in-memory search index when the directory changes. Large collections should
// use HTTPProvider so indexing and authorization remain provider-owned.
type DirectoryProvider struct {
	config      DirectoryConfig
	mu          sync.Mutex
	catalog     *Catalog
	fingerprint string
}

func NewDirectoryProvider(ctx context.Context, config DirectoryConfig) (*DirectoryProvider, error) {
	provider := &DirectoryProvider{config: config, fingerprint: "uninitialized"}
	if _, err := provider.current(ctx); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *DirectoryProvider) Search(ctx context.Context, query, origin string, limit int) ([]Match, error) {
	catalog, err := p.current(ctx)
	if err != nil {
		return nil, err
	}
	return catalog.Search(ctx, query, origin, limit)
}

func (p *DirectoryProvider) Fetch(ctx context.Context, id, version, digest string) (Recipe, error) {
	catalog, err := p.current(ctx)
	if err != nil {
		return Recipe{}, err
	}
	return catalog.Fetch(ctx, id, version, digest)
}

func (p *DirectoryProvider) current(ctx context.Context) (*Catalog, error) {
	fingerprint, err := directoryFingerprint(p.config)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if fingerprint == p.fingerprint {
		return p.catalog, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		catalog, err := LoadDirectory(ctx, p.config)
		if err != nil {
			return nil, fmt.Errorf("reload private recipe directory: %w", err)
		}
		loadedFingerprint, err := directoryFingerprint(p.config)
		if err != nil {
			return nil, err
		}
		if loadedFingerprint != fingerprint {
			// An atomic installer changed the directory while this snapshot was
			// loading. Retry from the observed state; never label the older index
			// with the newer fingerprint and leave it silently stale.
			fingerprint = loadedFingerprint
			continue
		}
		p.catalog = catalog
		p.fingerprint = loadedFingerprint
		return catalog, nil
	}
	return nil, errors.New("private recipe directory changed repeatedly while reloading")
}

func LoadDirectory(ctx context.Context, config DirectoryConfig) (*Catalog, error) {
	if err := validateDirectoryRoot(config); err != nil {
		return nil, err
	}
	var recipes []Recipe
	err := filepath.WalkDir(config.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("recipe root contains symlink %s", filepath.Base(path))
		}
		if entry.IsDir() {
			if path != config.Root {
				if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
					return errors.New("recipe root must not contain a nested git checkout")
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("recipe directory %s permissions %o are too broad; require 0700 or stricter", filepath.Base(path), info.Mode().Perm())
			}
			return nil
		}
		if filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a regular recipe file", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("recipe file %s permissions %o are too broad; require 0600 or stricter", filepath.Base(path), info.Mode().Perm())
		}
		if info.Size() > 1<<20 {
			return fmt.Errorf("recipe file %s exceeds 1 MiB", filepath.Base(path))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		value, err := Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		recipes = append(recipes, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return NewCatalog(ctx, recipes, config.Embedder)
}

func NewCatalog(ctx context.Context, recipes []Recipe, embedder Embedder) (*Catalog, error) {
	catalog := &Catalog{embedder: embedder, entries: make([]catalogEntry, 0, len(recipes)), idf: map[string]float64{}}
	seen := map[string]bool{}
	docFrequency := map[string]int{}
	dimension := 0
	for _, value := range recipes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := Validate(value); err != nil {
			return nil, fmt.Errorf("recipe %q: %w", value.ID, err)
		}
		key := value.ID + "@" + value.Version
		if seen[key] {
			return nil, fmt.Errorf("duplicate recipe %s", key)
		}
		seen[key] = true
		digest, err := Digest(value)
		if err != nil {
			return nil, err
		}
		doc := document(value)
		terms := termFrequency(tokenize(doc))
		for term := range terms {
			docFrequency[term]++
		}
		entry := catalogEntry{recipe: cloneRecipe(value), digest: digest, doc: strings.ToLower(doc), terms: terms}
		if embedder != nil {
			vector, err := embedder.Embed(ctx, doc)
			if err != nil {
				return nil, fmt.Errorf("embed %s: %w", key, err)
			}
			entry.vector, err = normalize(vector)
			if err != nil {
				return nil, fmt.Errorf("embed %s: %w", key, err)
			}
			if dimension == 0 {
				dimension = len(entry.vector)
			} else if len(entry.vector) != dimension {
				return nil, errors.New("embedder returned inconsistent dimensions")
			}
		}
		catalog.entries = append(catalog.entries, entry)
	}
	for term, count := range docFrequency {
		catalog.idf[term] = math.Log(1+float64(len(catalog.entries))/float64(1+count)) + 1
	}
	sort.Slice(catalog.entries, func(i, j int) bool {
		if catalog.entries[i].recipe.ID == catalog.entries[j].recipe.ID {
			return catalog.entries[i].recipe.Version < catalog.entries[j].recipe.Version
		}
		return catalog.entries[i].recipe.ID < catalog.entries[j].recipe.ID
	})
	catalog.buildIndexes()
	return catalog, nil
}

func (c *Catalog) Search(ctx context.Context, query, origin string, limit int) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 1000 {
		return nil, errors.New("invalid recipe query")
	}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return nil, errors.New("recipe search limit must be one to 50")
	}
	if origin != "" {
		if err := validateOrigin(origin); err != nil {
			return nil, fmt.Errorf("invalid origin filter: %w", err)
		}
	}
	queryTerms := termFrequency(tokenize(query))
	var queryVector []float64
	if c.embedder != nil {
		vector, err := c.embedder.Embed(ctx, query)
		if err != nil {
			return nil, err
		}
		queryVector, err = normalize(vector)
		if err != nil {
			return nil, err
		}
	}
	candidates := c.candidates(queryTerms, origin, c.embedder != nil)
	matches := make(matchMinHeap, 0, min(limit, len(candidates)))
	for candidateIndex, entryIndex := range candidates {
		if candidateIndex&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		entry := c.entries[entryIndex]
		if latest, ok := c.latestByID[entry.recipe.ID]; ok && latest != entryIndex {
			continue
		}
		score := lexicalScore(queryTerms, entry.terms, c.idf)
		if strings.Contains(entry.doc, strings.ToLower(query)) {
			score += 2
		}
		if c.embedder != nil {
			if len(queryVector) != len(entry.vector) {
				return nil, errors.New("query embedding dimension mismatch")
			}
			// Embeddings drive ranking; lexical overlap remains a deterministic
			// tie-breaker and helps exact IDs/site names.
			score = 10*dot(queryVector, entry.vector) + score
		}
		if score <= 0 {
			continue
		}
		match := Match{
			ID: entry.recipe.ID, Version: entry.recipe.Version, Name: entry.recipe.Name,
			Description: entry.recipe.Description,
			Risk:        entry.recipe.Risk, Digest: entry.digest, Score: score,
		}
		if len(matches) < limit {
			match.Origins = append([]string(nil), entry.recipe.Origins...)
			heap.Push(&matches, match)
		} else if betterMatch(match, matches[0]) {
			match.Origins = append([]string(nil), entry.recipe.Origins...)
			heap.Pop(&matches)
			heap.Push(&matches, match)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return betterMatch(matches[i], matches[j]) })
	return []Match(matches), nil
}

func (c *Catalog) Fetch(_ context.Context, id, version, digest string) (Recipe, error) {
	if !recipeIDPattern.MatchString(id) || !versionPattern.MatchString(version) || len(digest) != 64 {
		return Recipe{}, errors.New("invalid recipe identity")
	}
	index, ok := c.byIdentity[id+"@"+version]
	if !ok {
		return Recipe{}, os.ErrNotExist
	}
	entry := c.entries[index]
	if entry.digest != digest {
		return Recipe{}, errors.New("recipe digest mismatch")
	}
	return cloneRecipe(entry.recipe), nil
}

func (c *Catalog) buildIndexes() {
	c.postings = make(map[string][]int, len(c.idf))
	c.originPostings = make(map[string][]int)
	c.byIdentity = make(map[string]int, len(c.entries))
	c.latestByID = make(map[string]int, len(c.entries))
	for index, entry := range c.entries {
		c.byIdentity[entry.recipe.ID+"@"+entry.recipe.Version] = index
		previous, found := c.latestByID[entry.recipe.ID]
		if !found || compareSemanticVersions(entry.recipe.Version, c.entries[previous].recipe.Version) > 0 {
			c.latestByID[entry.recipe.ID] = index
		}
		for term := range entry.terms {
			c.postings[term] = append(c.postings[term], index)
		}
		seenOrigins := map[string]bool{}
		for _, origin := range entry.recipe.Origins {
			if !seenOrigins[origin] {
				c.originPostings[origin] = append(c.originPostings[origin], index)
				seenOrigins[origin] = true
			}
		}
	}
}

func compareSemanticVersions(left, right string) int {
	leftCore, leftPre := splitSemanticVersion(left)
	rightCore, rightPre := splitSemanticVersion(right)
	for index := range 3 {
		if compared := compareNumericIdentifier(leftCore[index], rightCore[index]); compared != 0 {
			return compared
		}
	}
	if leftPre == "" && rightPre != "" {
		return 1
	}
	if leftPre != "" && rightPre == "" {
		return -1
	}
	if leftPre == rightPre {
		return 0
	}
	leftParts, rightParts := strings.Split(leftPre, "."), strings.Split(rightPre, ".")
	for index := 0; index < min(len(leftParts), len(rightParts)); index++ {
		leftNumeric, rightNumeric := allDigits(leftParts[index]), allDigits(rightParts[index])
		switch {
		case leftNumeric && rightNumeric:
			if compared := compareNumericIdentifier(leftParts[index], rightParts[index]); compared != 0 {
				return compared
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			if leftParts[index] < rightParts[index] {
				return -1
			}
			if leftParts[index] > rightParts[index] {
				return 1
			}
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1
	}
	if len(leftParts) > len(rightParts) {
		return 1
	}
	return 0
}

func splitSemanticVersion(value string) ([3]string, string) {
	withoutBuild := strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(withoutBuild, "-", 2)
	coreParts := strings.Split(parts[0], ".")
	core := [3]string{coreParts[0], coreParts[1], coreParts[2]}
	if len(parts) == 2 {
		return core, parts[1]
	}
	return core, ""
}

func compareNumericIdentifier(left, right string) int {
	left = strings.TrimLeft(left, "0")
	right = strings.TrimLeft(right, "0")
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (c *Catalog) candidates(queryTerms map[string]float64, origin string, semantic bool) []int {
	if semantic || len(queryTerms) == 0 {
		if origin != "" {
			return c.originPostings[origin]
		}
		out := make([]int, len(c.entries))
		for index := range out {
			out[index] = index
		}
		return out
	}
	if len(queryTerms) == 1 && origin == "" {
		for term := range queryTerms {
			return c.postings[term]
		}
	}
	seen := make(map[int]struct{})
	for term := range queryTerms {
		for _, index := range c.postings[term] {
			if origin == "" || contains(c.entries[index].recipe.Origins, origin) {
				seen[index] = struct{}{}
			}
		}
	}
	out := make([]int, 0, len(seen))
	for index := range seen {
		out = append(out, index)
	}
	sort.Ints(out)
	return out
}

type matchMinHeap []Match

func (h matchMinHeap) Len() int           { return len(h) }
func (h matchMinHeap) Less(i, j int) bool { return betterMatch(h[j], h[i]) }
func (h matchMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *matchMinHeap) Push(value any)    { *h = append(*h, value.(Match)) }
func (h *matchMinHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func betterMatch(left, right Match) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Version != right.Version {
		return left.Version < right.Version
	}
	return left.Digest < right.Digest
}

type HTTPProviderConfig struct {
	BaseURL        string
	Token          string
	Client         *http.Client
	RequestTimeout time.Duration
}

type HTTPProvider struct {
	baseURL string
	token   string
	client  *http.Client
	timeout time.Duration
}

func NewHTTPProvider(config HTTPProviderConfig) (*HTTPProvider, error) {
	base := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid recipe provider URL")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, errors.New("recipe provider URL must use HTTPS except on loopback")
	}
	if parsed.User != nil {
		return nil, errors.New("recipe provider URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("recipe provider URL must not contain a query or fragment")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("recipe provider request timeout must not be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	// Provider replies are authenticated inputs. Refuse redirects rather than
	// risk forwarding a bearer credential to a downgraded or substituted
	// endpoint; operators should configure the canonical base URL directly.
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("recipe provider redirects are not allowed")
	}
	return &HTTPProvider{baseURL: base, token: config.Token, client: &clientCopy, timeout: config.RequestTimeout}, nil
}

func (p *HTTPProvider) Search(ctx context.Context, query, origin string, limit int) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 1000 {
		return nil, errors.New("invalid recipe query")
	}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return nil, errors.New("recipe search limit must be one to 50")
	}
	if origin != "" {
		if err := validateOrigin(origin); err != nil {
			return nil, fmt.Errorf("invalid origin filter: %w", err)
		}
	}
	var out struct {
		Matches []Match `json:"matches"`
	}
	err := p.post(ctx, "/v1/recipes/search", map[string]any{"query": query, "origin": origin, "limit": limit}, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Matches) > limit {
		return nil, errors.New("recipe provider returned more matches than requested")
	}
	seen := map[string]bool{}
	for index, match := range out.Matches {
		if err := validateMatch(match, origin); err != nil {
			return nil, fmt.Errorf("recipe provider match %d: %w", index+1, err)
		}
		key := match.ID + "@" + match.Version
		if seen[key] {
			return nil, errors.New("recipe provider returned a duplicate match")
		}
		seen[key] = true
	}
	return out.Matches, nil
}

func validateMatch(match Match, requestedOrigin string) error {
	if !recipeIDPattern.MatchString(match.ID) || !versionPattern.MatchString(match.Version) {
		return errors.New("invalid recipe identity")
	}
	if strings.TrimSpace(match.Name) == "" || strings.TrimSpace(match.Description) == "" || len(match.Name) > 200 || len(match.Description) > 2000 {
		return errors.New("invalid recipe name or description")
	}
	if match.Risk != "read_only" && match.Risk != "external_write" {
		return errors.New("invalid recipe risk")
	}
	if len(match.Origins) == 0 || len(match.Origins) > 32 {
		return errors.New("invalid recipe origins")
	}
	for _, origin := range match.Origins {
		if err := validateOrigin(origin); err != nil {
			return err
		}
	}
	if requestedOrigin != "" && !contains(match.Origins, requestedOrigin) {
		return errors.New("provider returned a match outside the requested origin")
	}
	digest, err := hex.DecodeString(match.Digest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("invalid recipe digest")
	}
	if math.IsNaN(match.Score) || math.IsInf(match.Score, 0) {
		return errors.New("invalid recipe score")
	}
	encoded, _ := json.Marshal(match)
	if secretPattern.Match(encoded) || refPattern.Match(encoded) {
		return errors.New("recipe metadata is not disclosure-safe")
	}
	return nil
}

func (p *HTTPProvider) Fetch(ctx context.Context, id, version, digest string) (Recipe, error) {
	decodedDigest, digestErr := hex.DecodeString(digest)
	if !recipeIDPattern.MatchString(id) || !versionPattern.MatchString(version) || digestErr != nil || len(decodedDigest) != sha256.Size {
		return Recipe{}, errors.New("invalid recipe identity")
	}
	var out struct {
		Recipe Recipe `json:"recipe"`
	}
	if err := p.post(ctx, "/v1/recipes/fetch", map[string]string{"id": id, "version": version, "digest": digest}, &out); err != nil {
		return Recipe{}, err
	}
	if err := Validate(out.Recipe); err != nil {
		return Recipe{}, fmt.Errorf("provider returned invalid recipe: %w", err)
	}
	actual, err := Digest(out.Recipe)
	if err != nil {
		return Recipe{}, err
	}
	if actual != digest || out.Recipe.ID != id || out.Recipe.Version != version {
		return Recipe{}, errors.New("provider returned a recipe that does not match the pinned identity")
	}
	return out.Recipe, nil
}

func (p *HTTPProvider) post(ctx context.Context, path string, body, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	if p.token != "" {
		req.Header.Set("authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, (8<<20)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return errors.New("recipe provider response exceeds 8 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("recipe provider returned %s", resp.Status)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("recipe provider response contains trailing JSON")
	}
	return nil
}

func EnsurePrivateRoot(recipeRoot, repositoryRoot string) error {
	if !filepath.IsAbs(recipeRoot) || !filepath.IsAbs(repositoryRoot) {
		return errors.New("recipe and repository roots must be absolute")
	}
	recipeResolved, err := resolveExistingPrefix(recipeRoot)
	if err != nil {
		return err
	}
	repositoryResolved, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(repositoryResolved, recipeResolved)
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return errors.New("recipe root must be outside the open-source repository")
	}
	return nil
}

func validateDirectoryRoot(config DirectoryConfig) error {
	if !filepath.IsAbs(config.Root) {
		return errors.New("recipe root must be absolute")
	}
	resolvedRoot, err := resolveExistingPrefix(config.Root)
	if err != nil {
		return err
	}
	if err := rejectBroadRecipeRoot(resolvedRoot); err != nil {
		return err
	}
	if err := EnsureOutsideGitCheckout(resolvedRoot); err != nil {
		return err
	}
	if config.RepositoryRoot != "" {
		if err := EnsurePrivateRoot(config.Root, config.RepositoryRoot); err != nil {
			return err
		}
	}
	info, err := os.Lstat(config.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("recipe root must be a real directory, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("recipe root permissions %o are too broad; require 0700 or stricter", info.Mode().Perm())
	}
	return nil
}

// PreparePrivateDirectory creates a dedicated local recipe root without ever
// placing it in a Git checkout. Existing roots are tightened to owner-only
// permissions; files within them are still validated independently on load.
func PreparePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("recipe root must be absolute")
	}
	resolved, err := resolveExistingPrefix(path)
	if err != nil {
		return err
	}
	if err := rejectBroadRecipeRoot(resolved); err != nil {
		return err
	}
	if err := EnsureOutsideGitCheckout(resolved); err != nil {
		return err
	}
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recipe root must be a real directory, not a symlink")
		}
	case errors.Is(statErr, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	default:
		return statErr
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("recipe root must be a real directory, not a symlink")
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return errors.New("recipe root changed while opening")
	}
	// Change permissions through the verified descriptor. A path-based chmod
	// could follow a symlink swapped in after Lstat and modify another target.
	if err := directory.Chmod(0o700); err != nil {
		return err
	}
	verified, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !verified.IsDir() || verified.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, verified) {
		return errors.New("recipe root changed while securing")
	}
	resolved, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	return EnsureOutsideGitCheckout(resolved)
}

// EnsureOutsideGitCheckout rejects any recipe root nested beneath a directory
// containing .git, independent of the daemon's current working directory.
func EnsureOutsideGitCheckout(path string) error {
	resolved, err := resolveExistingPrefix(path)
	if err != nil {
		return err
	}
	current := filepath.Clean(resolved)
	if info, statErr := os.Lstat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	for ; ; current = filepath.Dir(current) {
		if info, statErr := os.Lstat(filepath.Join(current, ".git")); statErr == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return errors.New("recipe root must be outside every git checkout")
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func directoryFingerprint(config DirectoryConfig) (string, error) {
	if err := validateDirectoryRoot(config); err != nil {
		return "", err
	}
	hash := sha256.New()
	err := filepath.WalkDir(config.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("recipe root contains symlink %s", filepath.Base(path))
		}
		if entry.IsDir() && path != config.Root {
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				return errors.New("recipe root must not contain a nested git checkout")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("recipe directory %s permissions %o are too broad; require 0700 or stricter", filepath.Base(path), info.Mode().Perm())
			}
			return nil
		}
		if filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular recipe file", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("recipe file %s permissions %o are too broad; require 0600 or stricter", filepath.Base(path), info.Mode().Perm())
		}
		if info.Size() > 1<<20 {
			return fmt.Errorf("recipe file %s exceeds 1 MiB", filepath.Base(path))
		}
		relative, err := filepath.Rel(config.Root, path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%o\n", relative, info.Size(), info.ModTime().UnixNano(), info.Mode().Perm())
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func rejectBroadRecipeRoot(path string) error {
	clean := filepath.Clean(path)
	volumeRoot := filepath.Clean(filepath.VolumeName(clean) + string(filepath.Separator))
	if clean == volumeRoot || filepath.Dir(clean) == volumeRoot {
		return errors.New("recipe root is too broad; use a dedicated private child directory")
	}
	for _, resolve := range []func() (string, error){os.UserHomeDir, os.UserCacheDir, os.UserConfigDir} {
		candidate, err := resolve()
		if err == nil && (clean == filepath.Clean(candidate) || filepath.Dir(clean) == filepath.Clean(candidate)) {
			return errors.New("recipe root is too broad; use a dedicated private child directory")
		}
	}
	if temp := filepath.Clean(os.TempDir()); clean == temp || filepath.Dir(clean) == temp {
		return errors.New("recipe root is too broad; use a dedicated private child directory")
	}
	return nil
}

func resolveExistingPrefix(path string) (string, error) {
	current := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func document(value Recipe) string {
	return strings.Join([]string{value.ID, value.Name, value.Description, strings.Join(value.Intents, " "), strings.Join(value.Origins, " ")}, "\n")
}

func tokenize(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func termFrequency(tokens []string) map[string]float64 {
	out := map[string]float64{}
	for _, token := range tokens {
		if len(token) > 1 {
			out[token]++
		}
	}
	if len(tokens) > 0 {
		for token, count := range out {
			out[token] = count / float64(len(tokens))
		}
	}
	return out
}

func lexicalScore(query, doc map[string]float64, idf map[string]float64) float64 {
	var score float64
	for term, weight := range query {
		if docWeight := doc[term]; docWeight > 0 {
			score += weight * docWeight * idf[term]
		}
	}
	return score
}

func normalize(vector []float64) ([]float64, error) {
	if len(vector) == 0 {
		return nil, errors.New("embedding is empty")
	}
	var total float64
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("embedding contains non-finite value")
		}
		total += value * value
	}
	if total == 0 {
		return nil, errors.New("embedding has zero magnitude")
	}
	scale := 1 / math.Sqrt(total)
	out := make([]float64, len(vector))
	for index, value := range vector {
		out[index] = value * scale
	}
	return out, nil
}

func dot(left, right []float64) float64 {
	var out float64
	for index := range left {
		out += left[index] * right[index]
	}
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneRecipe(value Recipe) Recipe {
	cloned := value
	cloned.Intents = append([]string(nil), value.Intents...)
	cloned.Origins = append([]string(nil), value.Origins...)
	if value.Inputs != nil {
		cloned.Inputs = make(map[string]Input, len(value.Inputs))
		for name, input := range value.Inputs {
			cloned.Inputs[name] = input
		}
	}
	if value.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(value.Metadata))
		for key, item := range value.Metadata {
			cloned.Metadata[key] = item
		}
	}
	cloned.Steps = append([]Step(nil), value.Steps...)
	for index := range cloned.Steps {
		cloned.Steps[index].Target = cloneTarget(value.Steps[index].Target)
		cloned.Steps[index].Event = cloneEvent(value.Steps[index].Event)
		cloned.Steps[index].Postcondition = cloneEvent(value.Steps[index].Postcondition)
		if value.Steps[index].Capture != nil {
			capture := *value.Steps[index].Capture
			capture.Target = cloneTarget(value.Steps[index].Capture.Target)
			cloned.Steps[index].Capture = &capture
		}
	}
	return cloned
}

func cloneTarget(value *Target) *Target {
	if value == nil {
		return nil
	}
	cloned := *value
	if value.Visible != nil {
		visible := *value.Visible
		cloned.Visible = &visible
	}
	return &cloned
}

func cloneEvent(value *Event) *Event {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Target = cloneTarget(value.Target)
	return &cloned
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
