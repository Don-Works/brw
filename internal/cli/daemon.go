package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/discovery"
)

const daemonProbeTimeout = 2 * time.Second

func resolveBaseURL(opts *options) (string, error) {
	if url := strings.TrimSpace(opts.daemon); url != "" {
		return url, nil
	}
	if url := strings.TrimSpace(os.Getenv("BRW_URL")); url != "" {
		return url, nil
	}

	candidates, err := discovery.Candidates(opts.policyPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v (pass --daemon or set BRW_URL)", errNoDaemon, err)
	}
	if name := strings.TrimSpace(opts.profile); name != "" {
		for _, profile := range candidates {
			if profile.Name == name {
				return discovery.HTTPURL(profile), nil
			}
		}
		return "", fmt.Errorf("%w: the profile policy has no extension-bridge profile named %q", errNoDaemon, name)
	}

	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("%w: the profile policy configures no extension-bridge profile", errNoDaemon)
	case 1:

		return discovery.HTTPURL(candidates[0]), nil
	}

	var names []string
	for _, profile := range candidates {
		names = append(names, profile.Name)
		if record := discovery.Probe(profile, daemonProbeTimeout); record.Reachable {
			return record.HTTPAddr, nil
		}
	}
	return "", fmt.Errorf("%w: none of the configured daemons answered /health (%s); name one with --profile",
		errNoDaemon, strings.Join(names, ", "))
}
