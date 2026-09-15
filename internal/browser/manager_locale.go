package browser

import (
	"context"
	"strings"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// SetLocale overrides the locale and/or timezone the tab reports to the page.
// After applying, it reads navigator.language and the resolved Intl timezone
// back, so the caller can see what the page actually got rather than what was
// requested.
func (m *Manager) SetLocale(ctx context.Context, opts LocaleOptions) (EnvironmentResult, error) {
	cfg, clear, err := NormalizeLocale(opts)
	if err != nil {
		return EnvironmentResult{}, err
	}
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, strings.TrimSpace(opts.TabID))
	if err != nil {
		return EnvironmentResult{}, err
	}
	defer cancel()

	if err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(runCtx context.Context) error {
		if clear {
			if err := emulation.SetLocaleOverride().Do(runCtx); err != nil {
				return err
			}
			return emulation.SetTimezoneOverride("").Do(runCtx)
		}
		if cfg.LocaleICU != "" {
			if err := emulation.SetLocaleOverride().WithLocale(cfg.LocaleICU).Do(runCtx); err != nil {
				return err
			}
		}
		if cfg.Timezone != "" {
			if err := emulation.SetTimezoneOverride(cfg.Timezone).Do(runCtx); err != nil {
				return err
			}
		}
		return nil
	})); err != nil {
		return EnvironmentResult{}, err
	}
	m.invalidateState(tabID)

	if !clear {
		cfg.ReportedLanguage = evaluatePageString(tabCtx, `navigator.language || ""`)
		cfg.ReportedTimezone = evaluatePageString(tabCtx, `(Intl.DateTimeFormat().resolvedOptions().timeZone) || ""`)
	}
	message := "locale override applied to this tab; a page that reads the locale once at startup needs a reload"
	if clear {
		message = "cleared locale and timezone overrides; the tab is back on the host's own locale"
	}
	return EnvironmentResult{OK: true, TabID: tabID, Cleared: clear, Locale: &cfg, Message: message}, nil
}

func evaluatePageString(tabCtx context.Context, expr string) string {
	var out string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(expr, &out)); err != nil {
		return ""
	}
	return out
}
