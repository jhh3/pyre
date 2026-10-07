package search

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// PerSessionCap is the most transcript hits one session keeps in a merged
// result, so one long session does not crowd out the rest.
const PerSessionCap = 3

// FromTranscript converts a node's transcript hit to a merged hit (Score
// keeps the raw relevance until Rank).
func FromTranscript(h contracts.TranscriptHit) contracts.SearchHit {
	return contracts.SearchHit{Source: contracts.SearchSourceTranscript, NodeID: h.NodeID, SessionID: h.SessionID, RunID: h.RunID,
		Title: h.Title, Kind: h.Kind, Seq: h.Seq, At: h.At, Snippet: h.Snippet, Score: h.Score,
		Provenance: string(h.Harness) + " transcript"}
}

// Rank orders hits from several sources by relevance and recency and
// applies the limit and the per-session cap. Relevance is normalized per
// source (BM25 for transcripts across all nodes; list position for sources
// that carry no score), then weighted by recency (a two-week-old hit
// weighs about 0.75 of a fresh one, a very old one 0.5). It returns the
// ranked hits and whether any were cut.
func Rank(hits []contracts.SearchHit, now time.Time, limit int) ([]contracts.SearchHit, bool) {
	maxBy := map[string]float64{}
	pos := map[string]int{}
	for _, h := range hits {
		if h.Score > maxBy[h.Source] {
			maxBy[h.Source] = h.Score
		}
	}
	type scored struct {
		h contracts.SearchHit
		s float64
	}
	out := make([]scored, 0, len(hits))
	for _, h := range hits {
		var rel float64
		if m := maxBy[h.Source]; m > 0 {
			rel = math.Max(0, h.Score) / m
		} else {
			rel = 1 / (1 + 0.15*float64(pos[h.Source]))
			pos[h.Source]++
		}
		age := 0.0
		if !h.At.IsZero() {
			age = math.Max(0, now.Sub(h.At).Hours()/24)
		}
		rec := 0.5 + 0.5/(1+age/14)
		s := (0.35 + 0.65*rel) * rec
		h.Score = math.Round(s*1000) / 1000
		out = append(out, scored{h, s})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		if !out[i].h.At.Equal(out[j].h.At) {
			return out[i].h.At.After(out[j].h.At)
		}
		return hitKey(out[i].h) < hitKey(out[j].h)
	})
	perSession := map[string]int{}
	ranked := make([]contracts.SearchHit, 0, min(len(out), limit))
	cut := false
	for _, sc := range out {
		h := sc.h
		if h.Source == contracts.SearchSourceTranscript {
			if perSession[h.SessionID] >= PerSessionCap {
				cut = true
				continue
			}
			perSession[h.SessionID]++
		}
		if limit > 0 && len(ranked) == limit {
			cut = true
			break
		}
		ranked = append(ranked, h)
	}
	return ranked, cut
}

func hitKey(h contracts.SearchHit) string {
	return strings.Join([]string{h.Source, h.NodeID, h.SessionID, h.RunID, h.Ref, itoa(h.Seq)}, "|")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// SortNodes orders node statuses: searched first, then by name.
func SortNodes(ns []contracts.SearchNodeStatus) {
	order := map[string]int{contracts.SearchNodeSearched: 0, contracts.SearchNodeTimeout: 1, contracts.SearchNodeError: 2,
		contracts.SearchNodeUnsupported: 3, contracts.SearchNodeOffline: 4, contracts.SearchNodeSkipped: 5}
	sort.SliceStable(ns, func(i, j int) bool {
		if order[ns[i].State] != order[ns[j].State] {
			return order[ns[i].State] < order[ns[j].State]
		}
		return ns[i].Name < ns[j].Name
	})
}

// WantSource reports whether req asks for source (empty Sources: all).
func WantSource(req contracts.SearchRequest, source string) bool {
	if len(req.Sources) == 0 {
		return true
	}
	for _, s := range req.Sources {
		if s == source || (s == "docs" && (source == contracts.SearchSourceDocument || source == contracts.SearchSourceNote || source == contracts.SearchSourceArtifact)) {
			return true
		}
	}
	return false
}

// NormalizeRequest validates and clamps a fan-out request.
func NormalizeRequest(req *contracts.SearchRequest) error {
	req.Query = strings.TrimSpace(req.Query)
	if len(req.Query) > MaxQueryLen {
		return contracts.Errorf(contracts.ErrInvalid, "query is longer than %d bytes", MaxQueryLen)
	}
	if len(Terms(req.Query)) == 0 {
		return contracts.Errorf(contracts.ErrInvalid, "a search query with at least one word is required")
	}
	if req.Limit <= 0 {
		req.Limit = 40
	}
	req.Limit = min(req.Limit, 200)
	if req.BudgetMS <= 0 {
		req.BudgetMS = 2500
	}
	req.BudgetMS = min(max(req.BudgetMS, 200), 15000)
	return nil
}
