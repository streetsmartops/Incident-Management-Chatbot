package jira_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/jira"
)

func TestJiraCreateAndComment(t *testing.T) {
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.com:tok"))
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rest/api/3/issue" {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"10000","key":"OPS-42","self":"https://x/rest/api/3/issue/10000"}`))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	c := jira.New(jira.Config{BaseURL: srv.URL + "/", Email: "bot@example.com", APIToken: "tok", ProjectKey: "OPS", IssueType: "Incident", ActionItemIssueType: "Task"}, srv.Client())
	ctx := context.Background()
	inc := &core.Incident{Key: "INC-2026-0001", Severity: core.Sev2, Title: "Checkout 500s", State: core.StateDeclared, Services: []string{"checkout"}}
	key, err := c.CreateIncidentTicket(ctx, inc)
	if err != nil || key != "OPS-42" {
		t.Fatalf("create: %s %v", key, err)
	}
	if err := c.Comment(ctx, key, "Mitigated"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateActionItem(ctx, inc, core.ActionItem{Title: "Add canary", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "POST /rest/api/3/issue" || paths[1] != "POST /rest/api/3/issue/OPS-42/comment" {
		t.Fatalf("paths %v", paths)
	}
	f := bodies[0]["fields"].(map[string]any)
	if f["project"].(map[string]any)["key"] != "OPS" || f["issuetype"].(map[string]any)["name"] != "Incident" {
		t.Fatalf("fields %+v", f)
	}
	if f["description"].(map[string]any)["type"] != "doc" {
		t.Fatal("description must be ADF")
	}
	if bodies[1]["body"].(map[string]any)["type"] != "doc" {
		t.Fatal("comment must be ADF")
	}
	if bodies[2]["fields"].(map[string]any)["issuetype"].(map[string]any)["name"] != "Task" {
		t.Fatal("action items use the action item issue type")
	}
}

func TestJiraErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"errorMessages":[],"errors":{"issuetype":"valid issue type is required"}}`))
	}))
	defer srv.Close()
	c := jira.New(jira.Config{BaseURL: srv.URL, ProjectKey: "OPS", IssueType: "Nope"}, srv.Client())
	if _, err := c.CreateIncidentTicket(context.Background(), &core.Incident{}); err == nil {
		t.Fatal("4xx must error")
	}
}
