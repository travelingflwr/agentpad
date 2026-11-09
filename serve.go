package main

// `agentpad serve` is the bridge for Nod, the iPhone remote: a small JSON
// API over the same files the board and the sheets use. It lists the
// agents, plan usage and open requests, streams changes, and takes answers
// by writing the very <id>.result.json a sheet writes, so the sheets, the
// terminal prompt and Activity all keep working, and whichever answers
// first wins.
//
// It listens on localhost and on this Mac's Tailscale address only, never
// on every interface. Every call but pairing needs a paired device's token.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const servePort = 7391

// serveInfo is serve.json in the state dir, so the hook can tell the bridge
// is up.
type serveInfo struct {
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Started time.Time `json:"started"`
	TLSHost string    `json:"tls_host,omitempty"` // the Tailscale name served over HTTPS
}

func serveInfoPath() string { return filepath.Join(stateDir(), "serve.json") }

// serveActive reports whether a bridge is running with a device paired, so
// a request is worth sending to a phone.
func serveActive() bool {
	data, err := os.ReadFile(serveInfoPath())
	if err != nil {
		return false
	}
	var si serveInfo
	if json.Unmarshal(data, &si) != nil || si.PID <= 0 || syscall.Kill(si.PID, 0) != nil {
		return false
	}
	return len(loadDevices()) > 0
}

const serveUsage = `agentpad serve: the bridge for the Nod iPhone app

  agentpad serve [--port N] [--demo]   run the bridge (localhost and Tailscale only)
  agentpad serve pair [--local] [--png FILE]
                                       show a QR code to pair a phone (--local: for the
                                       Simulator; --png: also save it as a picture)
  agentpad serve install               run the bridge as a service (launchd or systemd)
  agentpad serve uninstall             stop running it as a service
  agentpad serve devices               list paired devices
  agentpad serve revoke ID             unpair a device
  agentpad serve push-test [ID]        send a test notification
  agentpad serve log                   show remote decisions

--demo serves made-up agents and requests from a scratch folder, for
screenshots and trying the app; it never touches your real sessions.
`

func runServe(args []string) error {
	port, demo, local, png := servePort, false, false, ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--port":
			if i+1 < len(args) {
				port, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--demo":
			demo = true
		case "--local":
			local = true
		case "--png":
			if i+1 < len(args) {
				png = args[i+1]
				i++
			}
		case "-h", "--help", "help":
			fmt.Print(serveUsage)
			return nil
		default:
			rest = append(rest, a)
		}
	}
	if demo {
		useDemoDirs()
	}
	if len(rest) > 0 {
		switch rest[0] {
		case "pair":
			return runPair(port, local, png)
		case "devices":
			return runDevices()
		case "revoke":
			if len(rest) < 2 {
				return fmt.Errorf("usage: agentpad serve revoke ID")
			}
			if !revokeDevice(rest[1]) {
				return fmt.Errorf("no device %q", rest[1])
			}
			fmt.Println("Unpaired. Its token stops working now.")
			return nil
		case "install":
			return runServiceInstall(port, false)
		case "uninstall":
			return runServiceInstall(port, true)
		case "push-test":
			return runPushTest(rest[1:])
		case "log":
			data, err := os.ReadFile(remoteLogPath())
			if err != nil {
				fmt.Println("No remote decisions yet.")
				return nil
			}
			os.Stdout.Write(data)
			return nil
		}
		return fmt.Errorf("unknown serve command %q\n\n%s", rest[0], serveUsage)
	}
	return serve(port, demo)
}

// --- the server ---------------------------------------------------------

type server struct {
	port   int
	host   string
	pusher pusher

	mu      sync.Mutex
	snap    apiSnapshot
	body    []byte // snap as JSON, without the version
	version int
	subs    map[chan []byte]bool
	known   map[string]bool // open requests seen so far
	pushed  map[string]bool

	failMu sync.Mutex
	fails  map[string][]time.Time // auth failures by address
}

