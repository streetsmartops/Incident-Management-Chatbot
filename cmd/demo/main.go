// Command demo runs incidentd's real incident core, ingest poller, triage,
// remediation and postmortem engines behind a local web chat console, with
// in-memory fakes for vendors and a scripted LLM. It needs no credentials and
// is used to record docs/demo/incidentd-demo.mp4.
//
// It is NOT Slack: rendering is a simplified simulation of chat messages.
//
//	go run ./cmd/demo -listen 127.0.0.1:8090
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/ingest"
	"github.com/streetsmartops/incident-management-chatbot/metrics"
	"github.com/streetsmartops/incident-management-chatbot/postmortem"
	"github.com/streetsmartops/incident-management-chatbot/remediation"
	"github.com/streetsmartops/incident-management-chatbot/triage"
)

//go:embed static
var static embed.FS

// Users in the demo workspace.
var users = map[string]core.UserRef{
	"alice": {Platform: "demo", ID: "alice", Email: "alice@example.com", Name: "Alice (IC)"},
	"bob":   {Platform: "demo", ID: "bob", Email: "bob@example.com", Name: "Bob (on-call)"},
	"carol": {Platform: "demo", ID: "carol", Email: "carol@example.com", Name: "Carol (checkout owner)"},
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "listen address")
	catalogDir := flag.String("catalog", "remediation/catalog", "remediation catalog")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app, err := newApp(*catalogDir, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.poller.Run(ctx)
	fmt.Printf("incidentd demo console: http://%s\n", *listen)
	if err := http.ListenAndServe(*listen, app.routes()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type app struct {
	chat    *webChat
	log     *integrationLog
	svc     *core.Service
	slo     *demoSlo
	poller  *ingest.Poller
	metrics *metrics.Prom
}

func newApp(catalogDir string, log *slog.Logger) (*app, error) {
	ilog := &integrationLog{}
	chat := newWebChat()
	store := core.NewMemStore()
	slo := &demoSlo{log: ilog, status: core.SloOK}
	paging := &loggingPaging{log: ilog}
	itsm := &loggingItsm{log: ilog}
	svc := core.NewService(store, core.Integrations{
		Chats: map[string]core.ChatPort{"demo": chat}, Paging: paging, Itsm: itsm, Slo: slo,
	}, core.ServiceConfig{
		DefaultPlatform:  "demo",
		AnnounceChannels: map[string]core.ChannelRef{"demo": {Platform: "demo", ID: "C-incidents", Name: "incidents"}},
		PageAtOrAbove:    core.Sev2,
		TicketAtOrAbove:  core.Sev3,
	}, core.DefaultSeverityPolicy(), log)

	catalog, err := remediation.LoadCatalog(catalogDir)
	if err != nil {
		return nil, err
	}
	cloud := &demoCloud{log: ilog}
	rem := &remediation.Engine{
		Catalog: catalog, Providers: map[string]core.CloudProvider{"kubernetes": cloud}, Store: store, Recorder: svc, Itsm: itsm,
		Owners: func(_ context.Context, s string) (string, bool) { return "carol@example.com", s == "checkout" },
		TTL:    15 * time.Minute, EnvEnabled: false, Now: time.Now, NewID: core.NewULIDish,
	}
	m := metrics.New(metrics.Options{Store: store, ExecutorEnabled: rem.ExecutorEnabled, Version: "demo"})
	svc.Metrics, rem.Metrics = m, m
	svc.Remediate = rem
	svc.Triage = &triage.Engine{
		LLM: &scriptedLLM{log: ilog, turns: triageScript}, Catalog: catalog, Exec: replayExecutor(),
		MaxToolCalls: 15, Timeout: 30 * time.Second, MaxOutputBytes: 8192, MaxTokens: 16000, Now: time.Now, Metrics: m,
	}
	svc.Postmortem = &postmortem.Engine{Store: store, Slo: slo, LLM: &scriptedLLM{log: ilog, turns: postmortemScript}, Itsm: itsm,
		Redactor: core.MustRedactor(true), Now: time.Now}
	poller := &ingest.Poller{Slo: slo, Store: store, Service: svc, Interval: 2 * time.Second, Log: log, Now: time.Now, OnPoll: m.Poll}
	return &app{chat: chat, log: ilog, svc: svc, slo: slo, poller: poller, metrics: m}, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/command", a.command)
	mux.HandleFunc("POST /api/action", a.action)
	mux.HandleFunc("POST /api/simulate/breach", a.breach)
	mux.Handle("GET /metrics", a.metrics.Handler())
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) state(w http.ResponseWriter, r *http.Request) {
	ch, msgs := a.chat.snapshot()
	writeJSON(w, map[string]any{"channels": ch, "messages": msgs, "log": a.log.snapshot(), "users": users, "slo": a.slo.get()})
}

type commandReq struct {
	User    string `json:"user"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

func (a *app) command(w http.ResponseWriter, r *http.Request) {
	var req commandReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u, ok := users[req.User]
	if !ok {
		http.Error(w, "unknown user", 400)
		return
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Text), "/incident"))
	a.chat.userPost(req.Channel, u, "/incident "+text)
	var mentions []core.UserRef
	for _, id := range []string{"alice", "bob", "carol"} {
		if strings.Contains(text, "@"+id) {
			mentions = append(mentions, users[id])
		}
	}
	cmd, err := core.ParseCommand(text)
	var msg core.Message
	if err == nil {
		msg, err = a.svc.Execute(r.Context(), core.CommandContext{Actor: u, Channel: core.ChannelRef{Platform: "demo", ID: req.Channel}, Mentions: mentions}, cmd)
	}
	a.reply(req.Channel, u, msg, err)
	writeJSON(w, map[string]bool{"ok": true})
}

type actionReq struct {
	User     string `json:"user"`
	Channel  string `json:"channel"`
	ActionID string `json:"actionId"`
	Value    string `json:"value"`
}

func (a *app) action(w http.ResponseWriter, r *http.Request) {
	var req actionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	u, ok := users[req.User]
	if !ok {
		http.Error(w, "unknown user", 400)
		return
	}
	msg, err := a.svc.HandleInteraction(r.Context(), core.CommandContext{Actor: u, Channel: core.ChannelRef{Platform: "demo", ID: req.Channel}}, req.ActionID, req.Value)
	if err == nil {
		msg.Ephemeral = false
	}
	a.reply(req.Channel, u, msg, err)
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *app) reply(channel string, u core.UserRef, msg core.Message, err error) {
	if err != nil {
		text := "Something went wrong: " + err.Error()
		if ue, ok := err.(*core.UserError); ok {
			text = ue.Msg
		}
		msg = core.Message{Text: text, Ephemeral: true}
	}
	if msg.Text == "" && msg.Title == "" {
		return
	}
	a.chat.botPost(channel, msg, u.ID)
}

func (a *app) breach(w http.ResponseWriter, r *http.Request) {
	a.slo.set(core.SloBreached)
	a.log.add("multiplexer", "Guardrail 7 \"Checkout availability\" (tenant acme, enterprise) is now BREACHED")
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- web chat

type chatMsg struct {
	ID        int          `json:"id"`
	Channel   string       `json:"channel"`
	Author    string       `json:"author"` // bot | user id
	Name      string       `json:"name"`
	At        time.Time    `json:"at"`
	Text      string       `json:"text,omitempty"`
	Msg       core.Message `json:"msg"`
	Ephemeral string       `json:"ephemeral,omitempty"` // visible only to this user
}

type channel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Topic   string   `json:"topic"`
	Members []string `json:"members"`
	Private bool     `json:"private"`
}

type webChat struct {
	mu       sync.Mutex
	channels []*channel
	msgs     []chatMsg
	n        int
}

func newWebChat() *webChat {
	return &webChat{channels: []*channel{{ID: "C-incidents", Name: "incidents", Topic: "Incident announcements and proposals"}}}
}

func (c *webChat) find(id string) *channel {
	for _, ch := range c.channels {
		if ch.ID == id {
			return ch
		}
	}
	return nil
}

func (c *webChat) snapshot() ([]channel, []chatMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channel, len(c.channels))
	for i, ch := range c.channels {
		out[i] = *ch
	}
	return out, append([]chatMsg(nil), c.msgs...)
}

func (c *webChat) userPost(ch string, u core.UserRef, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.msgs = append(c.msgs, chatMsg{ID: c.n, Channel: ch, Author: u.ID, Name: u.Name, At: time.Now(), Text: text})
}

func (c *webChat) botPost(ch string, m core.Message, ephemeralFor string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	cm := chatMsg{ID: c.n, Channel: ch, Author: "bot", Name: "incidentbot", At: time.Now(), Msg: m}
	if m.Ephemeral {
		cm.Ephemeral = ephemeralFor
	}
	c.msgs = append(c.msgs, cm)
}

func (c *webChat) Platform() string { return "demo" }

func (c *webChat) PostMessage(_ context.Context, ch core.ChannelRef, m core.Message) (core.MessageRef, error) {
	c.botPost(ch.ID, m, "")
	return core.MessageRef{Channel: ch, ID: fmt.Sprint(c.n)}, nil
}

func (c *webChat) UpdateMessage(context.Context, core.MessageRef, core.Message) error { return nil }

func (c *webChat) CreateWarRoom(_ context.Context, name string, private bool) (core.ChannelRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := &channel{ID: fmt.Sprintf("C-%d", len(c.channels)+1), Name: name, Private: private}
	c.channels = append(c.channels, ch)
	return core.ChannelRef{Platform: "demo", ID: ch.ID, Name: name}, nil
}

func (c *webChat) Invite(_ context.Context, ref core.ChannelRef, us []core.UserRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch := c.find(ref.ID); ch != nil {
		for _, u := range us {
			ch.Members = append(ch.Members, u.ID)
		}
	}
	return nil
}

func (c *webChat) SetTopic(_ context.Context, ref core.ChannelRef, topic string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch := c.find(ref.ID); ch != nil {
		ch.Topic = topic
	}
	return nil
}

func (c *webChat) LookupUserByEmail(_ context.Context, email string) (core.UserRef, error) {
	for _, u := range users {
		if u.Email == email {
			return u, nil
		}
	}
	return core.UserRef{}, core.ErrNotFound
}

// ---------------------------------------------------------------- integration log

type logEntry struct {
	At     time.Time `json:"at"`
	System string    `json:"system"`
	Text   string    `json:"text"`
}

type integrationLog struct {
	mu      sync.Mutex
	entries []logEntry
}

func (l *integrationLog) add(system, format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{At: time.Now(), System: system, Text: fmt.Sprintf(format, a...)})
}

func (l *integrationLog) snapshot() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logEntry(nil), l.entries...)
}
