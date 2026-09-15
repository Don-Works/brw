package browser

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// LocaleOptions overrides the locale and timezone a tab reports to the page:
// navigator.language, Intl formatting, and Date timezone calculations.
type LocaleOptions struct {
	Locale   string `json:"locale,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Clear    bool   `json:"clear,omitempty"`
	TabID    string `json:"tab_id,omitempty"`
}

// LocaleConfig is the resolved override. LocaleICU is what CDP consumes
// (underscores). Locale is the BCP-47 form the caller passed, so a result can
// be compared with navigator.language without a surprise conversion.
type LocaleConfig struct {
	Locale           string `json:"locale,omitempty"`
	LocaleICU        string `json:"locale_icu,omitempty"`
	Timezone         string `json:"timezone,omitempty"`
	ReportedLanguage string `json:"reported_language,omitempty"`
	ReportedTimezone string `json:"reported_timezone,omitempty"`
}

// NormalizeLocale validates a locale/timezone request. At least one of locale,
// timezone, or clear is required. CDP wants an ICU locale (en_GB); callers
// usually write BCP-47 (en-GB). Both are accepted and the ICU form is derived.
func NormalizeLocale(opts LocaleOptions) (LocaleConfig, bool, error) {
	if opts.Clear {
		return LocaleConfig{}, true, nil
	}
	cfg := LocaleConfig{
		Locale:   strings.TrimSpace(opts.Locale),
		Timezone: strings.TrimSpace(opts.Timezone),
	}
	if cfg.Locale == "" && cfg.Timezone == "" {
		return LocaleConfig{}, false, errors.New("locale override needs locale, timezone, or clear:true")
	}
	if strings.ContainsAny(cfg.Locale, "\r\n") || strings.ContainsAny(cfg.Timezone, "\r\n") {
		return LocaleConfig{}, false, errors.New("locale and timezone must not contain newlines")
	}
	if cfg.Locale != "" {
		icu, err := localeToICU(cfg.Locale)
		if err != nil {
			return LocaleConfig{}, false, err
		}
		cfg.LocaleICU = icu
	}
	if cfg.Timezone != "" {
		if err := validTimezone(cfg.Timezone); err != nil {
			return LocaleConfig{}, false, err
		}
	}
	return cfg, false, nil
}

func localeToICU(locale string) (string, error) {
	trimmed := strings.TrimSpace(locale)
	if trimmed == "" {
		return "", errors.New("locale is empty")
	}
	normalized := strings.ReplaceAll(trimmed, "-", "_")
	if len(normalized) < 2 || len(normalized) > 32 {
		return "", fmt.Errorf("locale %q is not a usable BCP-47 or ICU locale", locale)
	}
	for _, r := range normalized {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_':
		default:
			return "", fmt.Errorf("locale %q contains %q, which is not a locale character", locale, r)
		}
	}
	return normalized, nil
}

func validTimezone(tz string) error {
	if strings.EqualFold(tz, "UTC") || strings.EqualFold(tz, "GMT") {
		return nil
	}
	if !strings.Contains(tz, "/") {
		return fmt.Errorf("timezone %q is not an IANA identifier (use Europe/London or UTC)", tz)
	}
	for _, r := range tz {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '/', r == '_', r == '+', r == '-':
		default:
			return fmt.Errorf("timezone %q contains %q, which is not an IANA timezone character", tz, r)
		}
	}
	return nil
}
