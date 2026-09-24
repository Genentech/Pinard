package webterm

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"github.com/Genentech/pinard/internal/pnats"
)

//go:embed web
var webFS embed.FS

// Gateway serves the xterm.js frontend and bridges browser WebSockets to the
// host-side responder over NATS. It mints a short-lived grant per view that the
// responder verifies.
//
// Auth (Phase 2): when Auth is non-nil the gateway enforces Cognito login and
// authorizes per target (operator → any target; viewer → a signed-link target).
// When Auth is nil it keeps Phase-1 behavior (the signed link is the sole gate).
type Gateway struct {
	NC          *nats.Conn
	Vignobles   []string // served vignobles; a request's v must be one of these
	LinkSecret  []byte
	GrantSecret []byte
	GrantTTL    time.Duration // short (~60s); default applied if zero
	ReqTimeout  time.Duration // request/reply timeout to the responder; default 5s

	Auth     *Authenticator    // nil = auth disabled (Phase-1 fallback)
	Owners   *OwnerStore       // nil = no operator discovery
	Funders  *FunderStore      // nil = no capsule funder discovery
	AgentsKV pnats.KVReader    // pinard-agents KV reader; falls back to Owners.KV when nil

	tmpl     *template.Template
	upgrader websocket.Upgrader
	once     sync.Once
}

// serves reports whether the gateway serves vignoble v.
//   - explicit allowlist (Vignobles) set → v must be in it;
//   - else, if operator discovery is available → serve any vignoble that has
//     published an owner to the pinard-vignobles KV (self-maintaining: a new
//     vignoble's daemon publishes on startup, so no gateway redeploy is needed);
//   - else (no list, no KV) → serve any (last-resort; the secret is the gate).
//
// An unserved vignoble is denied with 403 by the handlers.
func (g *Gateway) serves(v string) bool {
	if v == "" {
		return false
	}
	if len(g.Vignobles) > 0 {
		for _, s := range g.Vignobles {
			if s == v {
				return true
			}
		}
		return false
	}
	if g.Owners != nil {
		return g.Owners.Owner(v) != ""
	}
	return true
}

func (g *Gateway) grantTTL() time.Duration {
	if g.GrantTTL > 0 {
		return g.GrantTTL
	}
	return 60 * time.Second
}

func (g *Gateway) reqTimeout() time.Duration {
	if g.ReqTimeout > 0 {
		return g.ReqTimeout
	}
	return 5 * time.Second
}

func (g *Gateway) init() {
	g.once.Do(func() {
		g.tmpl = template.Must(template.ParseFS(webFS, "web/*.html"))
		// Phase 1 is internal-only; permit same-origin and file loads. Origin is
		// not a security boundary here (the signed link is), so allow all.
		g.upgrader = websocket.Upgrader{
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 32 * 1024,
		}
	})
}

