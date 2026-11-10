package core

import (
	"fmt"
	"strings"
	"unicode"
)

// Verbs is the command surface, identical on every chat platform.
var Verbs = []string{"declare", "sev", "role", "status", "update", "triage", "run", "resolve", "postmortem", "timeline", "help"}

// Command is a parsed chat command.
type Command struct {
	Verb  string
	Args  []string          // positional arguments (quotes removed)
	Flags map[string]string // --flag value / --flag (=> "true")
	KV    map[string]string // key=value pairs (used by `run`)
}

// Flag returns a flag value or def.
func (c Command) Flag(name, def string) string {
	if v, ok := c.Flags[name]; ok {
		return v
	}
	return def
}

// ParseCommand parses text such as:
//
//	declare "Checkout 500s" --sev 2 --service checkout
//	run k8s.rollout_restart namespace=checkout deployment=api
//
// A leading "/incident" or bot mention should be stripped by the adapter.
func ParseCommand(text string) (Command, error) {
	toks, err := tokenize(strings.TrimSpace(text))
	if err != nil {
		return Command{}, err
	}
	if len(toks) == 0 {
		return Command{Verb: "help"}, nil
	}
	cmd := Command{Verb: strings.ToLower(toks[0].s), Flags: map[string]string{}, KV: map[string]string{}}
	known := false
	for _, v := range Verbs {
		if v == cmd.Verb {
			known = true
		}
	}
	if !known {
		return Command{}, fmt.Errorf("unknown command %q; try: %s", toks[0].s, strings.Join(Verbs, ", "))
	}
	rest := toks[1:]
	for i := 0; i < len(rest); i++ {
		t := rest[i]
		switch {
		case strings.HasPrefix(t.s, "--") && !t.quoted:
			name := strings.TrimPrefix(t.s, "--")
			if k, v, ok := strings.Cut(name, "="); ok {
				cmd.Flags[k] = v
				continue
			}
			if i+1 < len(rest) && !(strings.HasPrefix(rest[i+1].s, "--") && !rest[i+1].quoted) {
				cmd.Flags[name] = rest[i+1].s
				i++
			} else {
				cmd.Flags[name] = "true"
			}
		case !t.quoted && strings.Contains(t.s, "=") && !strings.HasPrefix(t.s, "="):
			k, v, _ := strings.Cut(t.s, "=")
			cmd.KV[k] = v
		default:
			cmd.Args = append(cmd.Args, t.s)
		}
	}
	return cmd, nil
}

type token struct {
	s      string
	quoted bool
}

func tokenize(s string) ([]token, error) {
	var out []token
	var cur strings.Builder
	inTok, quoted := false, false
	var quote rune
	for _, r := range s {
		// Chat clients often turn quotes into smart quotes.
		switch r {
		case '“', '”':
			r = '"'
		case '‘', '’':
			r = '\''
		}
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inTok, quoted = r, true, true
		case unicode.IsSpace(r):
			if inTok {
				out = append(out, token{cur.String(), quoted})
				cur.Reset()
				inTok, quoted = false, false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inTok {
		out = append(out, token{cur.String(), quoted})
	}
	return out, nil
}

// HelpText lists the commands.
const HelpText = "*Incident bot commands*\n" +
	"• `declare \"<title>\" [--sev 1-4] [--service S] [--private]` - declare an incident\n" +
	"• `sev <1-4> [--reason \"…\"]` - change severity (IC)\n" +
	"• `role <ic|comms|ops|scribe> @user` - assign a role (IC; anyone may take a vacant IC)\n" +
	"• `status [investigating|identified|mitigating|monitoring]` - show or change state (IC)\n" +
	"• `update \"<text>\"` - post a stakeholder update (IC, comms)\n" +
	"• `triage [--focus \"…\"]` - run AI triage (read-only)\n" +
	"• `run <actionId> [key=value …]` - request a catalog action (needs approval)\n" +
	"• `resolve [\"summary\"]` - resolve (IC)\n" +
	"• `postmortem [draft|publish]` - draft or publish the postmortem (IC)\n" +
	"• `timeline [--last N]` - show the timeline"
