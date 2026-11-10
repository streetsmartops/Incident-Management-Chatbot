package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDemoFlow drives the demo console's API end to end (no browser) so the
// recorded walkthrough keeps working as the code changes.
func TestDemoFlow(t *testing.T) {
	a, err := newApp("../../remediation/catalog", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	post := func(path string, body any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, err, resp)
		}
	}
	ctx := context.Background()
	post("/api/simulate/breach", nil)
	for i := 0; i < 2; i++ {
		if err := a.poller.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	chans, _ := a.chat.snapshot()
	if len(chans) != 2 {
		t.Fatalf("expected a war room, got %+v", chans)
	}
	room := chans[1].ID
	cmd := func(user, text string) {
		post("/api/command", map[string]string{"user": user, "channel": room, "text": text})
	}
	last := func() string {
		_, msgs := a.chat.snapshot()
		m := msgs[len(msgs)-1]
		return m.Msg.Title + " " + m.Msg.Text
	}
	cmd("alice", "role ic me")
	cmd("alice", "triage")
	a.svc.Wait()
	_, msgs := a.chat.snapshot()
	var reqValue string
	for _, m := range msgs {
		for _, b := range m.Msg.Buttons {
			if b.ActionID == "request_action" {
				reqValue = b.Value
			}
		}
	}
	if reqValue == "" {
		t.Fatal("triage did not offer a catalog action")
	}
	if !strings.Contains(last(), "1 uncited hypothesis") {
		t.Fatalf("triage guardrails not visible: %s", last())
	}
	post("/api/action", map[string]string{"user": "bob", "channel": room, "actionId": "request_action", "value": reqValue})
	_, msgs = a.chat.snapshot()
	var approveID string
	for _, b := range msgs[len(msgs)-1].Msg.Buttons {
		if b.ActionID == "approve_action" {
			approveID = b.Value
		}
	}
	approve := func(user string) {
		post("/api/action", map[string]string{"user": user, "channel": room, "actionId": "approve_action", "value": approveID})
	}
	approve("bob")
	if !strings.Contains(last(), "requester cannot approve") {
		t.Fatalf("requester approval must be refused: %s", last())
	}
	approve("alice")
	approve("carol")
	if !strings.Contains(last(), "Executor is disabled") {
		t.Fatalf("expected dry-run only: %s", last())
	}
	for _, c := range []string{"status mitigating", "status monitoring", `resolve "done"`} {
		cmd("alice", c)
	}
	// SLO is still breached: polling must not re-declare.
	for i := 0; i < 3; i++ {
		_ = a.poller.Poll(ctx)
	}
	if chans, _ := a.chat.snapshot(); len(chans) != 2 {
		t.Fatalf("re-declared after resolve: %d channels", len(chans))
	}
	cmd("alice", "postmortem draft")
	a.svc.Wait()
	if !strings.Contains(last(), "Postmortem draft") {
		t.Fatalf("postmortem: %s", last())
	}
}
