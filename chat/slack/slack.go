// Package slack implements core.ChatPort and the HTTP handlers for Slack
// slash commands and interactivity.
//
// Verified against docs.slack.dev:
//   - Request signing: X-Slack-Signature = "v0=" + hex(HMAC-SHA256(signing
//     secret, "v0:" + X-Slack-Request-Timestamp + ":" + raw body)); reject if
//     the timestamp differs from local time by more than five minutes.
//   - Slash command form fields: command, text, user_id, channel_id,
//     response_url, trigger_id, team_id …; ack within 3 seconds.
//   - Interactivity: form field "payload" (JSON), type block_actions; ack
//     within 3 seconds; reply via response_url.
//   - Web API methods and bot scopes: conversations.create (channels:manage,
//     groups:write), conversations.invite (channels:write.invites),
//     conversations.setTopic (channels:write.topic), chat.postMessage /
//     chat.update (chat:write), users.lookupByEmail (users:read.email),
//     users.info (users:read).
package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Platform name.
const Platform = "slack"

// MaxSkew is the accepted request timestamp skew.
const MaxSkew = 5 * time.Minute

// ErrBadSignature is returned for unverifiable requests.
var ErrBadSignature = errors.New("slack signature verification failed")

// Verify checks a Slack request signature and returns the raw body.
func Verify(r *http.Request, signingSecret string, now time.Time) ([]byte, error) {
	if signingSecret == "" {
		return nil, fmt.Errorf("%w: signing secret not configured", ErrBadSignature)
	}
	tsHeader := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil || sig == "" {
		return nil, fmt.Errorf("%w: missing headers", ErrBadSignature)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
		return nil, fmt.Errorf("%w: timestamp outside %s window", ErrBadSignature, MaxSkew)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte("v0:" + tsHeader + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, ErrBadSignature
	}
	return body, nil
}

// Client implements core.ChatPort with the Slack Web API.
type Client struct {
	api *slack.Client

	mu     sync.Mutex
	emails map[string]cachedEmail
}

type cachedEmail struct {
	email string
	name  string
	at    time.Time
}

// New builds a client. apiURL overrides https://slack.com/api/ (tests).
func New(botToken, apiURL string, hc *http.Client) *Client {
	opts := []slack.Option{}
	if apiURL != "" {
		if !strings.HasSuffix(apiURL, "/") {
			apiURL += "/"
		}
		opts = append(opts, slack.OptionAPIURL(apiURL))
	}
	if hc != nil {
		opts = append(opts, slack.OptionHTTPClient(hc))
	}
	return &Client{api: slack.New(botToken, opts...), emails: map[string]cachedEmail{}}
}

func (c *Client) Platform() string { return Platform }

// Blocks renders a core.Message as Block Kit.
func Blocks(m core.Message) []slack.Block {
	var blocks []slack.Block
	if m.Title != "" {
		title := m.Title
		if len(title) > 150 {
			title = title[:147] + "…"
		}
		blocks = append(blocks, slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, title, false, false)))
	}
	if strings.TrimSpace(m.Text) != "" {
		for _, chunk := range chunks(m.Text, 2900) {
			blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, chunk, false, false), nil, nil))
		}
	}
	for i := 0; i < len(m.Fields); i += 10 {
		var fs []*slack.TextBlockObject
		for _, f := range m.Fields[i:min(i+10, len(m.Fields))] {
			v := f.Value
			if len(v) > 1900 {
				v = v[:1897] + "…"
			}
			fs = append(fs, slack.NewTextBlockObject(slack.MarkdownType, "*"+f.Label+"*\n"+v, false, false))
		}
		blocks = append(blocks, slack.NewSectionBlock(nil, fs, nil))
	}
	if len(m.Buttons) > 0 {
		var els []slack.BlockElement
		for _, b := range m.Buttons {
			btn := slack.NewButtonBlockElement(b.ActionID+":"+strconv.Itoa(len(els)), b.Value, slack.NewTextBlockObject(slack.PlainTextType, b.Label, false, false))
			switch b.Style {
			case "primary":
				btn.Style = slack.StylePrimary
			case "danger":
				btn.Style = slack.StyleDanger
			}
			els = append(els, btn)
		}
		blocks = append(blocks, slack.NewActionBlock("incidentd_actions", els...))
	}
	return blocks
}

