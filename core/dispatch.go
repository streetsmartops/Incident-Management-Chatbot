package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CommandContext carries who ran a command and where.
type CommandContext struct {
	Actor   UserRef
	Channel ChannelRef
	// Mentions are users @-mentioned in the command, resolved by the adapter
	// in order of appearance.
	Mentions []UserRef
}

// UserError is a message safe to show the user verbatim.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// Execute runs a parsed command and returns the reply to show the invoker.
// Long-running work (triage, postmortem drafts) runs asynchronously and
// posts its result to the incident channel.
func (s *Service) Execute(ctx context.Context, cc CommandContext, cmd Command) (Message, error) {
	if cmd.Verb == "help" {
		return Message{Text: HelpText, Ephemeral: true}, nil
	}
	if cmd.Verb == "declare" {
		return s.cmdDeclare(ctx, cc, cmd)
	}
	inc, err := s.resolveIncident(ctx, cc, cmd)
	if err != nil {
		return Message{}, err
	}
	unlock := s.lock(inc.ID)
	// Re-read under the lock so concurrent commands see fresh state.
	inc, err = s.store.GetIncident(ctx, inc.ID)
	if err != nil {
		unlock()
		return Message{}, err
	}
	defer unlock()

	switch cmd.Verb {
	case "sev":
		return s.cmdSev(ctx, cc, cmd, inc)
	case "role":
		return s.cmdRole(ctx, cc, cmd, inc)
	case "status":
		return s.cmdStatus(ctx, cc, cmd, inc)
	case "update":
		return s.cmdUpdate(ctx, cc, cmd, inc)
	case "triage":
		return s.cmdTriage(ctx, cc, cmd, inc)
	case "run":
		return s.cmdRun(ctx, cc, cmd, inc)
	case "resolve":
		return s.cmdResolve(ctx, cc, cmd, inc)
	case "postmortem":
		return s.cmdPostmortem(ctx, cc, cmd, inc)
	case "timeline":
		return s.cmdTimeline(ctx, cmd, inc)
	}
	return Message{}, userErr("unknown command %q", cmd.Verb)
}

func (s *Service) resolveIncident(ctx context.Context, cc CommandContext, cmd Command) (*Incident, error) {
	if key := cmd.Flag("incident", ""); key != "" {
		inc, err := s.store.FindIncidentByKey(ctx, strings.ToUpper(key))
		if errors.Is(err, ErrNotFound) {
			return nil, userErr("no incident %s", key)
		}
		return inc, err
	}
	inc, err := s.store.FindIncidentByChannel(ctx, cc.Channel)
	if errors.Is(err, ErrNotFound) {
		return nil, userErr("run this in an incident war room or pass `--incident INC-YYYY-NNNN`")
	}
	return inc, err
}

func (s *Service) isAdmin(u UserRef) bool {
	return u.Email != "" && slices.ContainsFunc(s.cfg.Admins, func(a string) bool { return strings.EqualFold(a, u.Email) })
}

func sameUser(a, b UserRef) bool {
	if a.Email != "" && b.Email != "" {
		return strings.EqualFold(a.Email, b.Email)
	}
	return a.Platform == b.Platform && a.ID == b.ID
}

func (s *Service) requireIC(inc *Incident, actor UserRef) error {
	if ic, ok := inc.IC(); ok && sameUser(ic, actor) {
		return nil
	}
	if s.isAdmin(actor) {
		return nil
	}
	if _, ok := inc.IC(); !ok {
		return userErr("no IC assigned; take it with `role ic me`")
	}
	return userErr("only the IC can do that")
}

func (s *Service) save(ctx context.Context, inc *Incident) error {
	return s.store.UpdateIncident(ctx, inc)
}

