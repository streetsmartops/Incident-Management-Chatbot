package pagerduty_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/pagerduty"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "pagerduty", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func signed(t *testing.T, body []byte, secret string) *http.Request {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/pagerduty", bytes.NewReader(body))
	r.Header.Set("X-PagerDuty-Signature", "v1=deadbeef,v1="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func resolver(_ context.Context, name string) (string, bool) {
	if name == "Checkout availability" {
		return "7", true
	}
	return "", false
}

func TestWebhookSignature(t *testing.T) {
	c := pagerduty.New(pagerduty.Config{WebhookSecret: "s3cret"}, nil)
	body := fixture(t, "incident-triggered-multiplexer.json")
	if _, err := c.VerifyAndNormalize(context.Background(), signed(t, body, "s3cret"), resolver); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if _, err := c.VerifyAndNormalize(context.Background(), signed(t, body, "wrong"), resolver); !errors.Is(err, pagerduty.ErrBadSignature) {
		t.Fatalf("bad signature accepted: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if _, err := c.VerifyAndNormalize(context.Background(), r, resolver); !errors.Is(err, pagerduty.ErrBadSignature) {
		t.Fatalf("missing signature accepted: %v", err)
	}
}

func TestNormalizeMultiplexerPageCorrelates(t *testing.T) {
	c := pagerduty.New(pagerduty.Config{Services: map[string]string{"checkout": "PCHK001"}}, nil)
	sig, err := c.Normalize(context.Background(), fixture(t, "incident-triggered-multiplexer.json"), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if sig.DedupKey != "multiplexer:guardrail:7" || sig.SloID != "7" || sig.Service != "checkout" || sig.SeverityHint != core.Sev2 || sig.Resolved {
		t.Fatalf("signal: %+v", sig)
	}
}

func TestNormalizeAckOfOwnPage(t *testing.T) {
	c := pagerduty.New(pagerduty.Config{}, nil)
	sig, err := c.Normalize(context.Background(), fixture(t, "incident-acknowledged-own-page.json"), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !sig.Acknowledged || sig.DedupKey != core.PageDedupKey("pagerduty", "PDOWN1") || sig.SeverityHint != core.Sev1 {
		t.Fatalf("signal: %+v", sig)
	}
}

func TestNormalizeSDKRecordedFixtureAndNonIncident(t *testing.T) {
	c := pagerduty.New(pagerduty.Config{}, nil)
	sig, err := c.Normalize(context.Background(), fixture(t, "incident-priority-updated-sdk.json"), nil)
	if err != nil || sig == nil || sig.Title != "A little bump in the road" || sig.SeverityHint != core.Sev1 {
		t.Fatalf("sdk fixture: %+v %v", sig, err)
	}
	sig, err = c.Normalize(context.Background(), fixture(t, "service-updated.json"), nil)
	if err != nil || sig != nil {
		t.Fatalf("non-incident events must be ignored: %+v %v", sig, err)
	}
}

type recorded struct {
	Method, Path, From, Auth string
	Body                     map[string]any
}

func fakeAPI(t *testing.T) (*httptest.Server, *[]recorded) {
	var mu sync.Mutex
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recorded{Method: r.Method, Path: r.URL.Path, From: r.Header.Get("From"), Auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(b, &rec.Body)
		mu.Lock()
		calls = append(calls, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/incidents":
			_, _ = w.Write([]byte(`{"incident":{"id":"PNEW1","type":"incident"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/incidents":
			_, _ = w.Write([]byte(`{"incidents":[{"id":"PNEW1","status":"resolved"}]}`))
		case strings.HasSuffix(r.URL.Path, "/notes"):
			_, _ = w.Write([]byte(`{"note":{"id":"N1","content":"x"}}`))
		case r.URL.Path == "/services/PCHK001":
			_, _ = w.Write([]byte(`{"service":{"id":"PCHK001","escalation_policy":{"id":"PEP1","type":"escalation_policy_reference"}}}`))
		case r.URL.Path == "/oncalls":
			if r.URL.Query().Get("escalation_policy_ids[]") != "PEP1" {
				t.Errorf("oncalls query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"oncalls":[{"escalation_level":1,"user":{"id":"PU1","name":"On Call","email":"oncall@example.com"}},{"escalation_level":2,"user":{"id":"PU2","email":"mgr@example.com"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return srv, &calls
}

func TestRESTCalls(t *testing.T) {
	srv, calls := fakeAPI(t)
	defer srv.Close()
	c := pagerduty.New(pagerduty.Config{APIURL: srv.URL, Token: "tok", FromEmail: "bot@example.com", Services: map[string]string{"checkout": "PCHK001"}}, srv.Client())
	ctx := context.Background()
	inc := &core.Incident{Key: "INC-2026-0001", Severity: core.Sev1, Title: "Checkout 500s", State: core.StateDeclared}
	ref, err := c.Page(ctx, "checkout", inc, "high")
	if err != nil || ref != "PNEW1" {
		t.Fatalf("page: %s %v", ref, err)
	}
	if err := c.AddNote(ctx, ref, "rolled back"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resolve(ctx, ref); err != nil {
		t.Fatal(err)
	}
	users, err := c.WhoIsOnCall(ctx, "checkout")
	if err != nil || len(users) != 1 || users[0].Email != "oncall@example.com" {
		t.Fatalf("oncall: %+v %v", users, err)
	}
	if _, err := c.Page(ctx, "unmapped", inc, "high"); err == nil {
		t.Fatal("unmapped service without default must error")
	}
	for _, rc := range (*calls)[:3] {
		if rc.From != "bot@example.com" {
			t.Errorf("%s %s missing From header", rc.Method, rc.Path)
		}
		if rc.Auth != "Token token=tok" {
			t.Errorf("auth header %q", rc.Auth)
		}
	}
	create := (*calls)[0].Body["incident"].(map[string]any)
	if create["urgency"] != "high" || create["service"].(map[string]any)["id"] != "PCHK001" || !strings.Contains(create["title"].(string), "INC-2026-0001") {
		t.Fatalf("create body: %+v", create)
	}
}
