package core

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Redactor removes secrets and PII before data leaves incidentd (to the LLM,
// to logs or to the eval store). It is deliberately conservative: a false
// positive costs a little context, a false negative leaks a secret.
type Redactor struct {
	rules      []redactRule
	hashEmails bool
}

type redactRule struct {
	re   *regexp.Regexp
	repl string
}

// NewRedactor builds the default rule set. Extra patterns (from config) are
// appended and replaced with [REDACTED:custom].
func NewRedactor(hashEmails bool, extra []string) (*Redactor, error) {
	r := &Redactor{hashEmails: hashEmails}
	defaults := []redactRule{
		{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)\s*[:=]\s*("?)(bearer|basic|token)?\s*[A-Za-z0-9._~+/=-]+`), "$1: [REDACTED:auth]"},
		{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[REDACTED:private_key]"},
		{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), "[REDACTED:jwt]"},
		{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "[REDACTED:aws_key_id]"},
		{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), "[REDACTED:slack_token]"},
		{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), "[REDACTED:github_token]"},
		{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}\b`), "[REDACTED:anthropic_key]"},
		{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^:/\s@]+):([^@\s/]+)@`), "$1[REDACTED:userinfo]@"},
		{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_-]?key|app[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|token)(["']?\s*[:=]\s*["']?)([^\s"',;&]{4,})`), "$1$2[REDACTED:secret]"},
	}
	r.rules = append(r.rules, defaults...)
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.rules = append(r.rules, redactRule{re, "[REDACTED:custom]"})
	}
	return r, nil
}

// MustRedactor is NewRedactor for static configuration.
func MustRedactor(hashEmails bool) *Redactor {
	r, err := NewRedactor(hashEmails, nil)
	if err != nil {
		panic(err)
	}
	return r
}

var emailRe = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)

// Redact applies all rules to s.
func (r *Redactor) Redact(s string) string {
	for _, rule := range r.rules {
		s = rule.re.ReplaceAllString(s, rule.repl)
	}
	if r.hashEmails {
		s = emailRe.ReplaceAllStringFunc(s, func(e string) string {
			sum := sha256.Sum256([]byte(strings.ToLower(e)))
			return "user-" + hex.EncodeToString(sum[:])[:10]
		})
	}
	return s
}
