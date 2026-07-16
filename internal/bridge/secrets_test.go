package bridge

import "testing"

func TestParseSecretSet(t *testing.T) {
	cases := []struct {
		arg, name, value string
	}{
		{"set GITHUB_TOKEN ghp_abc", "GITHUB_TOKEN", "ghp_abc"},
		{"SET GITHUB_TOKEN ghp_abc", "GITHUB_TOKEN", "ghp_abc"}, // case-insensitive token
		{"set API_KEY value with spaces", "API_KEY", "value with spaces"},
		{"  set   PADDED   trailing  ", "PADDED", "trailing"},
		{"set ONLYNAME", "ONLYNAME", ""}, // missing value
		{"set", "", ""},                  // nothing
		{"set  ", "", ""},                // whitespace only
	}
	for _, c := range cases {
		name, value := parseSecretSet(c.arg)
		if name != c.name || value != c.value {
			t.Errorf("parseSecretSet(%q) = (%q, %q), want (%q, %q)", c.arg, name, value, c.name, c.value)
		}
	}
}
