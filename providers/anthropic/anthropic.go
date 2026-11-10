// Package anthropic implements core.LlmPort with the official Anthropic Go
// SDK (github.com/anthropics/anthropic-sdk-go/v2).
//
// It uses the beta Messages namespace so server-side refusal fallbacks
// ("fallbacks": "default", beta server-side-fallback-2026-07-01) can be
// enabled; set llm.fallbacks: off to disable. Assistant turns are replayed
// with resp.ToParam() so thinking blocks round-trip unchanged.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Config configures the adapter.
type Config struct {
	APIKey    string
	Model     string // default claude-opus-5-5
	Effort    string // low | medium | high | xhigh | max
	Fallbacks bool
	BaseURL   string // optional private endpoint / gateway
	HTTP      *http.Client
}

// Client is the Anthropic LlmPort.
type Client struct {
	cfg Config
	api sdk.Client
}

// New builds the adapter.
func New(cfg Config) *Client {
	if cfg.Model == "" {
		cfg.Model = "claude-opus-5-5"
	}
	if cfg.Effort == "" {
		cfg.Effort = "high"
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTP))
	}
	return &Client{cfg: cfg, api: sdk.NewClient(opts...)}
}

func (c *Client) Name() string { return "anthropic:" + c.cfg.Model }

// NewConversation starts a conversation with a fixed system prompt and tools.
func (c *Client) NewConversation(system string, tools []core.LlmTool, maxTokens int) core.LlmConversation {
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	var ts []sdk.BetaToolUnionParam
	for _, t := range tools {
		schema := sdk.BetaToolInputSchemaParam{Properties: t.InputSchema["properties"]}
		if req, ok := t.InputSchema["required"].([]string); ok {
			schema.Required = req
		}
		schema.ExtraFields = map[string]any{"additionalProperties": false}
		tp := sdk.BetaToolParam{Name: t.Name, Description: sdk.String(t.Description), InputSchema: schema}
		ts = append(ts, sdk.BetaToolUnionParam{OfTool: &tp})
	}
	return &conversation{c: c, system: system, tools: ts, maxTokens: int64(maxTokens)}
}

type conversation struct {
	c         *Client
	system    string
	tools     []sdk.BetaToolUnionParam
	maxTokens int64
	messages  []sdk.BetaMessageParam
}

func (cv *conversation) Send(ctx context.Context, userText string) (core.LlmTurn, error) {
	cv.messages = append(cv.messages, sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(userText)))
	return cv.call(ctx)
}

func (cv *conversation) SendToolResults(ctx context.Context, results []core.LlmToolResult) (core.LlmTurn, error) {
	blocks := make([]sdk.BetaContentBlockParamUnion, 0, len(results))
	for _, r := range results {
		blocks = append(blocks, sdk.NewBetaToolResultBlock(r.CallID, r.Content, r.IsError))
	}
	// All results go back in one user turn (preserves parallel tool use).
	cv.messages = append(cv.messages, sdk.NewBetaUserMessage(blocks...))
	return cv.call(ctx)
}

func (cv *conversation) call(ctx context.Context) (core.LlmTurn, error) {
	params := sdk.BetaMessageNewParams{
		Model:     sdk.Model(cv.c.cfg.Model),
		MaxTokens: cv.maxTokens,
		System: []sdk.BetaTextBlockParam{{
			Text:         cv.system,
			CacheControl: sdk.NewBetaCacheControlEphemeralParam(),
		}},
		Messages:     cv.messages,
		Tools:        cv.tools,
		OutputConfig: sdk.BetaOutputConfigParam{Effort: sdk.BetaOutputConfigEffort(cv.c.cfg.Effort)},
	}
	if cv.c.cfg.Fallbacks {
		params.Fallbacks = sdk.BetaFallbacksParamOfDefault()
		params.Betas = []sdk.AnthropicBeta{sdk.AnthropicBetaServerSideFallback2026_07_01}
	}
	resp, err := cv.c.api.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *sdk.Error
		if errors.As(err, &apiErr) {
			return core.LlmTurn{}, fmt.Errorf("anthropic API error (HTTP %d): %w", apiErr.StatusCode, err)
		}
		return core.LlmTurn{}, fmt.Errorf("anthropic request failed: %w", err)
	}
	// Replay the full assistant content (incl. thinking blocks) unchanged.
	cv.messages = append(cv.messages, resp.ToParam())

	turn := core.LlmTurn{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	switch resp.StopReason {
	case sdk.BetaStopReasonEndTurn, sdk.BetaStopReasonStopSequence:
		turn.StopReason = core.StopEndTurn
	case sdk.BetaStopReasonToolUse:
		turn.StopReason = core.StopToolUse
	case sdk.BetaStopReasonMaxTokens, sdk.BetaStopReasonModelContextWindowExceeded:
		turn.StopReason = core.StopMaxTokens
	case sdk.BetaStopReasonRefusal:
		turn.StopReason = core.StopRefusal
	default:
		turn.StopReason = core.StopOther
	}
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			turn.Text += b.Text
		case sdk.BetaToolUseBlock:
			turn.ToolCalls = append(turn.ToolCalls, core.LlmToolCall{ID: b.ID, Name: b.Name, Input: []byte(b.JSON.Input.Raw())})
		}
	}
	return turn, nil
}

var _ core.LlmPort = (*Client)(nil)
