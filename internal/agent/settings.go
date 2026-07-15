package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

// writeSettings emits the Claude Code settings.json that installs the PreToolUse
// permission hook (DESIGN.md §4.3, §9.2). The hook fires before *every* tool
// call and shells out to `bitch hook`, which calls back into the bridge to apply
// the tier policy and, when needed, ask over Telegram. Because the hook is
// authoritative (its allow/deny short-circuits Claude Code's own permission
// check), this is the boundary that survives the full-root posture (§4.6).
func writeSettings(path, exe string) error {
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*", // every tool
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": fmt.Sprintf("%q hook", exe),
							// Cover the human approval window plus slack; the
							// bridge itself enforces the real timeout (§4.3).
							"timeout": 600,
						},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: marshal settings: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("agent: write settings: %w", err)
	}
	return nil
}