func (s *Service) refreshTopic(ctx context.Context, inc *Incident) {
	if inc.WarRoom == nil {
		return
	}
	if chat, ok := s.ints.Chats[inc.WarRoom.Platform]; ok {
		if err := chat.SetTopic(ctx, *inc.WarRoom, fmt.Sprintf("%s %s | %s | state: %s", inc.Key, inc.Severity, inc.Title, inc.State)); err != nil {
			s.recordErr(ctx, inc, "set_topic", err)
		}
	}
}

func (s *Service) cmdDeclare(ctx context.Context, cc CommandContext, cmd Command) (Message, error) {
	title := strings.TrimSpace(strings.Join(cmd.Args, " "))
	if title == "" {
		return Message{}, userErr("usage: `declare \"<title>\" [--sev 1-4] [--service S] [--private]`")
	}
	sev := Sev3
	if v := cmd.Flag("sev", ""); v != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(v), "sev"))
		if err != nil || !Severity(n).Valid() {
			return Message{}, userErr("severity must be 1-4")
		}
		sev = Severity(n)
	}
	var services []string
	if v := cmd.Flag("service", ""); v != "" {
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				services = append(services, x)
			}
		}
	}
	var private *bool
	if cmd.Flag("private", "") == "true" {
		t := true
		private = &t
	}
	inc, err := s.Declare(ctx, DeclareRequest{Title: title, Severity: sev, Services: services, Actor: cc.Actor, Platform: cc.Actor.Platform, Private: private})
	if err != nil {
		return Message{}, err
	}
	where := ""
	if inc.WarRoom != nil {
		where = " War room: #" + inc.WarRoom.Name
	}
	return Message{Text: fmt.Sprintf("Declared *%s* (%s). You are IC.%s", inc.Key, inc.Severity, where)}, nil
}

func (s *Service) cmdSev(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if err := s.requireIC(inc, cc.Actor); err != nil {
		return Message{}, err
	}
	if len(cmd.Args) != 1 {
		return Message{}, userErr("usage: `sev <1-4> [--reason \"…\"]`")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(cmd.Args[0]), "sev"))
	if err != nil || !Severity(n).Valid() {
		return Message{}, userErr("severity must be 1-4")
	}
	old := inc.Severity
	inc.Severity = Severity(n)
	if _, err := s.Record(ctx, inc.ID, HumanActor(cc.Actor), EvSevChanged, map[string]string{"from": old.String(), "to": inc.Severity.String(), "reason": cmd.Flag("reason", "")}); err != nil {
		return Message{}, err
	}
	if len(inc.PageRefs) == 0 {
		s.page(ctx, inc) // escalation into paging range
	}
	if inc.TicketRef == "" {
		s.fileTicket(ctx, inc)
	}
	if err := s.save(ctx, inc); err != nil {
		return Message{}, err
	}
	s.refreshTopic(ctx, inc)
	return Message{Text: fmt.Sprintf("Severity %s → *%s* by %s", old, inc.Severity, displayUser(cc.Actor))}, nil
}

func (s *Service) cmdRole(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if len(cmd.Args) < 1 {
		return Message{}, userErr("usage: `role <ic|comms|ops|scribe> @user|me`")
	}
	role, ok := ParseRole(strings.ToLower(cmd.Args[0]))
	if !ok {
		return Message{}, userErr("role must be one of ic, comms, ops, scribe")
	}
	var target UserRef
	switch {
	case len(cc.Mentions) > 0:
		target = cc.Mentions[0]
	case len(cmd.Args) > 1 && strings.EqualFold(cmd.Args[1], "me"):
		target = cc.Actor
	default:
		return Message{}, userErr("mention a user or use `me`")
	}
	_, hasIC := inc.IC()
	selfTakingVacantIC := role == RoleIC && !hasIC && sameUser(target, cc.Actor)
	if !selfTakingVacantIC {
		if err := s.requireIC(inc, cc.Actor); err != nil {
			return Message{}, err
		}
	}
	inc.Roles[role] = target
	if _, err := s.Record(ctx, inc.ID, HumanActor(cc.Actor), EvRoleAssigned, map[string]any{"role": role, "user": target}); err != nil {
		return Message{}, err
	}
	if err := s.save(ctx, inc); err != nil {
		return Message{}, err
	}
	if inc.WarRoom != nil && target.Platform == inc.WarRoom.Platform {
		if chat, ok := s.ints.Chats[target.Platform]; ok {
			_ = chat.Invite(ctx, *inc.WarRoom, []UserRef{target})
		}
	}
	return Message{Text: fmt.Sprintf("%s is now *%s*", displayUser(target), strings.ToUpper(string(role)))}, nil
}

