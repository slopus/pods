package store

import (
	"cmp"
	"sort"
	"strconv"
	"strings"

	"github.com/slopus/pods/internal/api"
)

// Where is one ANDed top-level field equality filter.
type Where struct {
	Field string
	Value string // raw string from the query parameter
}

// Query describes a collection query.
type Query struct {
	Where  []Where
	Sort   string // "" = created_at asc; leading '-' = descending
	Limit  int    // 0 = no limit
	Offset int
}

// Query returns the documents in pod/coll matching q. Total counts matches
// before limit/offset are applied. A missing pod or collection yields an
// empty result.
func (s *Store) Query(pod, coll string, q Query) api.QueryResult {
	collDocs, err := s.collectionDocs(pod, coll)
	if err != nil {
		return api.QueryResult{}
	}
	docs := make([]api.Doc, 0, len(collDocs))
	for _, d := range collDocs {
		if matchesAll(d, q.Where) {
			docs = append(docs, copyDoc(d))
		}
	}

	// Deterministic base order before the user sort.
	sort.Slice(docs, func(i, j int) bool {
		a, _ := docs[i][FieldID].(string)
		b, _ := docs[j][FieldID].(string)
		return a < b
	})

	field, desc := FieldCreatedAt, false
	if q.Sort != "" {
		field = q.Sort
		if rest, ok := strings.CutPrefix(q.Sort, "-"); ok {
			field, desc = rest, true
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		vi, oki := docs[i][field]
		vj, okj := docs[j][field]
		if oki != okj {
			return oki // docs missing the sort field always go last
		}
		if !oki {
			return false
		}
		c := compareValues(vi, vj)
		if desc {
			return c > 0
		}
		return c < 0
	})

	total := len(docs)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	docs = docs[offset:]
	if q.Limit > 0 && q.Limit < len(docs) {
		docs = docs[:q.Limit]
	}
	return api.QueryResult{Docs: docs, Total: total}
}

func matchesAll(d api.Doc, where []Where) bool {
	for _, w := range where {
		if !matches(d, w) {
			return false
		}
	}
	return true
}

// matches applies the contract's where rules: string equality, numeric
// comparison after parsing the value as float64, bool against "true"/"false",
// null against "null"; missing fields and composite values never match.
func matches(d api.Doc, w Where) bool {
	v, ok := d[w.Field]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case string:
		return t == w.Value
	case float64:
		f, err := strconv.ParseFloat(w.Value, 64)
		return err == nil && f == t
	case bool:
		return (w.Value == "true" && t) || (w.Value == "false" && !t)
	case nil:
		return w.Value == "null"
	default:
		return false
	}
}

// compareValues orders sortable JSON values: numbers numerically, strings
// lexically, bools false<true. Values of different types order by type rank
// so the sort is total and deterministic.
func compareValues(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return cmp.Compare(ra, rb)
	}
	switch av := a.(type) {
	case bool:
		bv := b.(bool)
		switch {
		case av == bv:
			return 0
		case !av:
			return -1
		default:
			return 1
		}
	case float64:
		return cmp.Compare(av, b.(float64))
	case string:
		return cmp.Compare(av, b.(string))
	default:
		return 0
	}
}

func typeRank(v any) int {
	switch v.(type) {
	case bool:
		return 0
	case float64:
		return 1
	case string:
		return 2
	default:
		return 3
	}
}
