package bridge

import "testing"

func TestParseKeyValue(t *testing.T) {
	cases := []struct{ arg, key, value string }{
		{"email joey@rmrf.uk", "email", "joey@rmrf.uk"},
		{"full_name Joey Innes", "full_name", "Joey Innes"},
		{"  padded   value here ", "padded", "value here"},
		{"keyonly", "keyonly", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		key, value := parseKeyValue(c.arg)
		if key != c.key || value != c.value {
			t.Errorf("parseKeyValue(%q) = (%q, %q), want (%q, %q)", c.arg, key, value, c.key, c.value)
		}
	}
}

func TestJoinPreamble(t *testing.T) {
	if got := joinPreamble("", ""); got != "" {
		t.Errorf("joinPreamble(empty) = %q, want empty", got)
	}
	if got := joinPreamble("facts", ""); got != "facts" {
		t.Errorf("joinPreamble(one) = %q, want %q", got, "facts")
	}
	if got := joinPreamble("facts", "secrets"); got != "facts\n\nsecrets" {
		t.Errorf("joinPreamble(two) = %q, want %q", got, "facts\n\nsecrets")
	}
}