func (s *Service) cmdStatus(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if len(cmd.Args) == 0 {
		m := s.summary(inc)
		allowed := AllowedTransitions(inc.State)
		names := make([]string, len(allowed))
		for i, a := range allowed {
			names[i] = string(a)
		}
		m.Text = "Next states: " + strings.Join(names, ", ")
		m.Ephemeral = true
		return m, nil
	}
	to, ok := ParseState(cmd.Args[0])
	if !ok {
		return Message{}, userErr("unknown state %q", cmd.Args[0])
	}
	if to == StateResolved || to == StatePostmortem || to == StateClosed {
		return Message{}, userErr("use `resolve` / `postmortem` for that")
	}
	if err := s.requireIC(inc, cc.Actor); err != nil {
		return Message{}, err
	}
	return s.transition(ctx, cc.Actor, inc, to, "")
}

func (s *Service) transition(ctx context.Context, actor UserRef, inc *Incident, to State, note string) (Message, error) {
	now := s.Now()
	if err := CheckTransition(inc, to, now); err != nil {
		return Message{}, &UserError{Msg: err.Error()}
	}
	from := inc.State
	ApplyTransition(inc, to, now)
	if _, err := s.Record(ctx, inc.ID, HumanActor(actor), EvTransition, map[string]string{"from": string(from), "to": string(to), "note": note}); err != nil {
		return Message{}, err
	}
	if err := s.save(ctx, inc); err != nil {
		return Message{}, err
	}
	s.refreshTopic(ctx, inc)
	return Message{Text: fmt.Sprintf("%s: %s → *%s*", inc.Key, from, to)}, nil
}

func (s *Service) cmdUpdate(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	text := strings.TrimSpace(strings.Join(cmd.Args, " "))
	if text == "" {
		return Message{}, userErr("usage: `update \"<text>\"`")
	}
	if comms, ok := inc.Roles[RoleComms]; !(ok && sameUser(comms, cc.Actor)) {
		if err := s.requireIC(inc, cc.Actor); err != nil {
			return Message{}, err
		}
	}
	now := s.Now()
	inc.LastStatusAt = &now
	if _, err := s.Record(ctx, inc.ID, HumanActor(cc.Actor), EvStatusPosted, map[string]string{"text": text, "state": string(inc.State)}); err != nil {
		return Message{}, err
	}
	if err := s.save(ctx, inc); err != nil {
		return Message{}, err
	}
	body := fmt.Sprintf("*%s update* (%s, %s): %s", inc.Key, inc.Severity, inc.State, text)
	if ann, ok := s.cfg.AnnounceChannels[s.cfg.DefaultPlatform]; ok {
		if chat, ok := s.ints.Chats[s.cfg.DefaultPlatform]; ok {
			if _, err := chat.PostMessage(ctx, ann, Message{Text: body}); err != nil {
				s.recordErr(ctx, inc, "post_update", err)
			}
		}
	}
	for _, ref := range inc.PageRefs {
		if s.ints.Paging != nil {
			if err := s.ints.Paging.AddNote(ctx, ref, text); err != nil {
				s.recordErr(ctx, inc, "page_note", err)
			}
		}
	}
	if inc.TicketRef != "" && s.ints.Itsm != nil {
		if err := s.ints.Itsm.Comment(ctx, inc.TicketRef, text); err != nil {
			s.recordErr(ctx, inc, "itsm_comment", err)
		}
	}
	return Message{Text: body}, nil
}

