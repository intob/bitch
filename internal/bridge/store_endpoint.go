package bridge

import (
	"encoding/json"
	"log"
	"net/http"
)

// The /store loopback endpoint backs the agent-driven capture MCP tools
// (`bitch mcp`): when the child Claude calls remember_fact or save_secret, the
// MCP server POSTs here to persist the value. Confirmation is NOT done here — it
// already happened at the PreToolUse gate, which asks over Telegram before the
// MCP tool is allowed to run (an MCP tool is an "unknown tool" → Ask at T1+,
// with the secret value redacted on the card). So by the time /store is called,
// the human has approved. The shared-secret header gates the endpoint the same
// way as /approve.

// storeRequest is what `bitch mcp` POSTs to persist a captured value.
type storeRequest struct {
	Kind        string `json:"kind"` // "fact" | "secret"
	Key         string `json:"key"`  // fact key
	Name        string `json:"name"` // secret env-var name
	Value       string `json:"value"`
	Description string `json:"description"`
}

type storeResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (b *Bridge) handleStore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Bitch-Secret") != b.cfg.HookSecret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req storeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeStore(w, false, "bad request")
		return
	}

	switch req.Kind {
	case "fact":
		if err := b.store.SaveFact(req.Key, req.Value); err != nil {
			writeStore(w, false, err.Error())
			return
		}
		log.Printf("agent stored fact %q", req.Key)
		writeStore(w, true, "")
	case "secret":
		if err := b.secrets.SaveSecret(req.Name, req.Value, req.Description); err != nil {
			writeStore(w, false, err.Error())
			return
		}
		log.Printf("agent stored secret %q", req.Name)
		writeStore(w, true, "")
	default:
		writeStore(w, false, "unknown kind")
	}
}

func writeStore(w http.ResponseWriter, ok bool, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(storeResponse{OK: ok, Error: errMsg})
}
