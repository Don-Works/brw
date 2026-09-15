package profileroster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/discovery"
	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/profilepolicy"
)

// CopyDomain copies (or moves) cookies for one registrable domain from src to dest
// via live CDP. Values stay in this process; the result has none.
func CopyDomain(ctx context.Context, policy profilepolicy.Policy, from, to, domain, mode string) (CopyResult, error) {
	domain = RegistrableDomain(domain)
	if domain == "" {
		return CopyResult{}, fmt.Errorf("domain is required")
	}
	src, err := policy.Find(from)
	if err != nil {
		return CopyResult{}, err
	}
	dst, err := policy.Find(to)
	if err != nil {
		return CopyResult{}, err
	}
	if from == to {
		return CopyResult{}, fmt.Errorf("source and destination are the same profile")
	}
	if !dst.DirectCDPAllowed {
		return CopyResult{}, fmt.Errorf("refusing to write cookies into %q: destination is not a brw-owned direct-CDP profile", to)
	}
	if isDailyChromeDir(dst.UserDataDir) {
		return CopyResult{}, fmt.Errorf("refusing to write into the daily Chrome user-data-dir")
	}
	if !src.DirectCDPAllowed {
		return CopyResult{}, fmt.Errorf("copying from %q needs a live direct-CDP source (extension-bridge profiles cannot list cookies)", from)
	}

	srcCtrl, err := httpclient.New(discovery.HTTPURL(src), 20*time.Second)
	if err != nil {
		return CopyResult{}, err
	}
	dstCtrl, err := httpclient.New(discovery.HTTPURL(dst), 20*time.Second)
	if err != nil {
		return CopyResult{}, err
	}

	listed, err := srcCtrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: "https://" + domain})
	if err != nil {
		return CopyResult{}, fmt.Errorf("list cookies on %s: %w", from, err)
	}
	copied := 0
	for _, c := range listed.Cookies {
		if RegistrableDomain(c.Domain) != domain {
			continue
		}
		_, err := dstCtrl.Cookies(ctx, browser.CookieParams{
			Action:   browser.CookieActionSet,
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
			SameSite: c.SameSite,
			Expires:  c.Expires,
			URL:      "https://" + domain,
		})
		if err != nil {
			return CopyResult{}, fmt.Errorf("set cookie %q on %s: %w", c.Name, to, err)
		}
		copied++
	}

	if strings.EqualFold(mode, "move") {
		for _, c := range listed.Cookies {
			if RegistrableDomain(c.Domain) != domain {
				continue
			}
			_, _ = srcCtrl.Cookies(ctx, browser.CookieParams{
				Action: browser.CookieActionDelete,
				Name:   c.Name,
				Domain: c.Domain,
				Path:   c.Path,
				URL:    "https://" + domain,
			})
		}
	} else {
		mode = "copy"
	}

	health := HealthCopiedUnverified
	verify, verr := dstCtrl.Cookies(ctx, browser.CookieParams{Action: browser.CookieActionList, URL: "https://" + domain})
	if verr == nil && len(verify.Cookies) > 0 {
		health = HealthSignedIn
	}

	return CopyResult{
		BatchID: newBatchID(),
		From:    from,
		To:      to,
		Domain:  domain,
		Mode:    mode,
		Copied:  copied,
		Health:  health,
	}, nil
}

func newBatchID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