func (s *Service) async(inc *Incident, name string, fn func(ctx context.Context) error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.AsyncTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			s.recordErr(ctx, inc, name, err)
			s.post(ctx, inc, Message{Text: fmt.Sprintf("%s failed: %s", name, err)})
		}
	}()
}

func (s *Service) cmdTriage(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if s.Triage == nil {
		return Message{}, userErr("AI triage is not configured")
	}
	focus := cmd.Flag("focus", "")
	if _, err := s.Record(ctx, inc.ID, HumanActor(cc.Actor), EvTriageRequested, map[string]string{"focus": focus}); err != nil {
		return Message{}, err
	}
	snapshot := cloneIncident(inc)
	s.async(snapshot, "triage", func(ctx context.Context) error {
		msg, payload, err := s.Triage.Triage(ctx, snapshot, focus)
		if err != nil {
			return err
		}
		if _, err := s.Record(ctx, snapshot.ID, Actor{Type: ActorBot, ID: "triage"}, EvTriageResult, payload); err != nil {
			return err
		}
		s.post(ctx, snapshot, msg)
		return nil
	})
	return Message{Text: "Triage started (read-only tools only). Results will be posted here."}, nil
}

func (s *Service) cmdRun(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if s.Remediate == nil {
		return Message{}, userErr("remediation is not configured")
	}
	if len(cmd.Args) != 1 {
		return Message{}, userErr("usage: `run <actionId> [key=value …]`")
	}
	_, msg, err := s.Remediate.Request(ctx, inc, cc.Actor, cmd.Args[0], cmd.KV, "human")
	return msg, err
}

func (s *Service) cmdResolve(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if err := s.requireIC(inc, cc.Actor); err != nil {
		return Message{}, err
	}
	summary := strings.TrimSpace(strings.Join(cmd.Args, " "))
	m, err := s.transition(ctx, cc.Actor, inc, StateResolved, summary)
	if err != nil {
		return Message{}, err
	}
	if s.ints.Paging != nil {
		for _, ref := range inc.PageRefs {
			if err := s.ints.Paging.Resolve(ctx, ref); err != nil {
				s.recordErr(ctx, inc, "page_resolve", err)
			}
		}
	}
	if inc.TicketRef != "" && s.ints.Itsm != nil {
		if err := s.ints.Itsm.Comment(ctx, inc.TicketRef, "Resolved: "+summary); err != nil {
			s.recordErr(ctx, inc, "itsm_comment", err)
		}
	}
	if inc.Severity <= Sev2 {
		m.Text += "\nPostmortem is required for " + inc.Severity.String() + ": `postmortem draft`."
	}
	return m, nil
}

func (s *Service) cmdPostmortem(ctx context.Context, cc CommandContext, cmd Command, inc *Incident) (Message, error) {
	if s.Postmortem == nil {
		return Message{}, userErr("postmortem engine is not configured")
	}
	if err := s.requireIC(inc, cc.Actor); err != nil {
		return Message{}, err
	}
	sub := "draft"
	if len(cmd.Args) > 0 {
		sub = strings.ToLower(cmd.Args[0])
	}
	switch sub {
	case "draft":
		if inc.State == StateResolved {
			if _, err := s.transition(ctx, cc.Actor, inc, StatePostmortem, "draft started"); err != nil {
				return Message{}, err
			}
		} else if inc.State != StatePostmortem {
			return Message{}, userErr("resolve the incident first (state is %s)", inc.State)
		}
		snapshot := cloneIncident(inc)
		s.async(snapshot, "postmortem draft", func(ctx context.Context) error {
			pm, msg, err := s.Postmortem.Draft(ctx, snapshot)
			if err != nil {
				return err
			}
			if _, err := s.Record(ctx, snapshot.ID, Actor{Type: ActorBot, ID: "postmortem"}, EvPostmortemDraft, map[string]any{"metrics": pm.Metrics, "similar": pm.SimilarIncidents}); err != nil {
				return err
			}
			s.post(ctx, snapshot, msg)
			return nil
		})
		return Message{Text: "Drafting postmortem… it will be posted here."}, nil
	case "publish":
		if inc.State != StatePostmortem {
			return Message{}, userErr("draft the postmortem first")
		}
		_, msg, err := s.Postmortem.Publish(ctx, inc)
		if err != nil {
			return Message{}, err
		}
		if _, err := s.transition(ctx, cc.Actor, inc, StateClosed, "postmortem published"); err != nil {
			return Message{}, err
		}
		return msg, nil
	}
	return Message{}, userErr("usage: `postmortem [draft|publish]`")
}

