package slack_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/chat/slack"
	"github.com/streetsmartops/incident-management-chatbot/core"
)

const secret = "8f742231b10e8888abcd99yyyzzz85a5"

func sign(t *testing.T, body string, ts time.Time) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(body))
	tss := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + tss + ":" + body))
	r.Header.Set("X-Slack-Request-Timestamp", tss)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestVerify(t *testing.T) {
	now := time.Now()
	if _, err := slack.Verify(sign(t, "a=b", now), secret, now); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if _, err := slack.Verify(sign(t, "a=b", now.Add(-6*time.Minute)), secret, now); !errors.Is(err, slack.ErrBadSignature) {
		t.Fatal("stale timestamp must be rejected (replay)")
	}
	r := sign(t, "a=b", now)
	r.Body = io.NopCloser(strings.NewReader("a=c"))
	if _, err := slack.Verify(r, secret, now); !errors.Is(err, slack.ErrBadSignature) {
		t.Fatal("tampered body must be rejected")
	}
	if _, err := slack.Verify(sign(t, "a=b", now), "", now); err == nil {
		t.Fatal("empty secret must fail closed")
	}
}

// fakeSlack serves the Web API methods the adapter uses.
func fakeSlack(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var calls []string
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		mu.Lock()
		calls = append(calls, method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "users.info":
			id := r.Form.Get("user")
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"` + id + `","real_name":"User ` + id + `","profile":{"email":"` + strings.ToLower(id) + `@example.com"}}}`))
		case "conversations.create":
			if r.Form.Get("name") == "" {
				t.Error("conversations.create without name")
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"CWAR1","name":"` + r.Form.Get("name") + `"}}`))
		case "conversations.invite", "conversations.setTopic":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"CWAR1"}}`))
		case "chat.postMessage":
			if r.Form.Get("blocks") == "" {
				t.Error("postMessage without blocks")
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"` + r.Form.Get("channel") + `","ts":"1700000000.000100"}`))
		case "users.lookupByEmail":
			_, _ = w.Write([]byte(`{"ok":false,"error":"users_not_found"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
		}
	})), &calls
}

func TestSlashCommandEndToEnd(t *testing.T) {
	api, calls := fakeSlack(t)
	defer api.Close()
	var mu sync.Mutex
	var replies []map[string]any
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		replies = append(replies, m)
		mu.Unlock()
	}))
	defer respSrv.Close()

	client := slack.New("xoxb-test", api.URL+"/api/", api.Client())
	store := core.NewMemStore()
	svc := core.NewService(store, core.Integrations{Chats: map[string]core.ChatPort{"slack": client}},
		core.ServiceConfig{DefaultPlatform: "slack", AnnounceChannels: map[string]core.ChannelRef{"slack": {Platform: "slack", ID: "CANN"}}},
		core.DefaultSeverityPolicy(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := &slack.Handler{Client: client, Service: svc, SigningSecret: secret, HTTP: respSrv.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}

	form := url.Values{"command": {"/incident"}, "text": {`declare "Checkout 500s" --sev 2`}, "user_id": {"UALICE"}, "channel_id": {"CANN"}, "response_url": {respSrv.URL + "/r"}}
	rec := httptest.NewRecorder()
	h.ServeCommand(rec, sign(t, form.Encode(), time.Now()))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Working on") {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	h.Wait()
	if len(replies) != 1 || !strings.Contains(replies[0]["text"].(string), "INC-") {
		t.Fatalf("reply: %+v", replies)
	}
	inc, err := store.FindIncidentByKey(t.Context(), "INC-"+strconv.Itoa(time.Now().Year())+"-0001")
	if err != nil || inc.WarRoom == nil || inc.WarRoom.ID != "CWAR1" {
		t.Fatalf("incident: %+v %v", inc, err)
	}
	if ic, _ := inc.IC(); ic.Email != "ualice@example.com" {
		t.Fatalf("actor email must be resolved via users.info, got %+v", ic)
	}
	joined := strings.Join(*calls, ",")
	for _, m := range []string{"users.info", "conversations.create", "conversations.invite", "conversations.setTopic", "chat.postMessage"} {
		if !strings.Contains(joined, m) {
			t.Errorf("expected call %s in %s", m, joined)
		}
	}

	// Mention resolution with escaped mentions.
	form = url.Values{"text": {`role comms <@UBOB|bob>`}, "user_id": {"UALICE"}, "channel_id": {"CWAR1"}, "response_url": {respSrv.URL + "/r"}}
	h.ServeCommand(httptest.NewRecorder(), sign(t, form.Encode(), time.Now()))
	h.Wait()
	inc, _ = store.GetIncident(t.Context(), inc.ID)
	if inc.Roles[core.RoleComms].Email != "ubob@example.com" {
		t.Fatalf("mention not resolved: %+v", inc.Roles)
	}

	// User errors come back ephemeral.
	form = url.Values{"text": {`sev 9`}, "user_id": {"UALICE"}, "channel_id": {"CWAR1"}, "response_url": {respSrv.URL + "/r"}}
	h.ServeCommand(httptest.NewRecorder(), sign(t, form.Encode(), time.Now()))
	h.Wait()
	last := replies[len(replies)-1]
	if last["response_type"] != "ephemeral" || !strings.Contains(last["text"].(string), "1-4") {
		t.Fatalf("error reply: %+v", last)
	}

	// Unsigned requests are refused.
	rec = httptest.NewRecorder()
	h.ServeCommand(rec, httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", rec.Code)
	}
}

type recordingDispatcher struct {
	mu              sync.Mutex
	actionID, value string
	actor           core.UserRef
}

func (d *recordingDispatcher) Execute(_ context.Context, _ core.CommandContext, _ core.Command) (core.Message, error) {
	return core.Message{}, nil
}
func (d *recordingDispatcher) HandleInteraction(_ context.Context, cc core.CommandContext, a, v string) (core.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.actionID, d.value, d.actor = a, v, cc.Actor
	return core.Message{Text: "approved"}, nil
}

func TestInteractivityStripsSuffixAndIdentifies(t *testing.T) {
	api, _ := fakeSlack(t)
	defer api.Close()
	respSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer respSrv.Close()
	d := &recordingDispatcher{}
	h := &slack.Handler{Client: slack.New("x", api.URL+"/api/", api.Client()), Service: d, SigningSecret: secret, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now, HTTP: respSrv.Client()}
	payload := `{"type":"block_actions","user":{"id":"UIC"},"channel":{"id":"CWAR1"},"response_url":"` + respSrv.URL + `","actions":[{"action_id":"approve_action:0","value":"req-1"}]}`
	h.ServeInteractivity(httptest.NewRecorder(), sign(t, url.Values{"payload": {payload}}.Encode(), time.Now()))
	h.Wait()
	if d.actionID != "approve_action" || d.value != "req-1" || d.actor.Email != "uic@example.com" {
		t.Fatalf("dispatch: %+v", d)
	}
}

func TestBlocksRendering(t *testing.T) {
	m := core.Message{Title: "T", Text: strings.Repeat("x\n", 2000), Fields: make([]core.Field, 12), Buttons: []core.Button{{ActionID: "approve_action", Label: "Approve", Value: "r1", Style: "primary"}, {ActionID: "deny_action", Label: "Deny", Value: "r1", Style: "danger"}}}
	for i := range m.Fields {
		m.Fields[i] = core.Field{Label: "L", Value: "V"}
	}
	blocks := slack.Blocks(m)
	b, _ := json.Marshal(blocks)
	s := string(b)
	if !strings.Contains(s, `"type":"header"`) || !strings.Contains(s, `"action_id":"approve_action:0"`) || !strings.Contains(s, `"style":"danger"`) {
		t.Fatalf("blocks: %s", s[:300])
	}
	// 4000 chars of text must be split into sections whose text is < 3000 chars.
	var generic []map[string]any
	_ = json.Unmarshal(b, &generic)
	sections := 0
	for _, blk := range generic {
		if blk["type"] != "section" {
			continue
		}
		if txt, ok := blk["text"].(map[string]any); ok {
			sections++
			if n := len(txt["text"].(string)); n > 3000 {
				t.Fatalf("section text %d chars exceeds Slack's 3000 limit", n)
			}
		}
	}
	if sections < 2 {
		t.Fatal("long text should be split into multiple sections")
	}
}
