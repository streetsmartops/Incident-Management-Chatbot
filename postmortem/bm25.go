package postmortem

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// BM25 parameters (Robertson/Spärck Jones defaults).
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an and are as at be by for from has in is it its of on or that the to was were will with this not no") {
		stop[w] = true
	}
}

// Tokenize lowercases, splits on non-alphanumerics and drops stop words.
func Tokenize(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-' })
	out := f[:0]
	for _, t := range f {
		t = strings.Trim(t, ".-")
		if len(t) > 1 && !stop[t] {
			out = append(out, t)
		}
	}
	return out
}

type doc struct {
	ref   core.SimilarIncident
	tf    map[string]int
	len   int
}

// Index is an in-memory BM25 index over past incidents.
type Index struct {
	mu    sync.RWMutex
	docs  []doc
	df    map[string]int
	total int
}

// NewIndex returns an empty index.
func NewIndex() *Index { return &Index{df: map[string]int{}} }

// Add indexes one document (title + summary + root cause + services …).
func (ix *Index) Add(ref core.SimilarIncident, text string) {
	toks := Tokenize(text)
	tf := map[string]int{}
	for _, t := range toks {
		tf[t]++
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for t := range tf {
		ix.df[t]++
	}
	ix.docs = append(ix.docs, doc{ref: ref, tf: tf, len: len(toks)})
	ix.total += len(toks)
}

// Search returns the top k documents for query.
func (ix *Index) Search(_ context.Context, query string, k int) ([]core.SimilarIncident, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := float64(len(ix.docs))
	if n == 0 {
		return nil, nil
	}
	avg := float64(ix.total) / n
	q := Tokenize(query)
	var out []core.SimilarIncident
	for _, d := range ix.docs {
		score := 0.0
		for _, t := range q {
			f := float64(d.tf[t])
			if f == 0 {
				continue
			}
			df := float64(ix.df[t])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * f * (bm25K1 + 1) / (f + bm25K1*(1-bm25B+bm25B*float64(d.len)/avg))
		}
		if score > 0 {
			r := d.ref
			r.Score = math.Round(score*1000) / 1000
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// Retriever keeps a BM25 index of closed/postmortem incidents, rebuilt from
// the store on demand.
type Retriever struct {
	Store   core.Store
	Exclude string // incident ID to leave out (the current one)
}

// Search builds the index from the store and queries it.
func (r *Retriever) Search(ctx context.Context, query string, k int) ([]core.SimilarIncident, error) {
	incs, err := r.Store.ListIncidents(ctx, core.IncidentFilter{States: []core.State{core.StateResolved, core.StatePostmortem, core.StateClosed}, Limit: 2000})
	if err != nil {
		return nil, err
	}
	ix := NewIndex()
	for _, inc := range incs {
		if inc.ID == r.Exclude {
			continue
		}
		text := inc.Title + " " + strings.Join(inc.Services, " ")
		if pm, err := r.Store.GetPostmortem(ctx, inc.ID); err == nil {
			for _, sec := range []string{"summary", "root_cause", "contributing_factors", "detection"} {
				text += " " + pm.Sections[sec]
			}
		}
		ix.Add(core.SimilarIncident{IncidentID: inc.ID, Key: inc.Key, Title: inc.Title}, text)
	}
	return ix.Search(ctx, query, k)
}
