package recipe

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/snapshot"
)

// A baseline of a private recipe belongs with the private recipe.
//
// The store ships with an operator-configured root and a refusal to sit inside
// a Git checkout, which stops the worst outcome; where a private-page baseline
// SHOULD go was left to an instruction in the docs. A screenshot of a signed-in
// page is the same class of thing as the recipe that navigated to it, and the
// provider is the party that already holds one of them — so brw puts the
// baseline there rather than asking an operator to remember to.
//
// BaselineStore is the provider's side of that. It is a separate interface from
// Provider for the reason DraftWriter is: reading recipes and writing captures
// of the pages they visit are different privileges, and a provider that
// implements neither still serves recipes.
type BaselineStore interface {
	BaselineRouter
	PutBaseline(context.Context, baseline.Record) error
	LoadBaseline(context.Context, baseline.Key) (baseline.Record, bool, error)
	BaselineEnvironments(context.Context, string, int) ([]baseline.Environment, error)
	DeleteBaseline(context.Context, baseline.Key) error
	// BaselineLocation names the destination for a result to report.
	BaselineLocation() string
}

// BaselineRouter answers where one capture belongs. It is split out of
// BaselineStore because a daemon that proxies its browser host can answer the
// question and cannot store the answer: the provider is upstream. Routing that
// a proxy could not ask about would fall back to that proxy's local root, which
// is the outcome this whole path exists to prevent.
type BaselineRouter interface {
	// RouteBaseline reports what this provider owns of one capture: the recipe
	// the digest pins, and the page the capture is of.
	//
	// pageURL is the full URL of the page being captured, or empty for an action
	// that reads no page. Only its ORIGIN is ever sent to a provider — the path
	// and query of a signed-in page are the private part, and the routing
	// question does not need them.
	RouteBaseline(ctx context.Context, digest, pageURL string) (BaselineRoute, error)
}

// BaselineRoute is a provider's answer about one capture.
//
// Two questions rather than one, because a destination decided from the digest
// alone is decided from an argument the caller invents. An agent sitting on a
// page a private recipe reached can pass any well-formed digest the provider
// does not own; without OwnsOrigin the screenshot and the page's accessible
// names would then be written to the local root, which is the leak the routing
// exists to close.
type BaselineRoute struct {
	// OwnsRecipe is true when the provider holds the recipe version the digest
	// pins. Its baselines belong with the provider.
	OwnsRecipe bool
	// OwnsOrigin is true when the provider holds ANY recipe for the origin the
	// page being captured is showing. With OwnsRecipe false it is a refusal, not
	// a destination: brw does not know which of the provider's recipes this
	// capture belongs to, and will not guess by writing it locally.
	OwnsOrigin bool
}

// baselineEnvelopeHeadroom is what a fetch response costs beyond the base64
// image: the key, the environment fingerprint, the ARIA tree and the JSON
// around them.
const baselineEnvelopeHeadroom = 1 << 20

// maxBaselineImageBytes bounds one stored screenshot on the wire.
//
// Derived from the response cap rather than picked: base64 inflates by 4/3, so
// an image the put accepts that no fetch can return is a baseline stored and
// never usable again — the failure the round-trip check in PutBaseline says it
// prevents. TestBaselineImageBoundFitsTheProviderResponseCap pins the
// arithmetic. A viewport capture is tens of kilobytes (both transports
// downscale to 800px wide), so this stays generous by orders of magnitude and
// small enough that a provider cannot be used as a blob store through it.
const maxBaselineImageBytes = (maxProviderResponseBytes - baselineEnvelopeHeadroom) / 4 * 3

