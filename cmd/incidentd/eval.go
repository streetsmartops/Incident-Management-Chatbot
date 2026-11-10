package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pdapi "github.com/PagerDuty/go-pagerduty"

	"github.com/streetsmartops/incident-management-chatbot/core"
	anthropicllm "github.com/streetsmartops/incident-management-chatbot/providers/anthropic"
	"github.com/streetsmartops/incident-management-chatbot/remediation"
	"github.com/streetsmartops/incident-management-chatbot/triage"
)

// MaxHistory is the hard cap on historical import (Q12: 3 months).
const MaxHistory = 90 * 24 * time.Hour

// Fixture is one evaluation incident.
type Fixture struct {
	ID          string                         `json:"id"`
	Source      string                         `json:"source"` // synthetic | historical
	Title       string                         `json:"title"`
	Severity    int                            `json:"severity"`
	Services    []string                       `json:"services"`
	DetectedAt  time.Time                      `json:"detectedAt"`
	Context     string                         `json:"context"`
	Recordings  map[string][]triage.ReplayRule `json:"recordings"`
	GroundTruth GroundTruth                    `json:"groundTruth"`
}

// GroundTruth is the human-labelled answer.
type GroundTruth struct {
	RootCause string `json:"rootCause"`
	// Keywords: every group must match; a group matches if any term appears.
	Keywords      [][]string `json:"keywords"`
	RelevantTools []string   `json:"relevantTools"`
	NeedsLabel    bool       `json:"needsLabel"`
}

// EvalRecord is one JSONL output line consumed by eval/score.py.
type EvalRecord struct {
	ID          string            `json:"id"`
	Source      string            `json:"source"`
	GroundTruth GroundTruth       `json:"groundTruth"`
	Result      *triage.Result    `json:"result,omitempty"`
	Stats       *triage.Stats     `json:"stats,omitempty"`
	Calls       []triage.CallInfo `json:"calls,omitempty"`
	Error       string            `json:"error,omitempty"`
	DurationMs  int64             `json:"durationMs"`
}

func runEval(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: incidentd eval run|import-pagerduty [flags]")
	}
	switch args[0] {
	case "run":
		return evalRun(args[1:])
	case "import-pagerduty":
		return evalImportPagerDuty(args[1:])
	}
	return fmt.Errorf("unknown eval command %q", args[0])
}

func loadFixtures(dir string) ([]Fixture, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Fixture
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var fx Fixture
		if err := json.Unmarshal(b, &fx); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, fx)
	}
	return out, nil
}