func (s *Service) cmdTimeline(ctx context.Context, cmd Command, inc *Incident) (Message, error) {
	evs, err := s.store.Events(ctx, inc.ID)
	if err != nil {
		return Message{}, err
	}
	n := 15
	if v := cmd.Flag("last", ""); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 {
			n = x
		}
	}
	if len(evs) > n {
		evs = evs[len(evs)-n:]
	}
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "`%s` #%d *%s* by %s %s\n", ev.At.Format("15:04:05Z"), ev.Seq, ev.Kind, ev.Actor.ID, compactPayload(ev.Payload))
	}
	return Message{Title: inc.Key + " timeline", Text: b.String(), Ephemeral: true}, nil
}

func compactPayload(p json.RawMessage) string {
	s := string(p)
	if len(s) > 160 {
		s = s[:157] + "…"
	}
	return s
}

// HandleInteraction handles a button click from any chat platform.
func (s *Service) HandleInteraction(ctx context.Context, cc CommandContext, actionID, value string) (Message, error) {
	switch actionID {
	case "declare_signal":
		id, sevStr, _ := strings.Cut(value, "|")
		sig, err := s.store.GetSignal(ctx, id)
		if err != nil {
			return Message{}, userErr("signal %s not found", id)
		}
		if inc, err := s.store.FindOpenIncidentByDedupKey(ctx, sig.DedupKey); err == nil && sig.DedupKey != "" {
			return Message{Text: fmt.Sprintf("Already declared as %s", inc.Key)}, nil
		}
		n, _ := strconv.Atoi(sevStr)
		inc, err := s.Declare(ctx, DeclareRequest{Title: sig.Title, Severity: Severity(n), Signals: []Signal{*sig}, Actor: cc.Actor, Platform: cc.Actor.Platform, Reason: "declared from proposal"})
		if err != nil {
			return Message{}, err
		}
		return Message{Text: fmt.Sprintf("%s declared %s (%s)", displayUser(cc.Actor), inc.Key, inc.Severity)}, nil
	case "dismiss_signal":
		s.log.Info("proposal dismissed", "signal", value, "by", cc.Actor.Email)
		return Message{Text: fmt.Sprintf("Dismissed by %s", displayUser(cc.Actor))}, nil
	case "request_action":
		// From a triage recommendation: creates an ActionRequest that still
		// needs approval. Triage itself can never execute anything.
		if s.Remediate == nil {
			return Message{}, userErr("remediation is not configured")
		}
		var v struct {
			I string            `json:"i"`
			A string            `json:"a"`
			P map[string]string `json:"p"`
		}
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return Message{}, userErr("malformed action button")
		}
		inc, err := s.store.GetIncident(ctx, v.I)
		if err != nil {
			return Message{}, userErr("incident not found")
		}
		_, m, err := s.Remediate.Request(ctx, inc, cc.Actor, v.A, v.P, "triage")
		return m, err
	case "approve_action", "deny_action":
		if s.Remediate == nil {
			return Message{}, userErr("remediation is not configured")
		}
		if actionID == "approve_action" {
			_, m, err := s.Remediate.Approve(ctx, value, cc.Actor)
			return m, err
		}
		_, m, err := s.Remediate.Deny(ctx, value, cc.Actor)
		return m, err
	}
	return Message{}, userErr("unknown action %q", actionID)
}
