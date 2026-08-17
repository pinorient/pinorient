package api

import "testing"

func TestNormalizeOrigin(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"https://example.com":         "example.com",
		"http://example.com/path/x":   "example.com",
		"https://example.com:8443":    "example.com",
		"example.com":                 "example.com",
		"https://sub.example.com/a?b": "sub.example.com",
	}
	for in, want := range cases {
		if got := NormalizeOrigin(in); got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvDomainChecker(t *testing.T) {
	// Empty allowlist permits everything, including empty origins.
	open := EnvDomainChecker(nil)
	for _, origin := range []string{"", "https://anything.com"} {
		if !open(origin) {
			t.Errorf("open checker denied %q", origin)
		}
	}

	checker := EnvDomainChecker([]string{"example.com", "*.mysite.com"})
	allowed := []string{
		"https://example.com",
		"http://example.com/some/path",
		"https://app.mysite.com",
		"https://deep.sub.mysite.com",
	}
	for _, origin := range allowed {
		if !checker(origin) {
			t.Errorf("checker denied allowed origin %q", origin)
		}
	}

	denied := []string{
		"",
		"https://other.com",
		"https://mysite.com", // wildcard requires a subdomain
		"https://notmysite.com",
	}
	for _, origin := range denied {
		if checker(origin) {
			t.Errorf("checker allowed denied origin %q", origin)
		}
	}
}
