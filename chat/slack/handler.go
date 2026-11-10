package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Dispatcher is the part of core.Service the handlers need.
type Dispatcher interface {
	Execute(ctx context.Context, cc core.CommandContext, cmd core.Command) (core.Message, error)
	HandleInteraction(ctx context.Context, cc core.CommandContext, actionID, value string) (core.Message, error)
}

// Handler serves /slack/commands and /slack/interactivity.
type Handler struct {
	Client        *Client
	Service       Dispatcher
	SigningSecret string
	HTTP          *http.Client // for response_url posts
	Log           *slog.Logger
	Now           func() time.Time
	Timeout       time.Duration
	wg            sync.WaitGroup
}

// Wait blocks until in-flight async replies finish (tests, shutdown).
func (h *Handler) Wait() { h.wg.Wait() }

// With "Escape channels, users, and links" enabled, mentions arrive as <@U123|name> or <@U123>.
var mentionRe = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|([^>]*))?>`)

// ServeCommand handles slash commands. It acks immediately and replies via response_url.
func (h *Handler) ServeCommand(w http.ResponseWriter, r *http.Request) {
	body, err := Verify(r, h.SigningSecret, h.Now())
	if err != nil {
		h.Log.Warn("slack command rejected", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	text := form.Get("text")
	var mentionIDs []string
	text = mentionRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := mentionRe.FindStringSubmatch(m)
		mentionIDs = append(mentionIDs, sub[1])
		if sub[2] != "" {
			return "@" + sub[2]
		}
		return "@" + sub[1]
	})
	cmd, perr := core.ParseCommand(text)
	responseURL := form.Get("response_url")
	if perr != nil {
		writeJSON(w, map[string]any{"response_type": "ephemeral", "text": perr.Error()})
		return
	}
	writeJSON(w, map[string]any{"response_type": "ephemeral", "text": "Working on `" + cmd.Verb + "`…"})

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
		defer cancel()
		cc := core.CommandContext{
			Actor:   h.Client.Identify(ctx, form.Get("user_id")),
			Channel: core.ChannelRef{Platform: Platform, ID: form.Get("channel_id"), Name: form.Get("channel_name")},
		}
		for _, id := range mentionIDs {
			cc.Mentions = append(cc.Mentions, h.Client.Identify(ctx, id))
		}
		msg, err := h.Service.Execute(ctx, cc, cmd)
		h.reply(ctx, responseURL, msg, err)
	}()
}

func (h *Handler) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 60 * time.Second
}

type blockActions struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// ServeInteractivity handles block_actions (button clicks).
func (h *Handler) ServeInteractivity(w http.ResponseWriter, r *http.Request) {
	body, err := Verify(r, h.SigningSecret, h.Now())
	if err != nil {
		h.Log.Warn("slack interaction rejected", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var p blockActions
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil || p.Type != "block_actions" || len(p.Actions) == 0 {
		w.WriteHeader(http.StatusOK) // ignore other interaction types
		return
	}
	w.WriteHeader(http.StatusOK)
	act := p.Actions[0]
	actionID, _, _ := strings.Cut(act.ActionID, ":") // strip the uniqueness suffix
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
		defer cancel()
		cc := core.CommandContext{Actor: h.Client.Identify(ctx, p.User.ID), Channel: core.ChannelRef{Platform: Platform, ID: p.Channel.ID, Name: p.Channel.Name}}
		msg, err := h.Service.HandleInteraction(ctx, cc, actionID, act.Value)
		if err == nil {
			msg.Ephemeral = false // approvals/denials are visible to the channel
		}
		h.reply(ctx, p.ResponseURL, msg, err)
	}()
}

func (h *Handler) reply(ctx context.Context, responseURL string, msg core.Message, err error) {
	if err != nil {
		var ue *core.UserError
		if errors.As(err, &ue) {
			msg = core.Message{Text: ue.Msg, Ephemeral: true}
		} else {
			h.Log.Error("command failed", "err", err)
			msg = core.Message{Text: "Something went wrong; the error was logged. (" + err.Error() + ")", Ephemeral: true}
		}
	}
	if responseURL == "" {
		return
	}
	rt := "in_channel"
	if msg.Ephemeral {
		rt = "ephemeral"
	}
	payload := map[string]any{"response_type": rt, "replace_original": false, "text": fallbackText(msg), "blocks": Blocks(msg)}
	b, _ := json.Marshal(payload)
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(b))
	if rerr != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	hc := h.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, rerr := hc.Do(req)
	if rerr != nil {
		h.Log.Warn("response_url post failed", "err", rerr)
		return
	}
	resp.Body.Close()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
