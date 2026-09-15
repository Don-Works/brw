package profileroster

import (
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

const defaultHTTPPort = 17310

func nextHTTPPort(policy profilepolicy.Policy) int {
	high := defaultHTTPPort
	for _, p := range policy.Profiles {
		for _, addr := range []string{p.BridgeHTTPAddr, p.BridgeWSAddr} {
			if n := portOf(addr); n > high {
				high = n
			}
		}
	}
	n := high + 2
	if n%2 == 0 {
		n++
	}
	return n
}

func portOf(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		n, err := strconv.Atoi(strings.TrimSuffix(addr[i+1:], "/"))
		if err == nil {
			return n
		}
	}
	return 0
}

func hostPort(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}

func isDailyChromeDir(userDataDir string) bool {
	d := strings.ToLower(userDataDir)
	return strings.Contains(d, "google/chrome") || strings.Contains(d, "google\\chrome")
}
