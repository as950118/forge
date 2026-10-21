package mcp

import "net/url"

// CheckURLSecrets is the exported form of the URL value-pattern check so that
// other registries (custom tools) apply the same secret heuristics.
func CheckURLSecrets(u *url.URL) error { return checkURLSecrets(u) }