func chunks(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut <= 0 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

func fallbackText(m core.Message) string {
	if m.Title != "" {
		return m.Title
	}
	if len(m.Text) > 200 {
		return m.Text[:200]
	}
	return m.Text
}

func (c *Client) PostMessage(ctx context.Context, ch core.ChannelRef, m core.Message) (core.MessageRef, error) {
	_, ts, err := c.api.PostMessageContext(ctx, ch.ID, slack.MsgOptionBlocks(Blocks(m)...), slack.MsgOptionText(fallbackText(m), false))
	if err != nil {
		return core.MessageRef{}, fmt.Errorf("slack chat.postMessage: %w", err)
	}
	return core.MessageRef{Channel: ch, ID: ts}, nil
}

func (c *Client) UpdateMessage(ctx context.Context, ref core.MessageRef, m core.Message) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, ref.Channel.ID, ref.ID, slack.MsgOptionBlocks(Blocks(m)...), slack.MsgOptionText(fallbackText(m), false))
	if err != nil {
		return fmt.Errorf("slack chat.update: %w", err)
	}
	return nil
}

func (c *Client) CreateWarRoom(ctx context.Context, name string, private bool) (core.ChannelRef, error) {
	ch, err := c.api.CreateConversationContext(ctx, slack.CreateConversationParams{ChannelName: name, IsPrivate: private})
	if err != nil {
		return core.ChannelRef{}, fmt.Errorf("slack conversations.create: %w", err)
	}
	return core.ChannelRef{Platform: Platform, ID: ch.ID, Name: ch.Name}, nil
}

func (c *Client) Invite(ctx context.Context, ch core.ChannelRef, users []core.UserRef) error {
	var ids []string
	for _, u := range users {
		if u.Platform == Platform && u.ID != "" {
			ids = append(ids, u.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := c.api.InviteUsersToConversationContext(ctx, ch.ID, ids...)
	if err != nil && !strings.Contains(err.Error(), "already_in_channel") {
		return fmt.Errorf("slack conversations.invite: %w", err)
	}
	return nil
}

func (c *Client) SetTopic(ctx context.Context, ch core.ChannelRef, topic string) error {
	if len(topic) > 250 {
		topic = topic[:250]
	}
	if _, err := c.api.SetTopicOfConversationContext(ctx, ch.ID, topic); err != nil {
		return fmt.Errorf("slack conversations.setTopic: %w", err)
	}
	return nil
}

func (c *Client) LookupUserByEmail(ctx context.Context, email string) (core.UserRef, error) {
	u, err := c.api.GetUserByEmailContext(ctx, email)
	if err != nil {
		return core.UserRef{}, fmt.Errorf("slack users.lookupByEmail: %w", err)
	}
	return core.UserRef{Platform: Platform, ID: u.ID, Email: u.Profile.Email, Name: u.RealName}, nil
}

// Identify resolves a Slack user ID to a UserRef with email (cached 1h).
func (c *Client) Identify(ctx context.Context, userID string) core.UserRef {
	c.mu.Lock()
	ce, ok := c.emails[userID]
	c.mu.Unlock()
	if ok && time.Since(ce.at) < time.Hour {
		return core.UserRef{Platform: Platform, ID: userID, Email: ce.email, Name: ce.name}
	}
	u, err := c.api.GetUserInfoContext(ctx, userID)
	if err != nil {
		return core.UserRef{Platform: Platform, ID: userID}
	}
	c.mu.Lock()
	c.emails[userID] = cachedEmail{u.Profile.Email, u.RealName, time.Now()}
	c.mu.Unlock()
	return core.UserRef{Platform: Platform, ID: userID, Email: u.Profile.Email, Name: u.RealName}
}

var _ core.ChatPort = (*Client)(nil)
