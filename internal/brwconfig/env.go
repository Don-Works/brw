package brwconfig

import "maps"

var flagEnv = map[string]string{
	"allowed-domains":             "BRW_ALLOWED_DOMAINS",
	"artifact-dir":                "BRW_ARTIFACT_DIR",
	"artifact-encrypt":            "BRW_ARTIFACT_ENCRYPT",
	"artifact-failure-bundle-ttl": "BRW_ARTIFACT_FAILURE_BUNDLE_TTL",
	"artifact-failure-bundles":    "BRW_ARTIFACT_FAILURE_BUNDLES",
	"artifact-key-file":           "BRW_ARTIFACT_KEY_FILE",
	"artifact-max-mb":             "BRW_ARTIFACT_MAX_MB",
	"artifact-total-mb":           "BRW_ARTIFACT_TOTAL_MB",
	"artifact-ttl":                "BRW_ARTIFACT_TTL",
	"baseline-root":               "BRW_BASELINE_ROOT",
	"blocked-domains":             "BRW_BLOCKED_DOMAINS",
	"bridge-addr":                 "BRW_BRIDGE_ADDR",
	"bridge-follow-focus":         "BRW_BRIDGE_FOLLOW_FOCUS",
	"bridge-max-inflight":         "BRW_BRIDGE_MAX_INFLIGHT",
	"bridge-raise-window":         "BRW_BRIDGE_RAISE_WINDOW",
	"bridge-tab-group":            "BRW_BRIDGE_TAB_GROUP",
	"ca-cert":                     "BRW_CA_CERT",
	"chrome-opt-in":               "BRW_CHROME_OPT_IN",
	"chrome-opt-in-browser":       "BRW_CHROME_OPT_IN_BROWSER",
	"chrome-opt-in-user-data-dir": "BRW_CHROME_OPT_IN_USER_DATA_DIR",
	"chrome-path":                 "BRW_CHROME_PATH",

	"config":                     "BRW_CONFIG",
	"confirm-actions":            "BRW_CONFIRM_ACTIONS",
	"content-nav-guard":          "BRW_CONTENT_NAV_GUARD",
	"enable-webmcp":              "BRW_ENABLE_WEBMCP",
	"pacing":                     "BRW_PACING",
	"page-watch-root":            "BRW_PAGE_WATCH_ROOT",
	"headless":                   "BRW_HEADLESS",
	"http":                       "BRW_HTTP_ADDR",
	"idle-exit":                  "BRW_IDLE_EXIT",
	"exit-on-upgrade":            "BRW_EXIT_ON_UPGRADE",
	"ignore-https-errors":        "BRW_IGNORE_HTTPS_ERRORS",
	"mcp-idle-exit":              "BRW_MCP_IDLE_EXIT",
	"mcp-tools":                  "BRW_MCP_TOOLS",
	"plugin-dir":                 "BRW_PLUGIN_DIR",
	"profile":                    "BRW_PROFILE",
	"profile-directory":          "BRW_PROFILE_DIRECTORY",
	"profile-policy":             "BRW_PROFILE_POLICY",
	"proxy-bypass-list":          "BRW_PROXY_BYPASS_LIST",
	"proxy-server":               "BRW_PROXY_SERVER",
	"recipe-provider-token-file": "BRW_RECIPE_PROVIDER_TOKEN_FILE",
	"http-token-file":            "BRW_HTTP_TOKEN_FILE",
	"upstream-token-file":        "BRW_UPSTREAM_TOKEN_FILE",
	"recipe-provider-url":        "BRW_RECIPE_PROVIDER_URL",
	"recipe-root":                "BRW_RECIPE_ROOT",
	"remote":                     "BRW_REMOTE_URL",
	"site-consent":               "BRW_SITE_CONSENT",
	"site-consent-config":        "BRW_SITE_CONSENT_CONFIG",
	"site-consent-prompt":        "BRW_SITE_CONSENT_PROMPT",
	"state-key-file":             "BRW_STATE_KEY_FILE",
	"state-root":                 "BRW_STATE_ROOT",
	"unsafe-real-profile":        "BRW_UNSAFE_REAL_PROFILE",
	"upstream-http":              "BRW_UPSTREAM_HTTP",
	"usage-log":                  "BRW_USAGE_LOG",
	"usage-log-backups":          "BRW_USAGE_LOG_BACKUPS",
	"usage-log-max-mb":           "BRW_USAGE_LOG_MAX_MB",
	"user-data-dir":              "BRW_USER_DATA_DIR",
	"workspace":                  "BRW_WORKSPACE",
}

// EnvFor names the environment variable brwd reads for a flag, or "" when it reads none.
func EnvFor(flag string) string { return flagEnv[flag] }

// EnvTable exposes the mapping for the drift test, which compares it against what cmd/brwd actually reads.
func EnvTable() map[string]string {
	return maps.Clone(flagEnv)
}

// NotConfigurable exposes the deny-list and its reasons, for the drift test and for an error message that has to say why.
func NotConfigurable() map[string]string {
	return maps.Clone(notConfigurable)
}