func serve(port int, demo bool) error {
	if si, ok := runningServeInfo(); ok && si.PID != os.Getpid() && serveRunning() {
		return fmt.Errorf("a bridge is already running (pid %d, port %d)", si.PID, si.Port)
	}
	host, _ := os.Hostname()
	s := &server{
		port: port, host: strings.Split(host, ".")[0],
		subs: map[chan []byte]bool{}, known: map[string]bool{}, pushed: map[string]bool{},
		fails: map[string][]time.Time{},
	}
	if demo {
		s.host = "studio"
		go runDemo()
	}
	if p, err := loadAPNs(); err == nil {
		s.pusher = p
		fmt.Println("Push: on, through APNs with key", p.cfg.KeyID)
	} else {
		fmt.Println("Push: off (" + err.Error() + ")")
	}
	s.refresh()

	// HTTPS on the Tailscale side, when the tailnet hands out certificates.
	var tlsConf *tls.Config
	tlsHost := ""
	if c, err := loadTailscaleCert(); err == nil {
		tlsConf, tlsHost = c.config(), c.domain
		fmt.Println("HTTPS: on, as", c.domain)
	} else {
		fmt.Println("HTTPS: off (" + err.Error() + "); the phone talks plain HTTP inside Tailscale")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair", s.handlePair)
	mux.HandleFunc("GET /v1/me", s.auth(s.handleMe))
	mux.HandleFunc("GET /v1/state", s.auth(s.handleState))
	mux.HandleFunc("GET /v1/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /v1/agents/{session}", s.auth(s.handleAgent))
	mux.HandleFunc("POST /v1/requests/{id}/answer", s.auth(s.handleAnswer))
	mux.HandleFunc("POST /v1/push", s.auth(s.handlePushRegister))
	mux.HandleFunc("DELETE /v1/devices/self", s.auth(s.handleUnpair))
	handler := guard(mux)

	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	info, _ := json.Marshal(serveInfo{PID: os.Getpid(), Port: port, Started: time.Now(), TLSHost: tlsHost})
	writeAtomic(serveInfoPath(), info)
	defer os.Remove(serveInfoPath())

	// Listen on localhost, and on each Tailscale address as it comes and goes.
	listening := map[string]net.Listener{}
	var lmu sync.Mutex
	listen := func(ip string, secure bool) {
		addr := net.JoinHostPort(ip, strconv.Itoa(port))
		lmu.Lock()
		defer lmu.Unlock()
		if listening[addr] != nil {
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			logf("serve: listen %s: %v", addr, err)
			fmt.Println("Can't listen on", addr+":", err)
			return
		}
		if secure {
			ln = tls.NewListener(ln, tlsConf)
		}
		listening[addr] = ln
		if secure {
			fmt.Println("Listening on", addr, "(HTTPS)")
		} else {
			fmt.Println("Listening on", addr)
		}
		go func() {
			srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
			srv.Serve(ln)
			lmu.Lock()
			delete(listening, addr)
			lmu.Unlock()
		}()
	}
	listen("127.0.0.1", false)
	for _, ip := range tailscaleIPs() {
		listen(ip, tlsConf != nil)
	}
	if len(tailscaleIPs()) == 0 {
		fmt.Println("No Tailscale address yet: only this Mac can connect until Tailscale is up.")
	}

	go func() {
		tick := time.NewTicker(400 * time.Millisecond)
		recheck := time.NewTicker(30 * time.Second)
		for {
			select {
			case <-tick.C:
				s.refresh()
			case <-recheck.C:
				for _, ip := range tailscaleIPs() {
					listen(ip, tlsConf != nil)
				}
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	return nil
}

// tailscaleIPs is this Mac's addresses in Tailscale's range (100.64.0.0/10).
func tailscaleIPs() []string {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil && cgnat.Contains(ip4) {
			out = append(out, ip4.String())
		}
	}
	return out
}

// guard turns away browsers (a page on this Mac could otherwise reach
// localhost) and caps request bodies.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "no browsers", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		h.ServeHTTP(w, r)
	})
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttled reports whether addr has failed too often lately, and slows
// it down if so.
func (s *server) throttled(addr string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	var recent []time.Time
	for _, t := range s.fails[addr] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.fails[addr] = recent
	return len(recent) >= 10
}

func (s *server) failed(addr string) {
	s.failMu.Lock()
	s.fails[addr] = append(s.fails[addr], time.Now())
	s.failMu.Unlock()
	time.Sleep(500 * time.Millisecond)
}

type authed func(w http.ResponseWriter, r *http.Request, d device)

func (s *server) auth(h authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		if s.throttled(addr) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts: wait a few minutes"})
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		d, found := deviceFor(strings.TrimSpace(tok))
		if !ok || !found {
			s.failed(addr)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not paired: pair again from Settings"})
			return
		}
		touchDevice(d, addr)
		h(w, r, d)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// --- what the app sees ----------------------------------------------------

type apiSnapshot struct {
	Host     string       `json:"host"`
	Version  int          `json:"version"`
	Agents   []apiAgent   `json:"agents"`
	Requests []apiRequest `json:"requests"`
	Plan     *planUsage   `json:"plan,omitempty"`
}

type apiAgent struct {
	SessionID string      `json:"session_id"`
	Project   string      `json:"project"`
	State     agentState  `json:"state"`
	Activity  string      `json:"activity,omitempty"`
	Prompt    string      `json:"prompt,omitempty"`
	Said      string      `json:"said,omitempty"`
	Tools     int         `json:"tools"`
	Subagents int         `json:"subagents,omitempty"`
	Started   time.Time   `json:"started"`
	Updated   time.Time   `json:"updated"`
	Context   *contextUse `json:"context,omitempty"`
	Tasks     []agentTask `json:"tasks,omitempty"`
	NeedsYou  bool        `json:"needs_you"`
}

type apiLine struct {
	Op   string `json:"op"` // " " plain, "+" added, "-" removed, "…" a note
	Text string `json:"text"`
}

type apiOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type apiQuestion struct {
	Question string      `json:"question"`
	Header   string      `json:"header,omitempty"`
	Multi    bool        `json:"multi"`
	Options  []apiOption `json:"options"`
}

type apiRequest struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Project   string        `json:"project"`
	Kind      string        `json:"kind"` // command, edit, write, fetch, question, other
	Tool      string        `json:"tool"`
	Title     string        `json:"title"`            // "Run a command", "Edit render.go"
	Detail    string        `json:"detail,omitempty"` // what it's for, or the file
	Label     string        `json:"label"`            // "Wants to run"
	Text      string        `json:"text"`             // the command, file or question, in short
	Lines     []apiLine     `json:"lines,omitempty"`
	Added     int           `json:"added,omitempty"`
	Removed   int           `json:"removed,omitempty"`
	Prose     string        `json:"prose,omitempty"`
	Always    string        `json:"always,omitempty"` // what Always allow remembers; "" means no such option
	Questions []apiQuestion `json:"questions,omitempty"`
	Opened    time.Time     `json:"opened"`
	ArmedAt   time.Time     `json:"armed_at"`
	Deadline  time.Time     `json:"deadline"`
	Subagent  bool          `json:"subagent,omitempty"`
}

const (
	maxLines    = 80
	maxLineLen  = 240
	maxTextLen  = 2000
	agentMaxAge = 24 * time.Hour
)

var (
	mdImage   = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	mdBullet  = regexp.MustCompile(`(?m)^\s*[-*]\s+`)
	spaces    = regexp.MustCompile(`\s+`)
)

// plainText turns the start of a Markdown reply into one plain line for a
// card: pictures and links keep only their words, and the markup goes.
func plainText(s string) string {
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		if alt := mdImage.FindStringSubmatch(m)[1]; alt != "" {
			return "[" + alt + "]"
		}
		return "[picture]"
	})
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdBullet.ReplaceAllString(s, "• ")
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

