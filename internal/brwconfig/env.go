package brwconfig

// flagEnv names the environment variable brwd reads for each flag.
//
// It has to be complete and it has to be right, because it is what makes the
// environment beat the config file: brwd reads its environment as each flag's
// DEFAULT, so by the time a config file is applied an env-configured flag looks
// exactly like an untouched one. A flag missing from this table would have its
// environment value silently overwritten by brw.json — the one inversion of
// precedence that would be invisible to whoever set the variable.
//
// It is spelled out rather than derived from the flag name because the two do
// not match: --http reads BRW_HTTP_ADDR and --remote reads BRW_REMOTE_URL.
// TestEveryEnvironmentVariableBrwdReadsIsInTheConfigTable reads cmd/brwd/main.go
// and fails on any drift in either direction.
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
	// Listed because brwd does read it, even though notConfigurable stops a
	// file from setting it: the table's job is to describe what brwd reads, and
	// an entry missing from it is what the drift test is looking for.
	"config":                     "BRW_CONFIG",
	"confirm-actions":            "BRW_CONFIRM_ACTIONS",
	"content-nav-guard":          "BRW_CONTENT_NAV_GUARD",
	"enable-webmcp":              "BRW_ENABLE_WEBMCP",
	"headless":                   "BRW_HEADLESS",
	"http":                       "BRW_HTTP_ADDR",
	"idle-exit":                  "BRW_IDLE_EXIT",
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

// EnvFor names the environment variable brwd reads for a flag, or "" when it
// reads none. A flag with no environment variable is decided by the command
// line, then the config file, then its built-in default.
func EnvFor(flag string) string { return flagEnv[flag] }

// EnvTable exposes the mapping for the drift test, which compares it against
// what cmd/brwd actually reads.
func EnvTable() map[string]string {
	out := make(map[string]string, len(flagEnv))
	for name, env := range flagEnv {
		out[name] = env
	}
	return out
}

// NotConfigurable exposes the deny-list and its reasons, for the drift test and
// for an error message that has to say why.
func NotConfigurable() map[string]string {
	out := make(map[string]string, len(notConfigurable))
	for name, reason := range notConfigurable {
		out[name] = reason
	}
	return out
}
