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
type BaselineStore interface {
	BaselineRouter
	PutBaseline(context.Context, baseline.Record) error
	LoadBaseline(context.Context, baseline.Key) (baseline.Record, bool, error)
	BaselineEnvironments(context.Context, string, int) ([]baseline.Environment, error)
	DeleteBaseline(context.Context, baseline.Key) error
	// BaselineLocation names the destination for a result to report.
	BaselineLocation() string
}

// BaselineRouter answers where one capture belongs.
type BaselineRouter interface {
	// RouteBaseline reports where one capture belongs.
	RouteBaseline(ctx context.Context, digest, pageURL string) (BaselineRoute, error)
}

// BaselineRoute is a provider's answer about one capture: the destination, or a refusal to pick one.
type BaselineRoute struct {
	destination BaselineDestination
}

// BaselineDestination is where one capture belongs, as a closed set.
type BaselineDestination string

const (
	// BaselineUnclassified is the zero value: a route that never went through NewBaselineRoute.
	BaselineUnclassified BaselineDestination = ""
	// BaselineLocal: nothing the provider holds claims this capture, so it goes to the operator-configured local root as it did before providers could hold baselines at all.
	BaselineLocal BaselineDestination = "local"
	// BaselineProvider: the provider holds the recipe the digest pins AND that recipe declares the origin of the page being captured.
	BaselineProvider BaselineDestination = "provider"
	// BaselineRefusedPageOutsideRecipe: the provider owns the recipe the digest pins and that recipe does not visit this page.
	BaselineRefusedPageOutsideRecipe BaselineDestination = "refused_page_outside_recipe"
	// BaselineRefusedProviderReachesPage: the provider has some recipe for this page's origin and the digest is not one of its recipes.
	BaselineRefusedProviderReachesPage BaselineDestination = "refused_provider_reaches_page"
)

// BaselineOwnership is what one provider knows about one capture, before the rule that turns it into a destination.
type BaselineOwnership struct {
	// PageURL is the page being captured, or empty for an action that reads no page.
	PageURL string
	// OwnsRecipe: the provider holds the recipe version the digest pins.
	OwnsRecipe bool
	// RecipeVisitsPage: that same recipe declares the page's origin among its own.
	RecipeVisitsPage bool
	// OwnsPageOrigin: the provider holds SOME recipe for the page's origin, whatever the caller's digest says.
	OwnsPageOrigin bool
}

// NewBaselineRoute applies the routing rule once, for every router.
func NewBaselineRoute(ownership BaselineOwnership) BaselineRoute {
	page := strings.TrimSpace(ownership.PageURL)
	switch {
	case ownership.OwnsRecipe && (page == "" || ownership.RecipeVisitsPage):
		return BaselineRoute{destination: BaselineProvider}
	case ownership.OwnsRecipe:
		return BaselineRoute{destination: BaselineRefusedPageOutsideRecipe}
	case page != "" && ownership.OwnsPageOrigin:
		return BaselineRoute{destination: BaselineRefusedProviderReachesPage}
	default:
		return BaselineRoute{destination: BaselineLocal}
	}
}

// Destination names where the capture belongs.
func (r BaselineRoute) Destination() BaselineDestination { return r.destination }

// ParseBaselineDestination rebuilds a route from the wire, for the proxy hop.
func ParseBaselineDestination(value string) (BaselineRoute, error) {
	switch BaselineDestination(strings.TrimSpace(value)) {
	case BaselineLocal:
		return BaselineRoute{destination: BaselineLocal}, nil
	case BaselineProvider:
		return BaselineRoute{destination: BaselineProvider}, nil
	case BaselineRefusedPageOutsideRecipe:
		return BaselineRoute{destination: BaselineRefusedPageOutsideRecipe}, nil
	case BaselineRefusedProviderReachesPage:
		return BaselineRoute{destination: BaselineRefusedProviderReachesPage}, nil
	default:
		return BaselineRoute{}, fmt.Errorf("unknown baseline destination %q", value)
	}
}

const baselineEnvelopeHeadroom = 1 << 20

const maxBaselineImageBytes = (maxProviderResponseBytes - baselineEnvelopeHeadroom) / 4 * 3

type baselineWire struct {
	RecipeDigest string               `json:"recipe_digest"`
	StepIndex    int                  `json:"step_index"`
	Environment  baseline.Environment `json:"environment"`
	Fingerprint  string               `json:"environment_fingerprint"`
	// ScreenshotPNG is base64 because this is a JSON API.
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

// ProviderBaselines adapts a BaselineStore to the baseline.Storage a check runs against.
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

// BaselineRoot is the reserved subdirectory a DirectoryProvider keeps baselines in, inside the private recipe root it already owns.
const BaselineRoot = "baselines"

const baselineRecordFile = "baseline.json"

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
	origin := originOf(pageURL)
	return NewBaselineRoute(BaselineOwnership{
		PageURL:          pageURL,
		OwnsRecipe:       catalog.Owns(digest),
		RecipeVisitsPage: catalog.RecipeVisitsOrigin(digest, origin),
		OwnsPageOrigin:   catalog.OwnsOrigin(origin),
	}), nil
}

func (p *DirectoryProvider) PutBaseline(_ context.Context, record baseline.Record) error {

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

// RecipeVisitsOrigin reports whether the recipe version digest pins declares origin among its own.
func (c *Catalog) RecipeVisitsOrigin(digest, origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	normalized, err := baseline.NormalizeRecipeDigest(digest)
	if err != nil {
		return false
	}
	index, ok := c.byDigest[normalized]
	if !ok {
		return false
	}
	for _, declared := range c.entries[index].recipe.Origins {
		if strings.EqualFold(strings.TrimSpace(declared), origin) {
			return true
		}
	}
	return false
}

// OwnsOrigin reports whether this catalog holds any recipe for origin.
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

	origin := originOf(pageURL)
	if origin != "" {
		request["origin"] = origin
	}

	var out struct {
		Owns       *bool `json:"owns"`
		OwnsOrigin *bool `json:"owns_origin"`
		// CoversPage is the binding in the other direction: the recipe the digest pins declares the page's origin among its own.
		CoversPage *bool `json:"covers_page"`
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
	if origin != "" && *out.Owns && out.CoversPage == nil {
		return BaselineRoute{}, errors.New("recipe provider answered the baseline routing question without covers_page, so a capture of a page this recipe never visits cannot be told from one it does")
	}
	return NewBaselineRoute(BaselineOwnership{
		PageURL:          pageURL,
		OwnsRecipe:       *out.Owns,
		RecipeVisitsPage: out.CoversPage != nil && *out.CoversPage,
		OwnsPageOrigin:   origin != "" && out.OwnsOrigin != nil && *out.OwnsOrigin,
	}), nil
}

func (p *HTTPProvider) PutBaseline(ctx context.Context, record baseline.Record) error {
	wire, err := encodeBaseline(record)
	if err != nil {
		return err
	}

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

var (
	_ BaselineStore = (*DirectoryProvider)(nil)
	_ BaselineStore = (*HTTPProvider)(nil)
)
