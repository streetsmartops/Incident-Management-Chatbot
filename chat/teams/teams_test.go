package teams_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/streetsmartops/incident-management-chatbot/chat/teams"
	"github.com/streetsmartops/incident-management-chatbot/core"
)

type idp struct {
	srv  *httptest.Server
	key  *rsa.PrivateKey
	endo []string
}

func newIDP(t *testing.T, endorsements []string) *idp {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &idp{key: k, endo: endorsements}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": teams.BotFrameworkIssuer, "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(k.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e), "endorsements": p.endo,
		}}})
	})
	return p
}

func (p *idp) token(t *testing.T, claims jwt.MapClaims) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + s
}

func claims(serviceURL string) jwt.MapClaims {
	return jwt.MapClaims{"iss": teams.BotFrameworkIssuer, "aud": "app-id", "serviceUrl": serviceURL,
		"exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix()}
}

func TestValidator(t *testing.T) {
	p := newIDP(t, []string{"msteams"})
	defer p.srv.Close()
	v := teams.NewValidator("app-id", p.srv.Client())
	v.MetadataURL = p.srv.URL + "/meta"
	ctx := context.Background()
	su := "https://smba.trafficmanager.net/emea/"
	if err := v.Validate(ctx, p.token(t, claims(su)), su, "msteams"); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	bad := map[string]func(jwt.MapClaims){
		"wrong audience": func(c jwt.MapClaims) { c["aud"] = "other" },
		"wrong issuer":   func(c jwt.MapClaims) { c["iss"] = "https://evil" },
		"expired":        func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() },
		"serviceUrl":     func(c jwt.MapClaims) { c["serviceUrl"] = "https://evil.example.com/" },
	}
	for name, mut := range bad {
		c := claims(su)
		mut(c)
		if err := v.Validate(ctx, p.token(t, c), su, "msteams"); !errors.Is(err, teams.ErrUnauthorized) {
			t.Errorf("%s must be rejected, got %v", name, err)
		}
	}
	if err := v.Validate(ctx, p.token(t, claims(su)), su, "skype"); !errors.Is(err, teams.ErrUnauthorized) {
		t.Error("unendorsed channel must be rejected")
	}
	if err := v.Validate(ctx, "", su, "msteams"); !errors.Is(err, teams.ErrUnauthorized) {
		t.Error("missing token must be rejected")
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, claims(su))
	forged.Header["kid"] = "k1"
	s, _ := forged.SignedString(other)
	if err := v.Validate(ctx, "Bearer "+s, su, "msteams"); !errors.Is(err, teams.ErrUnauthorized) {
		t.Error("token signed by another key must be rejected")
	}
}

type rec struct {
	mu       sync.Mutex
	posts    []map[string]any
	paths    []string
	text     string
	actor    core.UserRef
	mentions []core.UserRef
	action   string
}

func (r *rec) Execute(_ context.Context, cc core.CommandContext, cmd core.Command) (core.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text, r.actor, r.mentions = cmd.Verb+" "+strings.Join(cmd.Args, " "), cc.Actor, cc.Mentions
	return core.Message{Title: "ok", Buttons: []core.Button{{ActionID: "approve_action", Label: "Approve", Value: "r1", Style: "primary"}}}, nil
}
func (r *rec) HandleInteraction(_ context.Context, cc core.CommandContext, a, v string) (core.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.action, r.actor = a+"="+v, cc.Actor
	return core.Message{Text: "approved"}, nil
}

func TestHandlerEndToEnd(t *testing.T) {
	p := newIDP(t, nil)
	defer p.srv.Close()
	r := &rec{}
	connector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.paths = append(r.paths, req.Method+" "+req.URL.Path)
		switch {
		case req.URL.Path == "/token":
			_ = req.ParseForm()
			if req.Form.Get("scope") != "https://api.botframework.com/.default" || req.Form.Get("grant_type") != "client_credentials" {
				t.Errorf("token form: %v", req.Form)
			}
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		case strings.Contains(req.URL.Path, "/members/"):
			_, _ = w.Write([]byte(`{"id":"29:alice","name":"Alice","userPrincipalName":"alice@example.com"}`))
		case strings.HasSuffix(req.URL.Path, "/activities"):
			if req.Header.Get("Authorization") != "Bearer tok" {
				t.Error("outbound call without bot token")
			}
			b, _ := io.ReadAll(req.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			r.posts = append(r.posts, m)
			_, _ = w.Write([]byte(`{"id":"act-1"}`))
		}
	}))
	defer connector.Close()

	client := teams.New("app-id", "pw", "", connector.Client())
	client.TokenURL = connector.URL + "/token"
	v := teams.NewValidator("app-id", p.srv.Client())
	v.MetadataURL = p.srv.URL + "/meta"
	h := &teams.Handler{Client: client, Validator: v, Service: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	act := map[string]any{
		"type": "message", "id": "1", "serviceUrl": connector.URL, "channelId": "msteams",
		"from": map[string]string{"id": "29:alice", "name": "Alice"}, "recipient": map[string]string{"id": "28:bot", "name": "IncidentBot"},
		"conversation": map[string]string{"id": "19:conv"},
		"text":         `<at>IncidentBot</at> role comms <at>Bob</at>`,
		"entities":     []any{map[string]any{"type": "mention", "mentioned": map[string]string{"id": "28:bot", "name": "IncidentBot"}}, map[string]any{"type": "mention", "mentioned": map[string]string{"id": "29:bob", "name": "Bob"}}},
	}
	b, _ := json.Marshal(act)
	req := httptest.NewRequest(http.MethodPost, "/teams/messages", strings.NewReader(string(b)))
	req.Header.Set("Authorization", p.token(t, claims(connector.URL)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	h.Wait()
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if r.text != "role comms @Bob" || r.actor.Email != "alice@example.com" || len(r.mentions) != 1 || r.mentions[0].ID != "29:bob" {
		t.Fatalf("dispatch: text=%q actor=%+v mentions=%+v", r.text, r.actor, r.mentions)
	}
	att := r.posts[0]["attachments"].([]any)[0].(map[string]any)
	if att["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("attachment: %+v", att)
	}
	actions := att["content"].(map[string]any)["actions"].([]any)
	if actions[0].(map[string]any)["data"].(map[string]any)["actionId"] != "approve_action" {
		t.Fatalf("card actions: %+v", actions)
	}

	// Card button submit.
	act["text"] = ""
	act["value"] = map[string]string{"actionId": "approve_action", "value": "r1"}
	b, _ = json.Marshal(act)
	req = httptest.NewRequest(http.MethodPost, "/teams/messages", strings.NewReader(string(b)))
	req.Header.Set("Authorization", p.token(t, claims(connector.URL)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.Wait()
	if r.action != "approve_action=r1" {
		t.Fatalf("interaction: %q", r.action)
	}

	// Unauthenticated activity is rejected.
	req = httptest.NewRequest(http.MethodPost, "/teams/messages", strings.NewReader(string(b)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
}

func TestUnsupportedOps(t *testing.T) {
	c := teams.New("a", "b", "tenant-1", nil)
	if !strings.Contains(c.TokenURL, "/tenant-1/") {
		t.Fatal("single-tenant token URL")
	}
	if _, err := c.CreateWarRoom(context.Background(), "x", false); !errors.Is(err, core.ErrNotSupported) {
		t.Fatal("war room creation needs Graph; must be ErrNotSupported")
	}
}