func evalRun(args []string) error {
	fs := flag.NewFlagSet("eval run", flag.ExitOnError)
	dir := fs.String("fixtures", "test/fixtures/incidents", "fixture directory")
	out := fs.String("out", "eval/out/run.jsonl", "output JSONL")
	catalogDir := fs.String("catalog", "remediation/catalog", "remediation catalog")
	model := fs.String("model", "claude-opus-5-5", "model")
	effort := fs.String("effort", "high", "effort")
	maxCalls := fs.Int("max-tool-calls", 15, "tool budget")
	timeout := fs.Duration("timeout", 90*time.Second, "per-incident timeout")
	limit := fs.Int("limit", 0, "run only the first N fixtures (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return errors.New("ANTHROPIC_API_KEY is required for eval run (each run is billed)")
	}
	fixtures, err := loadFixtures(*dir)
	if err != nil {
		return err
	}
	if *limit > 0 && *limit < len(fixtures) {
		fixtures = fixtures[:*limit]
	}
	catalog, err := remediation.LoadCatalog(*catalogDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	llm := anthropicllm.New(anthropicllm.Config{APIKey: key, Model: *model, Effort: *effort, Fallbacks: true})
	redactor := core.MustRedactor(true)
	for _, fx := range fixtures {
		if fx.GroundTruth.NeedsLabel {
			fmt.Fprintf(os.Stderr, "skip %s: needs a human ground-truth label\n", fx.ID)
			continue
		}
		rec := evalOne(context.Background(), llm, catalog, redactor, fx, *maxCalls, *timeout)
		b, _ := json.Marshal(rec)
		fmt.Fprintln(w, string(b))
		fmt.Fprintf(os.Stderr, "%s: %s\n", fx.ID, status(rec))
	}
	fmt.Fprintf(os.Stderr, "wrote %s; score with: python3 eval/score.py %s\n", *out, *out)
	return nil
}

func status(r EvalRecord) string {
	if r.Error != "" {
		return "error: " + r.Error
	}
	if r.Result != nil && len(r.Result.Hypotheses) > 0 {
		return "top: " + r.Result.Hypotheses[0].Cause
	}
	return "no hypothesis"
}

func evalOne(ctx context.Context, llm core.LlmPort, catalog triage.Catalog, redactor *core.Redactor, fx Fixture, maxCalls int, timeout time.Duration) EvalRecord {
	inc := &core.Incident{ID: fx.ID, Key: strings.ToUpper(fx.ID), Title: fx.Title, Severity: core.Severity(fx.Severity),
		State: core.StateInvestigating, Services: fx.Services, DetectedAt: fx.DetectedAt}
	eng := &triage.Engine{LLM: llm, Catalog: catalog, MaxToolCalls: maxCalls, Timeout: timeout, MaxOutputBytes: 8192, MaxTokens: 16000, Now: time.Now,
		Exec: &triage.ReplayExecutor{Recordings: fx.Recordings, Context: fx.Context, Redactor: redactor}}
	start := time.Now()
	run, err := eng.Run(ctx, inc, "")
	rec := EvalRecord{ID: fx.ID, Source: fx.Source, GroundTruth: fx.GroundTruth, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	rec.Result, rec.Stats, rec.Calls = &run.Result, &run.Stats, run.Calls
	return rec
}

// evalImportPagerDuty writes historical fixture skeletons for PagerDuty
// incidents in the last --since (max 90 days). A human must fill in
// groundTruth (rootCause, keywords) and recordings before they are scored.
func evalImportPagerDuty(args []string) error {
	fs := flag.NewFlagSet("eval import-pagerduty", flag.ExitOnError)
	since := fs.Duration("since", MaxHistory, "how far back (max 2160h = 90 days)")
	out := fs.String("out", "test/fixtures/historical", "output directory (keep out of git if it contains customer data)")
	apiURL := fs.String("api-url", "https://api.pagerduty.com", "PagerDuty API URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since > MaxHistory {
		return fmt.Errorf("--since %s exceeds the 90-day historical limit", *since)
	}
	token := os.Getenv("PAGERDUTY_API_TOKEN")
	if token == "" {
		return errors.New("PAGERDUTY_API_TOKEN is required")
	}
	client := pdapi.NewClient(token, pdapi.WithAPIEndpoint(*apiURL))
	now := time.Now().UTC()
	redactor := core.MustRedactor(true)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	n, offset := 0, uint(0)
	for {
		resp, err := client.ListIncidentsWithContext(context.Background(), pdapi.ListIncidentsOptions{
			Since: now.Add(-*since).Format(time.RFC3339), Until: now.Format(time.RFC3339), Statuses: []string{"resolved"},
			Limit: 100, Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("pagerduty list incidents: %w", err)
		}
		for _, in := range resp.Incidents {
			created, _ := time.Parse(time.RFC3339, in.CreatedAt)
			fx := Fixture{
				ID: "pd-" + in.ID, Source: "historical", Title: redactor.Redact(in.Title), Severity: 2,
				Services: []string{in.Service.Summary}, DetectedAt: created,
				Context:     fmt.Sprintf(`{"source":"pagerduty","incident":%q,"urgency":%q}`, in.ID, in.Urgency),
				Recordings:  map[string][]triage.ReplayRule{},
				GroundTruth: GroundTruth{NeedsLabel: true},
			}
			b, _ := json.MarshalIndent(fx, "", "  ")
			if err := os.WriteFile(filepath.Join(*out, fx.ID+".json"), b, 0o600); err != nil {
				return err
			}
			n++
		}
		if !resp.More {
			break
		}
		offset += 100
	}
	fmt.Fprintf(os.Stderr, "wrote %d skeletons to %s; label groundTruth and add recordings, then set needsLabel=false\n", n, *out)
	return nil
}
