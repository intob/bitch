package bridge

import "testing"

func TestNormalizeModel(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"opus", "claude-opus-4-8", true},
		{"Sonnet", "claude-sonnet-5", true},
		{"  haiku ", "claude-haiku-4-5-20251001", true},
		{"fable", "claude-fable-5", true},
		{"claude-opus-4-8", "claude-opus-4-8", true}, // full id passes through
		{"claude-future-9", "claude-future-9", true}, // unknown claude-… still accepted
		{"gpt-4", "", false},                         // other vendors rejected
		{"", "", false},
		{"garbage", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeModel(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("normalizeModel(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
