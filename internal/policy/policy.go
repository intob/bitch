// Package policy is the permission gate — the primary security boundary under
// the full-root posture (DESIGN.md §4.6). Given a tool call and the session's
// tier, it returns Allow / Deny / Ask per the tier table in §4.3. The bridge
// resolves Ask into a Telegram approval; everything here is pure and testable.
package policy

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Decision is the outcome of evaluating a tool call.
type Decision int

const (
	Allow Decision = iota // auto-permit, no human
	Deny                  // auto-refuse, no human
	Ask                   // block and ask the human over Telegram (§4.3)
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	default:
		return "ask"
	}
}

// Verdict carries the decision plus human-facing context for the approval card.
type Verdict struct {
	Decision Decision
	Reason   string // short machine reason, surfaced to Claude Code on deny
	Action   string // one-line human summary, e.g. "Bash: git push origin main"
	Detail   string // longer preview (command, path, diff sizes) for the approval
}

// Request is the normalized tool call handed to Evaluate.
type Request struct {
	Tool    string         // tool name, e.g. "Bash", "Edit", "Read"
	Input   map[string]any // tool_input from the hook payload
	WorkDir string         // session working directory (jail anchor)
	Tier    int            // current permission tier 0..3
}

// WithinRoots reports whether an absolute, cleaned path is inside the jail. The
// bridge passes config.WithinRoots so policy stays dependency-free.
type WithinRoots func(absPath string) bool

// Evaluate applies the tier table (§4.3) to a tool call.
func Evaluate(r Request, within WithinRoots) Verdict {
	tool := r.Tool
	switch {
	case isReadTool(tool):
		// Read (fs, web) is auto at every tier. Still enforce the jail for
		// file reads so credential files outside the workspace stay unreadable.
		if p, ok := filePathArg(r.Input); ok {
			abs := resolve(r.WorkDir, p)
			if !within(abs) {
				return Verdict{Deny, "path outside workspace jail",
					"Read outside jail: " + p, abs}
			}
		}
		return Verdict{Allow, "read auto", "", ""}

	case isEditTool(tool):
		p, _ := filePathArg(r.Input)
		abs := resolve(r.WorkDir, p)
		if !within(abs) {
			// Path traversal outside the jail is denied regardless of tier.
			return Verdict{Deny, "path outside workspace jail",
				"Edit outside jail: " + p, abs}
		}
		action := tool + ": " + p
		switch r.Tier {
		case 0:
			return Verdict{Ask, "edit needs approval at T0", action, editDetail(r.Input)}
		default: // T1, T2, T3
			return Verdict{Allow, "edit auto in workspace", action, ""}
		}

	case isBashTool(tool):
		cmd, _ := r.Input["command"].(string)
		cmd = strings.TrimSpace(cmd)
		class := ClassifyBash(cmd)
		action := "Bash: " + oneLine(cmd)
		switch class {
		case bashSafe:
			switch r.Tier {
			case 0:
				return Verdict{Ask, "bash needs approval at T0", action, cmd}
			default:
				return Verdict{Allow, "safe bash allowlisted", action, ""}
			}
		default: // risky or unknown → the destructive/network column
			return riskyVerdict(r.Tier, action, cmd)
		}

	default:
		// Unknown / MCP / Task tools: fail closed. Auto only at full autonomy.
		action := tool + ": " + oneLine(compactInput(r.Input))
		return riskyVerdict(r.Tier, action, compactInput(r.Input))
	}
}

// riskyVerdict maps the "destructive / git push / network writes" column of the
// tier table: T0 deny, T1/T2 ask, T3 auto.
func riskyVerdict(tier int, action, detail string) Verdict {
	switch tier {
	case 0:
		return Verdict{Deny, "risky op denied at T0", action, detail}
	case 3:
		return Verdict{Allow, "full autonomy", action, ""}
	default:
		return Verdict{Ask, "risky op needs approval", action, detail}
	}
}

// --- tool classification -------------------------------------------------

func isReadTool(t string) bool {
	switch t {
	case "Read", "Glob", "Grep", "LS", "NotebookRead", "WebFetch", "WebSearch",
		"TodoWrite", "BashOutput",
		// Killing one of the agent's own background shells is inert.
		"KillBash", "KillShell":
		return true
	}
	return false
}

func isEditTool(t string) bool {
	switch t {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "Update":
		return true
	}
	return false
}

func isBashTool(t string) bool {
	return t == "Bash"
}

// --- bash classification -------------------------------------------------

type bashClass int

const (
	bashUnknown bashClass = iota
	bashSafe
	bashRisky
)

