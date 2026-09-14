// Package baseline turns a one-off page comparison into a regression gate.
//
// brw_diff answers "did my click change the page" against a mark taken seconds
// ago. A baseline answers "did this page change since the last release", which
// needs three things brw_diff has none of: a stored set rather than one live
// capture, a key that says what the stored capture was captured UNDER, and an
// update path that only runs when someone asks for it.
//
// The key is (recipe identity digest, step index, environment fingerprint). The
// environment is in the key because a screenshot taken at a different device
// pixel ratio, viewport, locale or browser build is not a worse version of the
// same picture — it is a different picture, and comparing the two produces a
// diff that is real, meaningless and impossible to act on. Keying on it turns
// that into a reported environment mismatch instead.
package baseline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Environment is the fingerprinted capture condition.
type Environment struct {
	// BrowserBuild is the browser's version string, for example "Chrome/141.0.0.0".
	BrowserBuild     string  `json:"browser_build"`
	ViewportWidth    int     `json:"viewport_width"`
	ViewportHeight   int     `json:"viewport_height"`
	DevicePixelRatio float64 `json:"device_pixel_ratio"`
	Locale           string  `json:"locale"`
	// OS is the browser host's operating system, as Go names it.
	OS string `json:"os"`
}

// Normalize makes two environments that mean the same thing compare equal: the
// device pixel ratio is rounded to two decimals (a reported 1.9999999 and 2.0
// are the same display) and text fields are trimmed and lowercased where case
// carries no meaning.
func (e Environment) Normalize() Environment {
	e.BrowserBuild = strings.TrimSpace(e.BrowserBuild)
	e.Locale = strings.ToLower(strings.TrimSpace(e.Locale))
	e.OS = strings.ToLower(strings.TrimSpace(e.OS))
	e.DevicePixelRatio = math.Round(e.DevicePixelRatio*100) / 100
	return e
}

func (e Environment) Validate() error {
	normalized := e.Normalize()
	var problems []string
	if normalized.BrowserBuild == "" {
		problems = append(problems, "browser_build is empty")
	}
	if normalized.ViewportWidth <= 0 || normalized.ViewportHeight <= 0 {
		problems = append(problems, "viewport must be positive")
	}
	if normalized.DevicePixelRatio <= 0 {
		problems = append(problems, "device_pixel_ratio must be positive")
	}
	if normalized.Locale == "" {
		problems = append(problems, "locale is empty")
	}
	if normalized.OS == "" {
		problems = append(problems, "os is empty")
	}
	if len(problems) > 0 {
		return fmt.Errorf("incomplete environment fingerprint: %s", strings.Join(problems, "; "))
	}
	return nil
}

// canonical is the exact string the fingerprint hashes. Field names are in it
// so adding a field later changes every fingerprint, which is correct: an old
// baseline was captured without that dimension pinned.
func (e Environment) canonical() string {
	n := e.Normalize()
	return strings.Join([]string{
		"browser_build=" + n.BrowserBuild,
		"viewport=" + strconv.Itoa(n.ViewportWidth) + "x" + strconv.Itoa(n.ViewportHeight),
		"dpr=" + strconv.FormatFloat(n.DevicePixelRatio, 'f', 2, 64),
		"locale=" + n.Locale,
		"os=" + n.OS,
	}, "\n")
}

// Fingerprint identifies this capture condition.
func (e Environment) Fingerprint() string {
	sum := sha256.Sum256([]byte(e.canonical()))
	return hex.EncodeToString(sum[:])
}

// Differences names the fields that moved, so a mismatch report says "the
// device pixel ratio changed from 1 to 2" rather than "different fingerprint".
func (e Environment) Differences(other Environment) []string {
	a, b := e.Normalize(), other.Normalize()
	var out []string
	if a.BrowserBuild != b.BrowserBuild {
		out = append(out, fmt.Sprintf("browser_build %s -> %s", a.BrowserBuild, b.BrowserBuild))
	}
	if a.ViewportWidth != b.ViewportWidth || a.ViewportHeight != b.ViewportHeight {
		out = append(out, fmt.Sprintf("viewport %dx%d -> %dx%d", a.ViewportWidth, a.ViewportHeight, b.ViewportWidth, b.ViewportHeight))
	}
	if a.DevicePixelRatio != b.DevicePixelRatio {
		out = append(out, fmt.Sprintf("device_pixel_ratio %s -> %s",
			strconv.FormatFloat(a.DevicePixelRatio, 'f', -1, 64),
			strconv.FormatFloat(b.DevicePixelRatio, 'f', -1, 64)))
	}
	if a.Locale != b.Locale {
		out = append(out, fmt.Sprintf("locale %s -> %s", a.Locale, b.Locale))
	}
	if a.OS != b.OS {
		out = append(out, fmt.Sprintf("os %s -> %s", a.OS, b.OS))
	}
	return out
}

var recipeDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Key identifies one baseline: which recipe, which step of it, under which
// environment.
type Key struct {
	// RecipeDigest is recipe.Digest output — the content digest of the exact
	// immutable recipe version, so editing a recipe orphans its baselines
	// rather than silently comparing against the previous behaviour.
	RecipeDigest string      `json:"recipe_digest"`
	StepIndex    int         `json:"step_index"`
	Environment  Environment `json:"environment"`
}

func (k Key) Validate() error {
	if !recipeDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(k.RecipeDigest))) {
		return errors.New("recipe_digest must be the 64-character hex content digest of a pinned recipe version")
	}
	if k.StepIndex < 0 {
		return errors.New("step_index must not be negative")
	}
	return k.Environment.Validate()
}

func (k Key) digest() string { return strings.ToLower(strings.TrimSpace(k.RecipeDigest)) }

// Scope is the (recipe, step) pair without the environment — the set inside
// which an environment mismatch is looked for.
func (k Key) Scope() string {
	return k.digest() + "/step-" + strconv.Itoa(k.StepIndex)
}

// ID is the stable identifier of one baseline, safe to report and to use as a
// directory name.
func (k Key) ID() string {
	return k.digest()[:16] + "-" + strconv.Itoa(k.StepIndex) + "-" + k.Environment.Fingerprint()[:16]
}
