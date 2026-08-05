package config

import (
	"net/url"
	"strings"
)

// SupportsServerWebSearch reports whether the provider kind has a wire format
// for a provider-executed web search tool. OpenAI Chat Completions is excluded:
// DeepSeek's documented chat-completions tool contract only supports functions.
func SupportsServerWebSearch(e *ProviderEntry) bool {
	if e == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Kind)) {
	case "anthropic", "responses":
		return true
	default:
		return false
	}
}

// IsOfficialDeepSeekWebSearchEndpoint matches the exact protocol base URLs
// documented by DeepSeek. Host-only matching is intentionally insufficient:
// Anthropic requires the /anthropic prefix, while Responses uses the origin.
func IsOfficialDeepSeekWebSearchEndpoint(e *ProviderEntry) bool {
	if !SupportsServerWebSearch(e) {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(e.BaseURL))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "api.deepseek.com") ||
		u.Port() != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch strings.ToLower(strings.TrimSpace(e.Kind)) {
	case "responses":
		return path == ""
	case "anthropic":
		return path == "/anthropic"
	default:
		return false
	}
}

// EffectiveWebSearch resolves the persisted tri-state. Fork default: server-side
// web search is OFF unless explicitly enabled with web_search = true — the fork
// uses the plain web_fetch/crawl4ai path instead (see docs/UPGRADE-CONFLICTS-1.19.7.md).
// Compatible third-party endpoints stay opt-in too. An explicit true always wins.
func EffectiveWebSearch(e *ProviderEntry) bool {
	if !SupportsServerWebSearch(e) {
		return false
	}
	if e.WebSearch != nil {
		return *e.WebSearch
	}
	return false
}
