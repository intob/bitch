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
		"TodoWrite", "BashOutput":
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
	return t == "Bash" || t == "KillBash" || t == "KillShell"
}

// --- bash classification -------------------------------------------------

type bashClass int

const (
	bashUnknown bashClass = iota
	bashSafe
	bashRisky
)

// denyPatterns match destructive ops, network writes, privilege escalation, and
// credential reads. Below T3 these always ask or deny (§4.3). Fail-closed: when
// in doubt, this list errs toward flagging.
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
	regexp.MustCompile(`(~|\$HOME)/\.ssh`),                                       // ssh key access
	regexp.MustCompile(`(~|\$HOME)/\.aws|\.config/gh|\.netrc|id_rsa|id_ed25519`), // cred files
	regexp.MustCompile(`\bufw\b|\biptables\b|\bnft\b`),                           // firewall tampering
	regexp.MustCompile(`:\s*\(\s*\)\s*\{`),                                       // fork bomb :(){ :|:& };:
	regexp.MustCompile(`>\s*/dev/(sd|nvme|disk)`),                                // write to raw disk
}

// safeCommands are leading tokens that only read or do local, reversible work.
var safeCommands = map[string]bool{
	"ls": true, "cat": true, "pwd": true, "echo": true, "printf": true,
	"head": true, "tail": true, "wc": true, "grep": true, "rg": true,
	"find": true, "which": true, "type": true, "file": true, "stat": true,
	"tree": true, "date": true, "env": true, "whoami": true, "hostname": true,
	"uname": true, "df": true, "du": true, "sort": true, "uniq": true,
	"cut": true, "awk": true, "sed": true, "tr": true, "diff": true,
	"true": true, "false": true, "test": true, "sleep": true, "basename": true,
	"dirname": true, "realpath": true, "readlink": true, "cd": true, "jq": true,
	// version-control read-only
	"git": true, // git is refined below (push/remote are caught by denyPatterns)
	// build/test toolchains (local, reversible)
	"go": true, "gofmt": true, "goimports": true,
	"node": true, "npm": true, "npx": true, "pnpm": true, "yarn": true,
	"python": true, "python3": true, "pip": true, "pip3": true, "pytest": true,
	"make": true, "cargo": true, "rustc": true, "tsc": true, "eslint": true,
}

var shellSplit = regexp.MustCompile(`&&|\|\||\||;|\n`)

// ClassifyBash categorizes a bash command as safe, risky, or unknown. Chained
// commands are safe only if every segment is safe; any risky segment taints the
// whole. Exported for tests.
func ClassifyBash(cmd string) bashClass {
	if cmd == "" {
		return bashUnknown
	}
	for _, re := range denyPatterns {
		if re.MatchString(cmd) {
			return bashRisky
		}
	}
	segments := shellSplit.Split(cmd, -1)
	allSafe := true
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		lead := leadingCommand(seg)
		if !safeCommands[lead] {
			allSafe = false
		}
	}
	if allSafe {
		return bashSafe
	}
	return bashUnknown
}

// leadingCommand extracts the executable token from a segment, skipping leading
// environment-variable assignments (FOO=bar cmd …).
func leadingCommand(seg string) string {
	fields := strings.Fields(seg)
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // VAR=value prefix
		}
		// strip a path, e.g. /usr/bin/git -> git
		return filepath.Base(f)
	}
	return ""
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
	return filepath.Clean(p)
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
