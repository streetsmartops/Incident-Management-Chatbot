package teams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Dispatcher is the part of core.Service the handler needs.
type Dispatcher interface {
	Execute(ctx context.Context, cc core.CommandContext, cmd core.Command) (core.Message, error)
	HandleInteraction(ctx context.Context, cc core.CommandContext, actionID, value string) (core.Message, error)
}

// Activity is the subset of a Bot Framework activity we use.
type Activity struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	ServiceURL string `json:"serviceUrl"`
	ChannelID  string `json:"channelId"`
	From       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Recipient struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"recipient"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	Text     string          `json:"text"`
	Value    json.RawMessage `json:"value"`
	Entities []struct {
		Type      string `json:"type"`
		Mentioned struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"mentioned"`
	} `json:"entities"`
}

// Handler serves POST /teams/messages.
type Handler struct {
	Client    *Client
	Validator *Validator
	Service   Dispatcher
	Log       *slog.Logger
	Timeout   time.Duration
	wg        sync.WaitGroup
}

// Wait blocks until async replies finish.
func (h *Handler) Wait() { h.wg.Wait() }

var atTag = regexp.MustCompile(`(?s)<at>.*?</at>`)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var act Activity
	if err := json.Unmarshal(body, &act); err != nil {
		http.Error(w, "bad activity", http.StatusBadRequest)
		return
	}
	if err := h.Validator.Validate(r.Context(), r.Header.Get("Authorization"), act.ServiceURL, act.ChannelID); err != nil {
		h.Log.Warn("teams activity rejected", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
	// Logged so admins can find teams.announceConversationId / announceServiceUrl.
	h.Log.Info("teams activity", "type", act.Type, "conversation_id", act.Conversation.ID, "service_url", act.ServiceURL)
	if act.Type != "message" {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		timeout := h.Timeout
		if timeout == 0 {
			timeout = 60 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		h.handle(ctx, act)
	}()
}

func (h *Handler) handle(ctx context.Context, act Activity) {
	ch := core.ChannelRef{Platform: Platform, ID: act.Conversation.ID, ServiceURL: act.ServiceURL}
	cc := core.CommandContext{Actor: h.Client.Identify(ctx, act.ServiceURL, act.Conversation.ID, act.From.ID, act.From.Name), Channel: ch}
	var msg core.Message
	var err error
	if len(act.Value) > 0 && string(act.Value) != "null" {
		var v struct {
			ActionID string `json:"actionId"`
			Value    string `json:"value"`
		}
		if json.Unmarshal(act.Value, &v) != nil || v.ActionID == "" {
			return
		}
		msg, err = h.Service.HandleInteraction(ctx, cc, v.ActionID, v.Value)
	} else {
		for _, e := range act.Entities {
			if e.Type == "mention" && e.Mentioned.ID != act.Recipient.ID {
				cc.Mentions = append(cc.Mentions, h.Client.Identify(ctx, act.ServiceURL, act.Conversation.ID, e.Mentioned.ID, e.Mentioned.Name))
			}
		}
		text := strings.TrimSpace(atTag.ReplaceAllStringFunc(act.Text, func(tag string) string {
			if strings.Contains(tag, act.Recipient.Name) {
				return "" // the bot mention
			}
			return "@" + strings.TrimSuffix(strings.TrimPrefix(tag, "<at>"), "</at>")
		}))
		text = strings.TrimPrefix(strings.TrimSpace(text), "/incident")
		cmd, perr := core.ParseCommand(text)
		if perr != nil {
			err = &core.UserError{Msg: perr.Error()}
		} else {
			msg, err = h.Service.Execute(ctx, cc, cmd)
		}
	}
	if err != nil {
		var ue *core.UserError
		if errors.As(err, &ue) {
			msg = core.Message{Text: ue.Msg}
		} else {
			h.Log.Error("teams command failed", "err", err)
			msg = core.Message{Text: "Something went wrong; the error was logged."}
		}
	}
	if _, perr := h.Client.PostMessage(ctx, ch, msg); perr != nil {
		h.Log.Warn("teams reply failed", "err", perr)
	}
}
