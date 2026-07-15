package tg

import (
	"strings"
	"testing"
)

func TestChunkNeverSplitsFence(t *testing.T) {
	// A long code block that must span multiple chunks — each chunk must contain
	// balanced <pre>…</pre>, never a half-open fence (§7).
	var sb strings.Builder
	sb.WriteString("Here is some output:\n```\n")
	for i := 0; i < 400; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", 40))
		sb.WriteByte('\n')
	}
	sb.WriteString("```\ndone")

	chunks := Chunk(sb.String())
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > MaxMessage {
			t.Errorf("chunk %d exceeds Telegram limit: %d", i, len(c))
		}
		if strings.Count(c, "<pre>") != strings.Count(c, "</pre>") {
			t.Errorf("chunk %d has unbalanced <pre> tags", i)
		}
	}
}

func TestRenderInline(t *testing.T) {
	cases := map[string]string{
		"plain text":        "plain text",
		"a `code` span":     "a <code>code</code> span",
		"**bold** word":     "<b>bold</b> word",
		"5 < 6 & 7 > 3":     "5 &lt; 6 &amp; 7 &gt; 3",
		"tag <script> here": "tag &lt;script&gt; here",
		"`<b>` in code":     "<code>&lt;b&gt;</code> in code",
	}
	for in, want := range cases {
		if got := renderInline(in); got != want {
			t.Errorf("renderInline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunkShortPassesThrough(t *testing.T) {
	chunks := Chunk("hello **world**")
	if len(chunks) != 1 || chunks[0] != "hello <b>world</b>" {
		t.Errorf("unexpected chunks: %#v", chunks)
	}
}