// Handler returns the HTTP handler mounted at /sessions.
func (g *Gateway) Handler() http.Handler {
	g.init()
	mux := http.NewServeMux()

	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/sessions/static/", http.StripPrefix("/sessions/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("/sessions/ws", g.handleWS)
	if g.Auth != nil {
		// Control-room index APIs (operator-gated).
		mux.HandleFunc("/sessions/api/vignobles", g.handleAPIVignobles)
		mux.HandleFunc("/sessions/api/sessions", g.handleAPISessions)
	}
	if g.Auth != nil {
		mux.HandleFunc("/sessions/auth/login", func(w http.ResponseWriter, r *http.Request) {
			g.Auth.StartLogin(w, r, r.URL.Query().Get("returnTo"))
		})
		// Serve the callback at the default path and at the configured redirect_uri
		// path (e.g. a Cognito-registered /api/oauth2-redirect), so both work.
		mux.HandleFunc("/sessions/auth/callback", g.Auth.HandleCallback)
		if cb := g.Auth.CallbackPath(); cb != "" && cb != "/sessions/auth/callback" {
			mux.HandleFunc(cb, g.Auth.HandleCallback)
		}
	}
	mux.HandleFunc("/sessions", g.handlePage)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// resolveTarget reads the requested vignoble + tmux target and whether a valid
// signed link scopes to them. vignoble/target may be present without a valid link
// (an operator can open any target); linkValid reports whether the v/exp/sig
// authorize this exact vignoble+target.
func (g *Gateway) resolveTarget(r *http.Request) (vignoble, target string, linkValid bool) {
	q := r.URL.Query()
	vignoble = q.Get("v")
	target = q.Get("target")
	if target == "" {
		return vignoble, "", false
	}
	exp, err := ParseExp(q.Get("exp"))
	if err != nil {
		return vignoble, target, false
	}
	linkValid = VerifyLink(vignoble, target, exp, q.Get("sig"), g.LinkSecret, time.Now()) == nil
	return vignoble, target, linkValid
}

// identify returns the authenticated identity. When auth is disabled it returns
// an empty identity marked present (Phase-1 anonymous access).
func (g *Gateway) identify(r *http.Request) (Identity, bool) {
	if g.Auth == nil {
		return Identity{}, true
	}
	return g.Auth.Identify(r)
}

func (g *Gateway) isOperator(vignoble string, id Identity) bool {
	return g.Owners != nil && g.Owners.IsOperator(vignoble, id.Username)
}

func (g *Gateway) isFunder(vignoble, target string, id Identity) bool {
	return g.Funders != nil && g.Funders.IsFunder(vignoble, target, id.Username)
}

func (g *Gateway) handlePage(w http.ResponseWriter, r *http.Request) {
	g.init()
	vignoble, target, linkValid := g.resolveTarget(r)

	id, authed := g.identify(r)
	if g.Auth != nil && !authed {
		g.Auth.StartLogin(w, r, r.URL.RequestURI())
		return
	}

	// No target → the control-room index (operator only; requires auth). Viewers
	// with a scoped link always carry a target and take the terminal path below.
	if target == "" {
		if g.Auth == nil || len(g.ownedBy(id)) == 0 {
			http.Error(w, "No sessions: not an operator of any vignoble.", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = g.tmpl.ExecuteTemplate(w, "index.html", nil)
		return
	}

	if !g.serves(vignoble) {
		http.Error(w, "Access denied: unknown vignoble", http.StatusForbidden)
		return
	}
	dec := Authorize(id, linkValid, g.isOperator(vignoble, id), g.isFunder(vignoble, target, id), g.Auth != nil, wantsWritable(r))
	if !dec.Allowed {
		http.Error(w, "Access denied: "+dec.Reason, http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	issueURL, issueLabel := g.resolveIssueURL(target)
	_ = g.tmpl.ExecuteTemplate(w, "term.html", map[string]any{
		"Target":     target,
		"Vignoble":   vignoble,
		"Writable":   dec.Mode == ModeRW,
		"IsFunder":   dec.Role == "funder",
		"IssueURL":   issueURL,
		"IssueLabel": issueLabel,
	})
}

// resolveIssueURL returns the issue URL and a short label (e.g. "#123") for the
// given session target, read from the pinard-agents KV record. Returns ("", "")
// when unavailable or no issue is associated with the session.
func (g *Gateway) resolveIssueURL(target string) (string, string) {
	kv := g.agentsKV()
	if kv == nil || target == "" {
		return "", ""
	}
	// Try direct Get by key first (session name is the KV key for non-process workers).
	if rec, err := kv.Get("pinard-agents", target); err == nil && rec != nil {
		if u, ok := rec["issueUrl"].(string); ok && u != "" {
			return u, issueLabel(u)
		}
	}
	// Fallback: scan all keys matching name == target (process workers use runId as key).
	keys, err := kv.Keys("pinard-agents")
	if err != nil {
		return "", ""
	}
	for _, k := range keys {
		rec, err := kv.Get("pinard-agents", k)
		if err != nil || rec == nil {
			continue
		}
		if name, _ := rec["name"].(string); name != target {
			continue
		}
		if u, ok := rec["issueUrl"].(string); ok && u != "" {
			return u, issueLabel(u)
		}
		break
	}
	return "", ""
}

// issueLabel derives a short display label from an issue URL, e.g. "#123".
func issueLabel(issueURL string) string {
	// URL shape: https://host/group/project/-/issues/123
	idx := strings.LastIndex(issueURL, "/")
	if idx >= 0 && idx < len(issueURL)-1 {
		return "#" + issueURL[idx+1:]
	}
	return issueURL
}

// wantsWritable reports whether the request opted into writable "steer" (?mode=rw).
func wantsWritable(r *http.Request) bool { return r.URL.Query().Get("mode") == ModeRW }

// ownedBy returns the vignobles the identity operates (empty if no discovery).
func (g *Gateway) ownedBy(id Identity) []string {
	if g.Owners == nil || id.Username == "" {
		return nil
	}
	// With an explicit allowlist, resolve ownership via direct per-key Get over the
	// known vignobles — the reliable path (same one IsOperator uses). This avoids
	// KV key enumeration entirely (issue #115). Falls back to full KV enumeration
	// only in KV-derived mode (no allowlist configured).
	if len(g.Vignobles) > 0 {
		return g.Owners.OwnedByAmong(id.Username, g.Vignobles)
	}
	return g.Owners.OwnedBy(id.Username)
}

func newViewerID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (g *Gateway) handleWS(w http.ResponseWriter, r *http.Request) {
	g.init()
	vignoble, target, linkValid := g.resolveTarget(r)

	id, authed := g.identify(r)
	if g.Auth != nil && !authed {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !g.serves(vignoble) {
		http.Error(w, "unknown vignoble", http.StatusForbidden)
		return
	}
	dec := Authorize(id, linkValid, g.isOperator(vignoble, id), g.isFunder(vignoble, target, id), g.Auth != nil, wantsWritable(r))
	if !dec.Allowed {
		http.Error(w, "Access denied: "+dec.Reason, http.StatusForbidden)
		return
	}
	if target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	conn, err := g.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	viewerID := newViewerID()
	g.audit(r, id, vignoble, target, dec.Mode, dec.Role)

	// Subscribe to the per-viewer output/event subjects BEFORE requesting, so no
	// initial output is missed.
	var wsMu sync.Mutex
	writeBin := func(b []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		return conn.WriteMessage(websocket.BinaryMessage, b)
	}
	writeTxt := func(s string) {
		wsMu.Lock()
		defer wsMu.Unlock()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(s))
	}

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() { closeOnce.Do(func() { close(done) }) }

	outSub, err := g.NC.Subscribe(OutSubject(vignoble, viewerID), func(m *nats.Msg) {
		if writeBin(m.Data) != nil {
			closeAll()
		}
	})
	if err != nil {
		writeTxt("\r\n[gateway error]\r\n")
		return
	}
	defer outSub.Unsubscribe()

	evtSub, err := g.NC.Subscribe(EvtSubject(vignoble, viewerID), func(m *nats.Msg) {
		var e EvtMsg
		if json.Unmarshal(m.Data, &e) == nil && e.Type == EvtEnded {
			writeTxt("\r\n\x1b[90m[session ended: " + e.Reason + "]\x1b[0m\r\n")
			closeAll()
		}
	})
	if err != nil {
		writeTxt("\r\n[gateway error]\r\n")
		return
	}
	defer evtSub.Unsubscribe()

	// Mint the grant (mode from the authz decision) and request a viewer.
	grant, err := SignGrant(Grant{Vignoble: vignoble, Target: target, Mode: dec.Mode, Exp: time.Now().Add(g.grantTTL()).Unix()}, g.GrantSecret)
	if err != nil {
		writeTxt("\r\n[gateway misconfigured]\r\n")
		return
	}
	cols, rows := parseDims(r.URL.Query().Get("cols"), r.URL.Query().Get("rows"))
	reqData, _ := json.Marshal(ReqMsg{Grant: grant, ViewerID: viewerID, Cols: cols, Rows: rows})
	msg, err := g.NC.Request(ReqSubject(vignoble), reqData, g.reqTimeout())
	if err != nil {
		writeTxt("\r\n\x1b[31m[session not found or ended — no responder answered]\x1b[0m\r\n")
		return
	}
	var reply ReqReply
	if json.Unmarshal(msg.Data, &reply) != nil || !reply.OK {
		reason := reply.Reason
		if reason == "" {
			reason = "session not found / ended"
		}
		writeTxt("\r\n\x1b[31m[" + reason + "]\x1b[0m\r\n")
		return
	}

	// Reader loop: browser → responder. Text frames are control (resize/heartbeat).
	// Binary frames are keystrokes — forwarded to the input subject ONLY when the
	// grant is writable (steer); dropped for read-only.
	writable := dec.Mode == ModeRW
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				closeAll()
				return
			}
			if mt == websocket.BinaryMessage {
				if writable {
					_ = g.NC.Publish(InSubject(vignoble, viewerID), data)
				}
				continue
			}
			var c CtlMsg
			if json.Unmarshal(data, &c) != nil {
				continue
			}
			switch c.Type {
			case CtlResize, CtlHeartbeat:
				_ = g.NC.Publish(CtlSubject(vignoble, viewerID), data)
			case CtlInterrupt:
				// Interrupt is allowed for any valid grant (RO or RW), not operator-gated.
				g.auditInterrupt(r, id, vignoble, target, dec.Role)
				_ = g.NC.Publish(CtlSubject(vignoble, viewerID), data)
			}
		}
	}()

	<-done
	// Tell the responder to tear down its PTY.
	closeMsg, _ := json.Marshal(CtlMsg{Type: CtlClose})
	_ = g.NC.Publish(CtlSubject(vignoble, viewerID), closeMsg)
}

// ── Control-room index APIs (operator-gated) ──────────────────

type sessionEntry struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Parcelle    string `json:"parcelle,omitempty"`
	State       string `json:"state,omitempty"`
	Step        string `json:"step,omitempty"`
	Remote      bool   `json:"remote,omitempty"`
	Unreachable bool   `json:"unreachable,omitempty"`
	Build       string `json:"build,omitempty"`
}

type sessionIndex struct {
	Vignoble    string         `json:"vignoble"`
	Regisseur   *sessionEntry  `json:"regisseur,omitempty"`
	Maitres     []sessionEntry `json:"maitres"`
	Vendangeurs []sessionEntry `json:"vendangeurs"`
	Note        string         `json:"note,omitempty"`
}

func (g *Gateway) handleAPIVignobles(w http.ResponseWriter, r *http.Request) {
	g.init()
	id, authed := g.identify(r)
	if g.Auth != nil && !authed {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"vignobles": g.ownedBy(id)})
}

