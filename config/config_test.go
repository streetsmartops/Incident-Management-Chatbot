package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/streetsmartops/incident-management-chatbot/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestExampleConfigLoadsWithSecrets(t *testing.T) {
	b, err := os.ReadFile("example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(b, env(map[string]string{
		"DATABASE_URL": "postgres://x", "SLACK_BOT_TOKEN": "xoxb-1", "SLACK_SIGNING_SECRET": "s",
	}))
	if err != nil {
		t.Fatalf("example config should load: %v", err)
	}
	if c.LLM.Model != "claude-opus-5-5" || c.Database.Schema != "incident_bot" || c.Incident.PrivateByDefault {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Secret("SLACK_BOT_TOKEN") != "xoxb-1" {
		t.Fatal("secret not loaded")
	}
}

func TestMissingSecretsAreAllReported(t *testing.T) {
	yaml := `
slack: {enabled: true}
pagerduty: {enabled: true, fromEmail: bot@example.com}
jira: {enabled: true, baseUrl: "https://x.atlassian.net", email: a@b.c, projectKey: OPS}
llm: {enabled: true}
`
	_, err := config.Parse([]byte(yaml), env(nil))
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"DATABASE_URL", "SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET", "PAGERDUTY_API_TOKEN", "PAGERDUTY_WEBHOOK_SECRET", "JIRA_API_TOKEN", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s:\n%v", want, err)
		}
	}
}

func TestSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	_ = os.WriteFile(p, []byte("xoxb-file\n"), 0o600)
	c, err := config.Parse([]byte("database: {driver: memory}\nslack: {enabled: true}\n"), env(map[string]string{
		"SLACK_BOT_TOKEN_FILE": p, "SLACK_SIGNING_SECRET": "s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Secret("SLACK_BOT_TOKEN") != "xoxb-file" {
		t.Fatalf("got %q", c.Secret("SLACK_BOT_TOKEN"))
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := config.Parse([]byte("database: {driver: memory}\nslack: {enabled: true, botToken: oops}\n"), env(map[string]string{"SLACK_BOT_TOKEN": "x", "SLACK_SIGNING_SECRET": "y"}))
	if err == nil || !strings.Contains(err.Error(), "botToken") {
		t.Fatalf("secrets must not be accepted in YAML; got %v", err)
	}
}

func TestKubernetesRequiresNamespaceAllowList(t *testing.T) {
	yaml := "database: {driver: memory}\nslack: {enabled: true}\nkubernetes: {enabled: true, clusters: [{name: prod, inCluster: true}]}\n"
	_, err := config.Parse([]byte(yaml), env(map[string]string{"SLACK_BOT_TOKEN": "x", "SLACK_SIGNING_SECRET": "y"}))
	if err == nil || !strings.Contains(err.Error(), "namespaces") {
		t.Fatalf("got %v", err)
	}
}

func TestRemediationKillSwitchEnvDefaultsOff(t *testing.T) {
	c, err := config.Parse([]byte("database: {driver: memory}\nslack: {enabled: true}\n"), env(map[string]string{"SLACK_BOT_TOKEN": "x", "SLACK_SIGNING_SECRET": "y"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RemediationEnvEnabled() {
		t.Fatal("remediation must default off")
	}
}
