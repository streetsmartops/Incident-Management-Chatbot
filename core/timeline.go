package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// GenesisHash is the PrevHash of an incident's first event.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// NewEvent builds the next hash-chained event after prev (nil for first).
func NewEvent(incidentID string, prev *TimelineEvent, at time.Time, actor Actor, kind EventKind, payload any) (TimelineEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return TimelineEvent{}, fmt.Errorf("marshal timeline payload: %w", err)
	}
	ev := TimelineEvent{
		Seq:        1,
		IncidentID: incidentID,
		At:         at.UTC().Truncate(time.Microsecond),
		Actor:      actor,
		Kind:       kind,
		Payload:    raw,
		PrevHash:   GenesisHash,
	}
	if prev != nil {
		ev.Seq = prev.Seq + 1
		ev.PrevHash = prev.Hash
	}
	ev.Hash = hashEvent(ev)
	return ev, nil
}

func hashEvent(ev TimelineEvent) string {
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	write(strconv.FormatInt(ev.Seq, 10))
	write(ev.IncidentID)
	write(ev.At.Format(time.RFC3339Nano))
	write(string(ev.Actor.Type))
	write(ev.Actor.ID)
	write(ev.Actor.Email)
	write(string(ev.Kind))
	write(string(ev.Payload))
	write(ev.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyChain checks sequence continuity and hashes. It returns the seq of
// the first bad event, or 0 if the chain is intact.
func VerifyChain(events []TimelineEvent) (int64, error) {
	prev := GenesisHash
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			return ev.Seq, fmt.Errorf("sequence gap at index %d: seq=%d", i, ev.Seq)
		}
		if ev.PrevHash != prev {
			return ev.Seq, fmt.Errorf("seq %d: prevHash mismatch", ev.Seq)
		}
		if hashEvent(ev) != ev.Hash {
			return ev.Seq, fmt.Errorf("seq %d: hash mismatch (event modified)", ev.Seq)
		}
		prev = ev.Hash
	}
	return 0, nil
}