func (g *Gateway) handleAPISessions(w http.ResponseWriter, r *http.Request) {
	g.init()
	id, authed := g.identify(r)
	if g.Auth != nil && !authed {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	v := r.URL.Query().Get("v")
	if v == "" {
		http.Error(w, "missing v", http.StatusBadRequest)
		return
	}
	if !g.isOperator(v, id) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	grant, err := SignGrant(Grant{Vignoble: v, Mode: ModeList, Exp: time.Now().Add(g.grantTTL()).Unix()}, g.GrantSecret)
	if err != nil {
		http.Error(w, "gateway misconfigured", http.StatusInternalServerError)
		return
	}
	reqData, _ := json.Marshal(ListReq{Grant: grant})
	w.Header().Set("Content-Type", "application/json")

	msg, err := g.NC.Request(ListSubject(v), reqData, g.reqTimeout())
	if err != nil {
		// No live responder — still surface remote KV agents so they're visible
		// even when the daemon's webterm responder is down.
		idx := g.buildIndex(v, ListReply{})
			idx.Note = "no local responder"
		_ = json.NewEncoder(w).Encode(idx)
		return
	}
	var reply ListReply
	if json.Unmarshal(msg.Data, &reply) != nil || !reply.OK {
		http.Error(w, "enumeration failed", http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(g.buildIndex(v, reply))
}

// agentLivenessThreshold is the maximum age of a KV lastSeen timestamp before
// the record is considered stale and excluded from the /sessions index.
const agentLivenessThreshold = 5 * time.Minute

// agentsKV returns the KV reader for the pinard-agents bucket: AgentsKV if set,
// else Owners.KV (the same NATS KV client serves both buckets).
func (g *Gateway) agentsKV() pnats.KVReader {
	if g.AgentsKV != nil {
		return g.AgentsKV
	}
	if g.Owners != nil && g.Owners.KV != nil {
		return g.Owners.KV
	}
	return nil
}

// buildIndex turns a responder's raw tmux listing into the control-room view:
// régisseur (conductor window 0 / named), maîtres (other conductor windows), and
// vendangeurs (non-conductor sessions), enriched with parcelle + live state.
// Remote agents (not a local tmux session) are discovered from the pinard-agents
// KV and unioned in, flagged Remote:true.
func (g *Gateway) buildIndex(v string, reply ListReply) sessionIndex {
	idx := sessionIndex{Vignoble: v, Maitres: []sessionEntry{}, Vendangeurs: []sessionEntry{}}
	for _, wdw := range reply.Windows {
		isReg := wdw.Index == 0 || strings.Contains(wdw.Name, "gisseur") // régisseur / regisseur
		if isReg && idx.Regisseur == nil {
			// Window target → viewed via a grouped session (non-disruptive).
			idx.Regisseur = &sessionEntry{Name: "régisseur", Target: fmt.Sprintf("conductor:%d", wdw.Index)}
			continue
		}
		idx.Maitres = append(idx.Maitres, sessionEntry{
			Name:   wdw.Name,
			Target: fmt.Sprintf("conductor:%d", wdw.Index),
		})
	}
	// Track local session names to avoid duplicating them from the KV scan.
	localSessions := make(map[string]bool, len(reply.Sessions))
	for _, s := range reply.Sessions {
		localSessions[s] = true
	}
	for _, s := range reply.Sessions {
		if s == "conductor" {
			continue
		}
		e := sessionEntry{Name: s, Target: s}
		// Parcelle is the session-name prefix (<parcelle>--<project>-<id>).
		if pfx, _, ok := strings.Cut(s, "--"); ok {
			e.Parcelle = pfx
		}
		// Live state + step (best-effort) from the pinard-agents KV.
		if kv := g.agentsKV(); kv != nil {
			if rec, err := kv.Get("pinard-agents", s); err == nil && rec != nil {
				if st, ok := rec["tempo"].(string); ok {
					e.State = st
				}
				if st, ok := rec["step"].(string); ok {
					e.Step = st
				}
				if b, ok := rec["build"].(string); ok {
					e.Build = b
				}
			}
		}
		idx.Vendangeurs = append(idx.Vendangeurs, e)
	}
	// Union in remote agents: KV records for this vignoble whose session name
	// is not a local tmux session. Stale records (lastSeen older than threshold
	// or absent) are skipped so they disappear automatically.
	if kv := g.agentsKV(); kv != nil {
		keys, err := kv.Keys("pinard-agents")
		if err == nil {
			for _, k := range keys {
				rec, err := kv.Get("pinard-agents", k)
				if err != nil || rec == nil {
					continue
				}
				// Only include records for this vignoble.
				vig, _ := rec["vignoble"].(string)
				if vig != v {
					continue
				}
				name, _ := rec["name"].(string)
				if name == "" || localSessions[name] {
					continue // already listed from tmux
				}
				// Liveness check via lastSeen.
				lastSeenStr, _ := rec["lastSeen"].(string)
				if lastSeenStr == "" {
					continue // no heartbeat timestamp — cannot determine liveness
				}
				lastSeenAt, err := time.Parse(time.RFC3339, lastSeenStr)
				if err != nil || time.Since(lastSeenAt) > agentLivenessThreshold {
					continue // stale
				}
				// Determine locality from the KV record's standalone marker.
				// standalone=true → genuinely remote (HPC/SIF, no local daemon).
				// standalone absent/false → local agent whose responder is down.
				isStandalone, _ := rec["standalone"].(bool)
				e := sessionEntry{Name: name, Target: name, Remote: isStandalone, Unreachable: !isStandalone}
				if pfx, _, ok := strings.Cut(name, "--"); ok {
					e.Parcelle = pfx
				} else if p, ok := rec["parcelle"].(string); ok {
					e.Parcelle = p
				}
				if st, ok := rec["tempo"].(string); ok {
					e.State = st
				}
				if st, ok := rec["step"].(string); ok {
					e.Step = st
				}
				if b, ok := rec["build"].(string); ok {
					e.Build = b
				}
				idx.Vendangeurs = append(idx.Vendangeurs, e)
			}
		}
	}
	return idx
}

// parseDims parses cols/rows query params, falling back to 80x24 if absent or invalid.
func parseDims(colsStr, rowsStr string) (cols, rows int) {
	const defCols, defRows = 80, 24
	c, err1 := strconv.Atoi(colsStr)
	r, err2 := strconv.Atoi(rowsStr)
	if err1 != nil || err2 != nil || c <= 0 || r <= 0 {
		return defCols, defRows
	}
	return c, r
}

func (g *Gateway) audit(r *http.Request, id Identity, vignoble, target, mode, role string) {
	// Records who opened which target, when, and read-only/writable. With auth
	// enabled the identity is the verified username; otherwise "link" (Phase-1).
	who := id.Username
	if who == "" {
		who = "link"
	}
	log.Printf("[webterm-audit] identity=%s role=%s src=%s vignoble=%s target=%s mode=%s ts=%s",
		who, role, r.RemoteAddr, vignoble, target, mode, time.Now().UTC().Format(time.RFC3339))
}

func (g *Gateway) auditInterrupt(r *http.Request, id Identity, vignoble, target, role string) {
	who := id.Username
	if who == "" {
		who = "link"
	}
	log.Printf("[webterm-audit] action=interrupt identity=%s role=%s src=%s vignoble=%s target=%s ts=%s",
		who, role, r.RemoteAddr, vignoble, target, time.Now().UTC().Format(time.RFC3339))
}