var (
	attachNote  = regexp.MustCompile(`(?m)^\[User attached [^\]]*\]\s*$`)
	attachImage = regexp.MustCompile(`(?m)^\[(?:Attached )?(?:image|file|Image)[^\]]*?(?:"([^"]+)")?[^\]]*\]\s*$`)
	t3Context   = regexp.MustCompile(`(?s)<t3_context[^>]*>.*?</t3_context>`)
)

// userText tidies a prompt for the phone: the notes a host adds about
// attachments (which carry local file paths) become a short marker, and
// hidden context blocks go.
func userText(s string) string {
	s = t3Context.ReplaceAllString(s, "")
	s = attachNote.ReplaceAllString(s, "")
	n := 0
	s = attachImage.ReplaceAllStringFunc(s, func(string) string { n++; return "" })
	s = strings.TrimSpace(s)
	if n > 0 {
		mark := "📎 image"
		if n > 1 {
			mark = fmt.Sprintf("📎 %d images", n)
		}
		if s == "" {
			return mark
		}
		return mark + "\n" + s
	}
	return s
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

type openRequest struct {
	req     approvalRequest
	path    string
	resPath string
	opened  time.Time
}

// openRequests is every request still waiting for an answer, oldest first.
func openRequests() []openRequest {
	files, _ := filepath.Glob(filepath.Join(requestsDir(), "*.json"))
	var out []openRequest
	now := time.Now()
	for _, f := range files {
		if strings.HasSuffix(f, ".result.json") {
			continue
		}
		res := strings.TrimSuffix(f, ".json") + ".result.json"
		if fileExists(res) {
			continue
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var req approvalRequest
		if json.Unmarshal(data, &req) != nil || req.ID == "" || now.After(req.Deadline) {
			continue
		}
		if req.HookPID > 0 && syscall.Kill(req.HookPID, 0) != nil {
			continue // Claude moved on and the hook didn't tidy up
		}
		out = append(out, openRequest{req: req, path: f, resPath: res, opened: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].opened.Before(out[j].opened) })
	return out
}

func requestKind(tool string) string {
	switch tool {
	case "Bash":
		return "command"
	case "Edit", "MultiEdit", "NotebookEdit":
		return "edit"
	case "Write":
		return "write"
	case "WebFetch", "WebSearch":
		return "fetch"
	case "AskUserQuestion":
		return "question"
	}
	return "other"
}

func toAPIRequest(o openRequest) apiRequest {
	req := o.req
	label, text, _ := requestText(req)
	out := apiRequest{
		ID: req.ID, SessionID: req.SessionID, Project: req.Project(), Kind: requestKind(req.Tool),
		Tool: req.Tool, Label: label, Text: clip(text, maxTextLen),
		Opened: o.opened, ArmedAt: o.opened.Add(armDelay), Deadline: req.Deadline, Subagent: req.Subagent,
	}
	if req.Tool == "AskUserQuestion" {
		var in struct {
			Questions []question `json:"questions"`
		}
		json.Unmarshal(req.Input, &in)
		out.Title = "Claude asks"
		for _, q := range in.Questions {
			aq := apiQuestion{Question: q.Question, Header: q.Header, Multi: q.MultiSelect}
			for _, op := range q.Options {
				aq.Options = append(aq.Options, apiOption{Label: op.Label, Description: op.Description})
			}
			out.Questions = append(out.Questions, aq)
		}
		return out
	}
	sh := newSheet(req)
	out.Title, out.Detail, out.Prose, out.Always = sh.title, sh.detail, clip(sh.prose, maxTextLen), sh.always
	for _, l := range sh.code {
		op := " "
		switch l.kind {
		case lnAdd:
			op = "+"
			out.Added++
		case lnDel:
			op = "-"
			out.Removed++
		case lnNote:
			op = "…"
		}
		if len(out.Lines) < maxLines {
			out.Lines = append(out.Lines, apiLine{op, clip(l.text, maxLineLen)})
		}
	}
	if n := len(sh.code) - maxLines; n > 0 {
		out.Lines = append(out.Lines, apiLine{"…", fmt.Sprintf("%d more lines", n)})
	}
	return out
}

func buildSnapshot(host string) apiSnapshot {
	snap := apiSnapshot{Host: host, Agents: []apiAgent{}, Requests: []apiRequest{}}
	needs := map[string]bool{}
	for _, o := range openRequests() {
		snap.Requests = append(snap.Requests, toAPIRequest(o))
		needs[o.req.SessionID] = true
	}
	now := time.Now()
	for _, a := range loadAgents() {
		if now.Sub(a.Updated) > agentMaxAge || a.State == stEnded && now.Sub(a.Updated) > time.Hour {
			continue
		}
		aa := apiAgent{
			SessionID: a.SessionID, Project: a.Project(), State: a.State, Activity: a.Activity,
			Prompt: clip(plainText(userText(a.Prompt)), 400), Said: clip(plainText(a.Said), 400), Tools: a.Tools, Subagents: a.Subagents,
			Started: a.Started, Updated: a.Updated, Tasks: a.Tasks,
			NeedsYou: needs[a.SessionID] || a.State == stWaiting,
		}
		if c, ok := contextOf(a); ok {
			aa.Context = &c
		}
		snap.Agents = append(snap.Agents, aa)
	}
	if p, ok := loadPlan(); ok {
		snap.Plan = &p
	}
	return snap
}

// refresh rebuilds the snapshot and tells the streams when it changed.
func (s *server) refresh() {
	snap := buildSnapshot(s.host)
	body, _ := json.Marshal(snap)
	s.mu.Lock()
	if bytes.Equal(body, s.body) {
		s.mu.Unlock()
		return
	}
	s.version++
	snap.Version = s.version
	s.snap, s.body = snap, body
	open := map[string]bool{}
	var fresh []apiRequest
	for _, r := range snap.Requests {
		open[r.ID] = true
		if !s.known[r.ID] {
			fresh = append(fresh, r)
		}
	}
	s.known = open
	for id := range s.pushed {
		if !open[id] {
			delete(s.pushed, id)
		}
	}
	msg, _ := json.Marshal(snap)
	for ch := range s.subs {
		select {
		case ch <- msg:
		default: // a slow reader gets the next one
		}
	}
	s.mu.Unlock()
	for _, r := range fresh {
		go s.pushLater(r)
	}
}

// pushDelay gives you a moment to answer at the Mac before the phone buzzes.
const pushDelay = 3 * time.Second

func (s *server) pushLater(r apiRequest) {
	if s.pusher == nil {
		return
	}
	time.Sleep(pushDelay)
	s.mu.Lock()
	still := s.known[r.ID] && !s.pushed[r.ID]
	if still {
		s.pushed[r.ID] = true
	}
	s.mu.Unlock()
	if !still {
		return
	}
	a := alert{Title: "Needs you", Body: "An agent needs you in " + clip(r.Project, 40), RequestID: r.ID, Thread: "needs-you"}
	if r.Kind == "question" {
		a.Body = "An agent has a question in " + clip(r.Project, 40)
	}
	s.pushAll(a)
}

func (s *server) pushAll(a alert) {
	for _, d := range loadDevices() {
		if d.APNsToken == "" {
			continue
		}
		err := s.pusher.push(d, a)
		var gone pushGone
		switch {
		case errors.As(err, &gone):
			logf("serve: push to %s: %v; forgetting its token", d.Name, err)
			changeDevices(func(list []device) []device {
				for i := range list {
					if list[i].ID == d.ID {
						list[i].APNsToken = ""
					}
				}
				return list
			})
		case err != nil:
			logf("serve: push to %s: %v", d.Name, err)
		}
	}
}

// --- handlers -------------------------------------------------------------

func (s *server) handlePair(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	if s.throttled(addr) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts: wait a few minutes"})
		return
	}
	var in struct {
		Code string `json:"code"`
		Name string `json:"device_name"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	d, tok, err := redeemPairing(in.Code, strings.TrimSpace(in.Name), addr)
	if err != nil {
		s.failed(addr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	logRemote(map[string]any{"event": "paired", "device_id": d.ID, "device": d.Name, "addr": addr})
	writeJSON(w, http.StatusOK, map[string]string{"device_id": d.ID, "token": tok, "host": s.host})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request, d device) {
	writeJSON(w, http.StatusOK, map[string]any{
		"host": s.host, "device_id": d.ID, "device_name": d.Name, "paired": d.Created,
		"push_ready": s.pusher != nil, "push_registered": d.APNsToken != "",
	})
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request, d device) {
	s.mu.Lock()
	snap := s.snap
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, snap)
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request, d device) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 4)
	s.mu.Lock()
	s.subs[ch] = true
	first, _ := json.Marshal(s.snap)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	fmt.Fprintf(w, "event: state\ndata: %s\n\n", first)
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			fl.Flush()
		}
	}
}

type apiTurn struct {
	Kind string `json:"kind"` // you, claude, tool, note
	Text string `json:"text"`
	Tool string `json:"tool,omitempty"`
	Err  bool   `json:"error,omitempty"`
}

func (s *server) handleAgent(w http.ResponseWriter, r *http.Request, d device) {
	id := r.PathValue("session")
	var found *Agent
	for _, a := range loadAgents() {
		if a.SessionID == id {
			found = &a
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	s.mu.Lock()
	var card *apiAgent
	for _, a := range s.snap.Agents {
		if a.SessionID == id {
			card = &a
		}
	}
	s.mu.Unlock()
	turns := []apiTurn{}
	if found.Transcript != "" {
		log := &chatLog{path: found.Transcript, cwd: found.Cwd, byID: map[string]int{}}
		log.read()
		items := log.items
		tools := 0
		for _, it := range items {
			if it.kind == itTool {
				tools++
			}
		}
		if card != nil {
			c := *card
			c.Tools = tools // the whole session, not just the latest turn
			card = &c
		}
		if len(items) > 40 {
			items = items[len(items)-40:]
		}
		for _, it := range items {
			switch it.kind {
			case itUser:
				turns = append(turns, apiTurn{Kind: "you", Text: clip(userText(it.text), maxTextLen)})
			case itText:
				turns = append(turns, apiTurn{Kind: "claude", Text: clip(it.text, maxTextLen)})
			case itTool:
				turns = append(turns, apiTurn{Kind: "tool", Tool: it.tool,
					Text: clip(describeTool(it.tool, it.input, log.cwd), 200), Err: it.isErr || it.denied})
			case itNote:
				turns = append(turns, apiTurn{Kind: "note", Text: clip(it.text, 400)})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": card, "turns": turns})
}

func (s *server) handleAnswer(w http.ResponseWriter, r *http.Request, d device) {
	id := r.PathValue("id")
	addr := clientAddr(r)
	var in struct {
		Decision string            `json:"decision"`
		Note     string            `json:"note"`
		Confirm  bool              `json:"confirm"`
		Answers  map[string]string `json:"answers"`
	}
	if safeName(id) != id || json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	entry := map[string]any{"event": "answer", "device_id": d.ID, "device": d.Name, "addr": addr,
		"request": id, "decision": in.Decision}
	reply := func(code int, outcome string, body map[string]any) {
		entry["outcome"] = outcome
		logRemote(entry)
		writeJSON(w, code, body)
	}
	var open *openRequest
	for _, o := range openRequests() {
		if o.req.ID == id {
			open = &o
			break
		}
	}
	if open == nil {
		gone := "Claude moved on"
		if fileExists(filepath.Join(requestsDir(), id+".result.json")) {
			gone = "Already answered"
		}
		reply(http.StatusConflict, "gone", map[string]any{"error": gone, "gone": gone})
		return
	}
	req := open.req
	entry["session"], entry["project"], entry["tool"] = req.SessionID, req.Project(), req.Tool
	if time.Now().Before(open.opened.Add(armDelay)) {
		reply(http.StatusTooEarly, "too_early", map[string]any{"error": "too early: look before you approve"})
		return
	}
	res := approvalResult{Decision: in.Decision, Via: d.Name}
	isQuestion := req.Tool == "AskUserQuestion"
	switch in.Decision {
	case "allow", "deny":
		if isQuestion {
			reply(http.StatusBadRequest, "bad", map[string]any{"error": "a question needs answers"})
			return
		}
		res.Note = clip(strings.TrimSpace(in.Note), 500)
	case "always":
		if isQuestion || alwaysSummary(req.Suggestions, req.Cwd) == "" {
			reply(http.StatusBadRequest, "bad", map[string]any{"error": "nothing to always allow here"})
			return
		}
		if !in.Confirm {
			reply(http.StatusPreconditionRequired, "unconfirmed", map[string]any{"error": "always allow needs confirming"})
			return
		}
	case "answer":
		answers, err := checkAnswers(req, in.Answers)
		if err != nil {
			reply(http.StatusBadRequest, "bad", map[string]any{"error": err.Error()})
			return
		}
		res.Answers = answers
	default:
		reply(http.StatusBadRequest, "bad", map[string]any{"error": "decision is allow, always, deny or answer"})
		return
	}
	data, _ := json.Marshal(res)
	if err := writeExclusive(open.resPath, data); err != nil {
		if errors.Is(err, os.ErrExist) {
			reply(http.StatusConflict, "gone", map[string]any{"error": "Already answered", "gone": "Already answered"})
			return
		}
		reply(http.StatusInternalServerError, "error", map[string]any{"error": err.Error()})
		return
	}
	reply(http.StatusOK, "ok", map[string]any{"ok": true})
	go s.refresh()
}

// checkAnswers makes sure each answer is one of the question's options.
func checkAnswers(req approvalRequest, got map[string]string) (map[string]string, error) {
	var in struct {
		Questions []question `json:"questions"`
	}
	json.Unmarshal(req.Input, &in)
	if len(in.Questions) == 0 {
		return nil, errors.New("this isn't a question")
	}
	out := map[string]string{}
	for _, q := range in.Questions {
		ans, ok := got[q.Question]
		if !ok || ans == "" {
			return nil, fmt.Errorf("no answer for %q", q.Question)
		}
		valid := map[string]bool{}
		for _, op := range q.Options {
			valid[op.Label] = true
		}
		picks := []string{ans}
		if q.MultiSelect {
			picks = strings.Split(ans, ", ")
		}
		for _, p := range picks {
			if !valid[p] {
				return nil, fmt.Errorf("%q isn't one of the options", p)
			}
		}
		out[q.Question] = ans
	}
	return out, nil
}

// writeExclusive writes path in one go, failing if it already exists, so
// two answers can't both land.
func writeExclusive(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.%s.tmp", path, os.Getpid(), newID())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Link(tmp, path)
}

func (s *server) handlePushRegister(w http.ResponseWriter, r *http.Request, d device) {
	var in struct {
		Token string `json:"apns_token"`
		Env   string `json:"env"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Env != "sandbox" && in.Env != "production" ||
		len(in.Token) < 32 || len(in.Token) > 200 || strings.Trim(in.Token, "0123456789abcdefABCDEF") != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad push registration"})
		return
	}
	changeDevices(func(list []device) []device {
		for i := range list {
			if list[i].ID == d.ID {
				list[i].APNsToken, list[i].APNsEnv = strings.ToLower(in.Token), in.Env
			}
		}
		return list
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push_ready": s.pusher != nil})
}

func (s *server) handleUnpair(w http.ResponseWriter, r *http.Request, d device) {
	revokeDevice(d.ID)
	logRemote(map[string]any{"event": "unpaired", "device_id": d.ID, "device": d.Name, "addr": clientAddr(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- the decision log -----------------------------------------------------

func remoteLogPath() string { return filepath.Join(stateDir(), "remote.log") }

var remoteLogMu sync.Mutex

// logRemote adds one JSON line to remote.log. It never holds the command
// or the diff, only who decided what, when, and for which request.
func logRemote(entry map[string]any) {
	entry["time"] = time.Now().Format(time.RFC3339)
	line, _ := json.Marshal(entry)
	remoteLogMu.Lock()
	defer remoteLogMu.Unlock()
	os.MkdirAll(stateDir(), 0o700)
	f, err := os.OpenFile(remoteLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// --- the other subcommands ------------------------------------------------

func runDevices() error {
	list := loadDevices()
	if len(list) == 0 {
		fmt.Println("No paired devices. Pair one with: agentpad serve pair")
		return nil
	}
	for _, d := range list {
		push := "no push"
		if d.APNsToken != "" {
			push = "push (" + d.APNsEnv + ")"
		}
		seen := "never"
		if !d.LastSeen.IsZero() {
			seen = d.LastSeen.Format("Jan 2 15:04")
		}
		fmt.Printf("%s  %-24s paired %s · last seen %s · %s\n", d.ID, d.Name, d.Created.Format("Jan 2 15:04"), seen, push)
	}
	return nil
}

func runPushTest(args []string) error {
	p, err := loadAPNs()
	if err != nil {
		return err
	}
	sent := 0
	for _, d := range loadDevices() {
		if len(args) > 0 && !strings.HasPrefix(d.ID, args[0]) || d.APNsToken == "" {
			continue
		}
		if err := p.push(d, alert{Title: "Nod", Body: "Test notification from your Mac", Thread: "test"}); err != nil {
			fmt.Printf("%s: %v\n", d.Name, err)
			continue
		}
		fmt.Printf("Sent to %s (%s)\n", d.Name, d.APNsEnv)
		sent++
	}
	if sent == 0 {
		return errors.New("no device with push registered: open Nod on the phone and allow notifications")
	}
	return nil
}

// resultVia is the device that answered a request, if a phone did.
func resultVia(resPath string) string {
	data, err := os.ReadFile(resPath)
	if err != nil {
		return ""
	}
	var res approvalResult
	json.Unmarshal(data, &res)
	return res.Via
}