// baselineWire is the interchange format: the recipe/step key, the environment
// fingerprint the capture was taken under, and the image bytes.
//
// The fingerprint is carried explicitly ALONGSIDE the environment it was
// computed from, and checked on the way in and on the way back, because it is
// the thing the store is keyed by. A provider that stored a record under a
// fingerprint that does not describe the environment in it would answer later
// checks with a baseline taken on another machine, which is precisely the
// failure the key exists to prevent.
type baselineWire struct {
	RecipeDigest string               `json:"recipe_digest"`
	StepIndex    int                  `json:"step_index"`
	Environment  baseline.Environment `json:"environment"`
	Fingerprint  string               `json:"environment_fingerprint"`
	// ScreenshotPNG is base64 because this is a JSON API. The bytes are a PNG:
	// the daemon re-encodes whatever the transport captured before it stores
	// anything, so a provider holding these can render them.
	ScreenshotPNG string                  `json:"screenshot_png_base64"`
	AriaTree      snapshot.AriaTree       `json:"aria_tree"`
	IgnoreRegions []baseline.IgnoreRegion `json:"ignore_regions,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
}

func encodeBaseline(record baseline.Record) (baselineWire, error) {
	if err := record.Key.Validate(); err != nil {
		return baselineWire{}, err
	}
	if len(record.Screenshot) == 0 {
		return baselineWire{}, errors.New("a baseline needs a screenshot")
	}
	if len(record.Screenshot) > maxBaselineImageBytes {
		return baselineWire{}, fmt.Errorf("baseline screenshot exceeds %d bytes", maxBaselineImageBytes)
	}
	environment := record.Key.Environment.Normalize()
	created := record.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return baselineWire{
		RecipeDigest:  strings.ToLower(strings.TrimSpace(record.Key.RecipeDigest)),
		StepIndex:     record.Key.StepIndex,
		Environment:   environment,
		Fingerprint:   environment.Fingerprint(),
		ScreenshotPNG: base64.StdEncoding.EncodeToString(record.Screenshot),
		AriaTree:      record.Tree,
		IgnoreRegions: record.IgnoreRegions,
		CreatedAt:     created.UTC(),
	}, nil
}

// decodeBaseline turns a provider's answer back into a record, refusing one
// that does not describe the key that was asked for. The provider is
// authenticated, not trusted: a record for another step or another environment
// would be compared against this page and reported as a regression in it.
func decodeBaseline(wire baselineWire, want baseline.Key) (baseline.Record, error) {
	environment := wire.Environment.Normalize()
	if err := environment.Validate(); err != nil {
		return baseline.Record{}, fmt.Errorf("provider returned a baseline with an unusable environment: %w", err)
	}
	if environment.Fingerprint() != strings.TrimSpace(wire.Fingerprint) {
		return baseline.Record{}, errors.New("provider returned a baseline whose fingerprint does not describe its environment")
	}
	key := baseline.Key{RecipeDigest: wire.RecipeDigest, StepIndex: wire.StepIndex, Environment: environment}
	if err := key.Validate(); err != nil {
		return baseline.Record{}, fmt.Errorf("provider returned an invalid baseline key: %w", err)
	}
	if key.ID() != want.ID() {
		return baseline.Record{}, errors.New("provider returned a baseline for a different key")
	}
	image, err := base64.StdEncoding.DecodeString(wire.ScreenshotPNG)
	if err != nil {
		return baseline.Record{}, errors.New("provider returned a baseline screenshot that is not base64")
	}
	if len(image) == 0 {
		return baseline.Record{}, errors.New("provider returned a baseline with no screenshot")
	}
	if len(image) > maxBaselineImageBytes {
		return baseline.Record{}, fmt.Errorf("provider returned a baseline screenshot over %d bytes", maxBaselineImageBytes)
	}
	// Decoded rather than trusted by extension: the comparison reads pixels out
	// of this, and "the file is a PNG" is not something to take on the word of
	// whoever served it.
	image, err = baseline.NormalizePNG(image)
	if err != nil {
		return baseline.Record{}, fmt.Errorf("provider returned a baseline screenshot that is not a decodable image: %w", err)
	}
	return baseline.Record{
		Key:           key,
		Environment:   environment,
		Tree:          wire.AriaTree,
		IgnoreRegions: wire.IgnoreRegions,
		CreatedAt:     wire.CreatedAt,
		Screenshot:    image,
	}, nil
}

// ProviderBaselines adapts a BaselineStore to the baseline.Storage a check
// runs against.
//
// The context is held in the adapter because baseline.Storage takes none: it
// is the shape of a local directory, where every call is a file system call.
// One adapter is built per request and discarded with it, so the context it
// carries is that request's and outlives nothing.
func ProviderBaselines(ctx context.Context, store BaselineStore) baseline.Storage {
	return providerStorage{ctx: ctx, store: store}
}

type providerStorage struct {
	ctx   context.Context
	store BaselineStore
}

func (p providerStorage) Load(key baseline.Key) (baseline.Record, bool, error) {
	if err := key.Validate(); err != nil {
		return baseline.Record{}, false, err
	}
	return p.store.LoadBaseline(p.ctx, key)
}

func (p providerStorage) Save(record baseline.Record) error {
	return p.store.PutBaseline(p.ctx, record)
}

func (p providerStorage) EnvironmentsFor(digest string, step int) ([]baseline.Environment, error) {
	return p.store.BaselineEnvironments(p.ctx, digest, step)
}

func (p providerStorage) Delete(key baseline.Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return p.store.DeleteBaseline(p.ctx, key)
}

func (p providerStorage) Location() string { return p.store.BaselineLocation() }

// BaselineRoot is the reserved subdirectory a DirectoryProvider keeps baselines
// in, inside the private recipe root it already owns. It is reserved rather
// than configurable so that the recipe walk can skip exactly one name: a
// baseline record is a .json file, and a .json file anywhere under the recipe
// root is parsed as a recipe.
const BaselineRoot = "baselines"

// baselineRecordFile is the only .json file name the reserved subtree may hold.
// It is baseline.Store's own record name; a mismatch would make the guard in
// checkReservedBaselineDir refuse the provider's own writes, so the two are
// pinned together by TestReservedBaselineDirAcceptsWhatTheStoreWrites.
const baselineRecordFile = "baseline.json"

// baselineStore lazily opens the directory provider's baseline set.
//
// It reuses baseline.Store rather than writing files here: that type is where
// the traversal check on a caller-supplied digest, the 0700 refusal and the
// ignore-everything file live, and a second implementation would be a second
// set of those rules to keep right.
func (p *DirectoryProvider) baselineStore() (*baseline.Store, error) {
	p.baselineMu.Lock()
	defer p.baselineMu.Unlock()
	if p.baselines != nil {
		return p.baselines, nil
	}
	store, err := baseline.NewStore(filepath.Join(p.config.Root, BaselineRoot))
	if err != nil {
		return nil, fmt.Errorf("open the private recipe provider's baseline store: %w", err)
	}
	p.baselines = store
	return store, nil
}

func (p *DirectoryProvider) RouteBaseline(ctx context.Context, digest, pageURL string) (BaselineRoute, error) {
	catalog, err := p.current(ctx)
	if err != nil {
		return BaselineRoute{}, err
	}
	return BaselineRoute{
		OwnsRecipe: catalog.Owns(digest),
		OwnsOrigin: catalog.OwnsOrigin(originOf(pageURL)),
	}, nil
}

func (p *DirectoryProvider) PutBaseline(_ context.Context, record baseline.Record) error {
	// Encoded and decoded through the same wire format the HTTPS provider uses,
	// so a baseline that one implementation accepts is one the other accepts.
	// Without it the directory provider would be the lenient one, and a corpus
	// written against it would fail to move to a hosted provider later.
	wire, err := encodeBaseline(record)
	if err != nil {
		return err
	}
	decoded, err := decodeBaseline(wire, record.Key)
	if err != nil {
		return err
	}
	store, err := p.baselineStore()
	if err != nil {
		return err
	}
	return store.Save(decoded)
}

func (p *DirectoryProvider) LoadBaseline(_ context.Context, key baseline.Key) (baseline.Record, bool, error) {
	store, err := p.baselineStore()
	if err != nil {
		return baseline.Record{}, false, err
	}
	return store.Load(key)
}

func (p *DirectoryProvider) BaselineEnvironments(_ context.Context, digest string, step int) ([]baseline.Environment, error) {
	store, err := p.baselineStore()
	if err != nil {
		return nil, err
	}
	return store.EnvironmentsFor(digest, step)
}

func (p *DirectoryProvider) DeleteBaseline(_ context.Context, key baseline.Key) error {
	store, err := p.baselineStore()
	if err != nil {
		return err
	}
	return store.Delete(key)
}

func (p *DirectoryProvider) BaselineLocation() string {
	return "the private recipe provider at " + filepath.Join(p.config.Root, BaselineRoot)
}

// Owns reports whether this catalog holds the recipe version digest pins.
func (c *Catalog) Owns(digest string) bool {
	normalized, err := baseline.NormalizeRecipeDigest(digest)
	if err != nil {
		return false
	}
	_, ok := c.byDigest[normalized]
	return ok
}

// OwnsOrigin reports whether this catalog holds any recipe for origin.
//
// Compared case-insensitively: a recipe declares its origin and a page reports
// one, and a host that differs only in case is the same site. An empty origin
// owns nothing, so an action that reads no page is never routed by this.
func (c *Catalog) OwnsOrigin(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	for known, postings := range c.originPostings {
		if len(postings) > 0 && strings.EqualFold(known, origin) {
			return true
		}
	}
	return false
}

func (p *HTTPProvider) RouteBaseline(ctx context.Context, digest, pageURL string) (BaselineRoute, error) {
	normalized, err := baseline.NormalizeRecipeDigest(digest)
	if err != nil {
		return BaselineRoute{}, err
	}
	request := map[string]string{"recipe_digest": normalized}
	// Only the origin crosses to the provider. A signed-in page's path and query
	// are the private part, and "do you have a recipe for this site" does not
	// need them.
	origin := originOf(pageURL)
	if origin != "" {
		request["origin"] = origin
	}
	// Pointers because an ABSENT answer is not a "no". A provider that does not
	// implement half of this question cannot be told apart from one that has no
	// recipe for the page, and the difference decides whether a capture of a
	// signed-in page is written to the local root.
	var out struct {
		Owns       *bool `json:"owns"`
		OwnsOrigin *bool `json:"owns_origin"`
	}
	if err := p.post(ctx, "/v1/baselines/owner", request, &out); err != nil {
		return BaselineRoute{}, err
	}
	if out.Owns == nil {
		return BaselineRoute{}, errors.New("recipe provider answered the baseline routing question without owns")
	}
	if origin != "" && out.OwnsOrigin == nil {
		return BaselineRoute{}, errors.New("recipe provider answered the baseline routing question without owns_origin, so a page its recipes reach cannot be told from one they do not")
	}
	return BaselineRoute{OwnsRecipe: *out.Owns, OwnsOrigin: origin != "" && *out.OwnsOrigin}, nil
}

func (p *HTTPProvider) PutBaseline(ctx context.Context, record baseline.Record) error {
	wire, err := encodeBaseline(record)
	if err != nil {
		return err
	}
	// Round-tripped before it is sent, for the same reason the directory
	// provider does it: the write must refuse locally anything the read would
	// refuse coming back, or a baseline can be stored and never usable again.
	if _, err := decodeBaseline(wire, record.Key); err != nil {
		return err
	}
	var out struct {
		Stored bool `json:"stored"`
	}
	if err := p.post(ctx, "/v1/baselines/put", wire, &out); err != nil {
		return err
	}
	if !out.Stored {
		return errors.New("recipe provider did not acknowledge storing the baseline")
	}
	return nil
}

func (p *HTTPProvider) LoadBaseline(ctx context.Context, key baseline.Key) (baseline.Record, bool, error) {
	if err := key.Validate(); err != nil {
		return baseline.Record{}, false, err
	}
	var out struct {
		Found    bool         `json:"found"`
		Baseline baselineWire `json:"baseline"`
	}
	request := map[string]any{
		"recipe_digest":           strings.ToLower(strings.TrimSpace(key.RecipeDigest)),
		"step_index":              key.StepIndex,
		"environment_fingerprint": key.Environment.Fingerprint(),
	}
	if err := p.post(ctx, "/v1/baselines/fetch", request, &out); err != nil {
		return baseline.Record{}, false, err
	}
	if !out.Found {
		return baseline.Record{}, false, nil
	}
	record, err := decodeBaseline(out.Baseline, key)
	if err != nil {
		return baseline.Record{}, false, err
	}
	return record, true, nil
}

func (p *HTTPProvider) BaselineEnvironments(ctx context.Context, digest string, step int) ([]baseline.Environment, error) {
	normalized, err := baseline.NormalizeRecipeDigest(digest)
	if err != nil {
		return nil, err
	}
	if step < 0 {
		return nil, errors.New("step_index must not be negative")
	}
	var out struct {
		Environments []baseline.Environment `json:"environments"`
	}
	request := map[string]any{"recipe_digest": normalized, "step_index": step}
	if err := p.post(ctx, "/v1/baselines/environments", request, &out); err != nil {
		return nil, err
	}
	// An environment that does not validate is dropped rather than reported: it
	// exists to explain a mismatch in words ("device_pixel_ratio 1 -> 2"), and
	// half a fingerprint explains nothing.
	kept := make([]baseline.Environment, 0, len(out.Environments))
	for _, environment := range out.Environments {
		normalizedEnvironment := environment.Normalize()
		if normalizedEnvironment.Validate() != nil {
			continue
		}
		kept = append(kept, normalizedEnvironment)
	}
	return kept, nil
}

func (p *HTTPProvider) DeleteBaseline(ctx context.Context, key baseline.Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	var out struct {
		Deleted bool `json:"deleted"`
	}
	request := map[string]any{
		"recipe_digest":           strings.ToLower(strings.TrimSpace(key.RecipeDigest)),
		"step_index":              key.StepIndex,
		"environment_fingerprint": key.Environment.Fingerprint(),
	}
	if err := p.post(ctx, "/v1/baselines/delete", request, &out); err != nil {
		return err
	}
	if !out.Deleted {
		return errors.New("no baseline stored for that key")
	}
	return nil
}

func (p *HTTPProvider) BaselineLocation() string {
	return "the private recipe provider at " + p.baseURL
}

// compile-time assertions that both providers carry the capability. A provider
// that loses a method stops being a BaselineStore silently otherwise: the
// daemon type-asserts for it and would simply route every baseline to the local
// root again.
var (
	_ BaselineStore = (*DirectoryProvider)(nil)
	_ BaselineStore = (*HTTPProvider)(nil)
)
