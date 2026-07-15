// Package tg holds the Telegram output plumbing — the UX make-or-break (§7).
// This file is pure rendering: converting agent Markdown to Telegram's HTML
// parse mode and chunking under the 4096-char limit without ever splitting a
// code fence. sender.go holds the side-effecting Bot operations.
package tg

import (
	"regexp"
	"strings"
)

// MaxMessage is Telegram's hard message length. We chunk under a slightly lower
// budget to leave room for the closing tags we may add when splitting code.
const MaxMessage = 4096

const chunkBudget = 3900

var (
	fenceRe = regexp.MustCompile("(?m)^```")
	boldRe  = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

// Chunk renders Markdown to Telegram HTML and splits it into messages that each
// fit under the limit. Code fences are rendered as complete <pre> blocks; an
// oversized fence is split across several complete <pre> blocks, never left
// half-open (§7).
func Chunk(md string) []string {
	var chunks []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	// add appends a self-contained rendered block, starting a new chunk if it
	// would overflow the current one.
	add := func(block string) {
		if cur.Len() > 0 && cur.Len()+len(block)+1 > chunkBudget {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(block)
	}

	for _, tok := range tokenize(md) {
		if tok.code {
			for _, pre := range renderCode(tok.text) {
				add(pre)
			}
		} else {
			for _, para := range renderText(tok.text) {
				add(para)
			}
		}
	}
	flush()
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

type token struct {
	code bool
	text string
}

// tokenize splits Markdown into alternating text and fenced-code tokens.
func tokenize(md string) []token {
	lines := strings.Split(md, "\n")
	var toks []token
	var buf []string
	inCode := false
	flush := func(code bool) {
		if len(buf) == 0 {
			return
		}
		toks = append(toks, token{code: code, text: strings.Join(buf, "\n")})
		buf = nil
	}
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			if inCode {
				flush(true)
				inCode = false
			} else {
				flush(false)
				inCode = true
			}
			continue
		}
		buf = append(buf, ln)
	}
	// A dangling unterminated fence is still rendered as code.
	flush(inCode)
	return toks
}

// renderCode wraps code content in <pre>, splitting on line boundaries if it
// exceeds the budget so each emitted block is a complete <pre>…</pre>.
func renderCode(code string) []string {
	code = strings.Trim(code, "\n")
	if code == "" {
		return nil
	}
	lines := strings.Split(code, "\n")
	var out []string
	var buf strings.Builder
	for _, ln := range lines {
		esc := escapeHTML(ln)
		if buf.Len() > 0 && buf.Len()+len(esc)+1 > chunkBudget-len("<pre></pre>") {
			out = append(out, "<pre>"+buf.String()+"</pre>")
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		// A single line longer than the budget is hard-split.
		for len(esc) > chunkBudget-len("<pre></pre>") {
			cut := chunkBudget - len("<pre></pre>")
			out = append(out, "<pre>"+esc[:cut]+"</pre>")
			esc = esc[cut:]
		}
		buf.WriteString(esc)
	}
	if buf.Len() > 0 {
		out = append(out, "<pre>"+buf.String()+"</pre>")
	}
	return out
}

// renderText renders a non-code token to HTML paragraphs, splitting on blank
// lines and, if needed, on single newlines to stay under budget.
func renderText(text string) []string {
	rendered := renderInline(text)
	if len(rendered) <= chunkBudget {
		return []string{rendered}
	}
	// Too big: split the original on lines and render piecewise.
	var out []string
	var buf strings.Builder
	for ln := range strings.SplitSeq(text, "\n") {
		r := renderInline(ln)
		if buf.Len() > 0 && buf.Len()+len(r)+1 > chunkBudget {
			out = append(out, buf.String())
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		for len(r) > chunkBudget {
			out = append(out, r[:chunkBudget])
			r = r[chunkBudget:]
		}
		buf.WriteString(r)
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// renderInline converts inline Markdown (`code`, **bold**) in a text run to
// Telegram HTML, escaping everything else. Inline code spans are protected from
// bold conversion.
func renderInline(s string) string {
	var b strings.Builder
	parts := strings.Split(s, "`")
	for i, part := range parts {
		if i%2 == 1 { // inside a `code` span
			b.WriteString("<code>")
			b.WriteString(escapeHTML(part))
			b.WriteString("</code>")
			continue
		}
		// escape, then apply bold on the escaped text (** survives escaping)
		esc := escapeHTML(part)
		esc = boldRe.ReplaceAllString(esc, "<b>$1</b>")
		b.WriteString(esc)
	}
	return b.String()
}

// escapeHTML escapes the three characters Telegram's HTML parse mode reserves.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