// denyPatterns match destructive ops, network writes, privilege escalation, and
// credential reads. Below T3 these always ask or deny (§4.3). They scan the
// WHOLE command string, so they also catch badness embedded in the arguments of
// an otherwise-safe leading command. Fail-closed: when in doubt, err toward
// flagging. The agent runs as root, so /root/ variants matter as much as ~.
var denyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rf]`), // rm -rf / rm -fr / rm -r -f
	regexp.MustCompile(`\brm\s+-[a-zA-Z]*r`),                    // rm -r
	regexp.MustCompile(`\b(mkfs|fdisk|parted|dd)\b`),            // disk ops
	regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff|init)\b`),
	regexp.MustCompile(`\b(sudo|su|doas)\b`),                                       // privilege escalation
	regexp.MustCompile(`\b(curl|wget|fetch)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`), // curl … | sh
	regexp.MustCompile(`\bgit\s+push\b`),                                           // network write
	regexp.MustCompile(`\bgit\s+remote\s+(add|set-url)\b`),                         // repoint remotes
	regexp.MustCompile(`\b(scp|rsync|sftp|ssh)\b`),                                 // remote transfer/exec
	regexp.MustCompile(`\bnpm\s+publish\b`),
	regexp.MustCompile(`\b(systemctl|service|launchctl)\b`),        // service control
	regexp.MustCompile(`\bchmod\s+(-[a-zA-Z]*\s+)*[0-7]*7{2,3}\b`), // world-writable
	regexp.MustCompile(`\bchown\b`),
	regexp.MustCompile(`(~|\$HOME|/root)/\.ssh`),                                                         // ssh key access (agent HOME is /root)
	regexp.MustCompile(`(~|\$HOME|/root)/\.aws|\.config/gh|\.netrc|\.git-credentials|id_rsa|id_ed25519`), // cred files (incl. the git PAT store, §4.5)
	regexp.MustCompile(`/etc/bitch`),                                                                     // the bridge's own secrets (§4.2)
	regexp.MustCompile(`\bfind\b[^;|&]*\s-(delete|exec|execdir|ok|okdir)\b`),                             // find as an exec/delete engine
	regexp.MustCompile(`\bufw\b|\biptables\b|\bnft\b`),                                                   // firewall tampering
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`),                                                               // fork bomb :(){ :|:& };:
	regexp.MustCompile(`>\s*/dev/(sd|nvme|disk)`),                                                        // write to raw disk
}

// safeCommands are leading tokens that are inert: they read, filter, or format,
// and cannot execute other code or write outside the current directory. The
// bar is deliberately high — under the full-root posture this allowlist is the
// primary boundary (§4.6), so anything that can interpret code (python, node,
// awk), install code from the network (npx, pip), edit arbitrary files (sed
// -i), or print secrets (env) must go through Ask instead. Commands whose
// safety depends on the subcommand (git, go, npm, …) live in safeSubcommands.
//
// Executing code that lives in the workdir (go test, npm test, pytest) is
// allowed on purpose: a coding agent that cannot run the code it writes is
// useless, and code the agent authored is at least visible in the transcript
// and the diff. The zero-footprint escapes (inline -c code, network installs)
// are what this list refuses.
var safeCommands = map[string]bool{
	"ls": true, "cat": true, "pwd": true, "echo": true, "printf": true,
	"head": true, "tail": true, "wc": true, "grep": true, "rg": true,
	"find": true, "which": true, "type": true, "file": true, "stat": true,
	"tree": true, "date": true, "whoami": true, "hostname": true,
	"uname": true, "df": true, "du": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "diff": true,
	"true": true, "false": true, "test": true, "sleep": true, "basename": true,
	"dirname": true, "realpath": true, "readlink": true, "cd": true, "jq": true,
	// format/lint/test of workdir code
	"gofmt": true, "goimports": true, "pytest": true, "tsc": true, "eslint": true,
	// make runs the workdir Makefile — the agent-authored-code class above
	"make": true,
}

// safeSubcommands are commands that are safe only with an allowlisted first
// subcommand: read-only VCS queries and build/test of workdir code. Anything
// else (git clean/reset/config, go run/generate with its module-fetch-and-run,
// npm install/ci/exec, pip install) falls through to Ask.
var safeSubcommands = map[string]map[string]bool{
	"git": {"status": true, "diff": true, "log": true, "show": true,
		"branch": true, "blame": true, "rev-parse": true, "ls-files": true,
		"describe": true, "shortlog": true, "grep": true, "fetch": true},
	"go": {"build": true, "test": true, "vet": true, "fmt": true,
		"list": true, "mod": true, "version": true, "doc": true},
	"npm":   {"test": true, "run": true, "ls": true, "view": true},
	"pnpm":  {"test": true, "run": true, "ls": true},
	"yarn":  {"test": true, "run": true},
	"cargo": {"build": true, "test": true, "check": true, "fmt": true, "clippy": true, "tree": true},
	"pip":   {"list": true, "show": true, "freeze": true, "check": true},
	"pip3":  {"list": true, "show": true, "freeze": true, "check": true},
}

var shellSplit = regexp.MustCompile(`&&|\|\||\||;|\n`)

// fdDupRe / devNullRe strip harmless redirect forms (2>&1, >/dev/null) before
// hasFileRedirect looks for real file redirection.
var (
	fdDupRe   = regexp.MustCompile(`\d*>&\d+`)
	devNullRe = regexp.MustCompile(`\d*>>?\s*/dev/null`)
)

// hasFileRedirect reports whether cmd redirects output to a file. A safe
// leading command with `> path` writes an arbitrary file as root (e.g.
// `echo x > /etc/systemd/system/bitch.service`), so any surviving `>` demotes
// the command to Ask. Conservative: a literal '>' inside quotes also trips it.
func hasFileRedirect(cmd string) bool {
	s := fdDupRe.ReplaceAllString(cmd, "")
	s = devNullRe.ReplaceAllString(s, "")
	return strings.Contains(s, ">")
}

// hasSubstitution reports whether cmd embeds command/process substitution —
// `$(…)`, backticks, `<(…)` — which hides an inner command from the leading-
// token check. Those go to Ask.
func hasSubstitution(cmd string) bool {
	return strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") ||
		strings.Contains(cmd, "<(")
}

// ClassifyBash categorizes a bash command as safe, risky, or unknown. Chained
// commands are safe only if every segment is safe; any risky segment taints the
// whole; redirects and substitutions demote to unknown (Ask). Exported for
// tests.
func ClassifyBash(cmd string) bashClass {
	if cmd == "" {
		return bashUnknown
	}
	for _, re := range denyPatterns {
		if re.MatchString(cmd) {
			return bashRisky
		}
	}
	if hasSubstitution(cmd) || hasFileRedirect(cmd) {
		return bashUnknown
	}
	segments := shellSplit.Split(cmd, -1)
	allSafe := true
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		lead, sub := leadingTokens(seg)
		switch {
		case safeCommands[lead]:
			// safe
		case safeSubcommands[lead] != nil && safeSubcommands[lead][sub]:
			// safe subcommand (git status, go test, …)
		default:
			allSafe = false
		}
	}
	if allSafe {
		return bashSafe
	}
	return bashUnknown
}

// leadingTokens extracts the executable token and its first subcommand from a
// segment, skipping leading environment-variable assignments (FOO=bar cmd …).
// The subcommand is the first argument that doesn't look like a flag; a flag
// taking a value (git -C dir log) yields the value and thus fails the
// subcommand allowlist — conservative by design.
func leadingTokens(seg string) (cmd, sub string) {
	fields := strings.Fields(seg)
	i := 0
	for ; i < len(fields); i++ {
		f := fields[i]
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // VAR=value prefix
		}
		// strip a path, e.g. /usr/bin/git -> git
		cmd = filepath.Base(f)
		break
	}
	for j := i + 1; j < len(fields); j++ {
		if !strings.HasPrefix(fields[j], "-") {
			sub = fields[j]
			break
		}
	}
	return cmd, sub
}

// --- helpers -------------------------------------------------------------

func filePathArg(in map[string]any) (string, bool) {
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if v, ok := in[k].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func resolve(workdir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(workdir, p)
	}
	return resolveSymlinks(filepath.Clean(p))
}

// resolveSymlinks resolves symlinks in the deepest existing ancestor of p, so
// the jail check sees the physical path — a symlink inside the workdir pointing
// outside it must not launder the target (§4.3). Works for paths that don't
// exist yet (new files): the missing tail is re-joined onto the resolved
// ancestor. If nothing resolves (pure lexical path, as in tests), p is
// returned unchanged.
func resolveSymlinks(p string) string {
	remainder := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // hit the root without resolving anything
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
}

func editDetail(in map[string]any) string {
	p, _ := filePathArg(in)
	if content, ok := in["content"].(string); ok {
		return p + " (write " + itoa(len(content)) + " bytes)"
	}
	if ns, ok := in["new_string"].(string); ok {
		return p + " (edit → " + itoa(len(ns)) + " new bytes)"
	}
	return p
}

func compactInput(in map[string]any) string {
	parts := make([]string, 0, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			parts = append(parts, k+"="+oneLine(s))
		}
	}
	return strings.Join(parts, " ")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func itoa(n int) string {
	// tiny local itoa to avoid pulling strconv into a hot pure path
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
