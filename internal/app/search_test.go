package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

// Other Gateway methods remain nil: an accidental Stat or Download makes the
// test fail rather than masking hidden content reads in a metadata-only search.
type searchGateway struct {
	Gateway
	objects []api.Object
	calls   []ListOptions
	list    func(ListOptions) (*api.Listing, error)
}

func (g *searchGateway) List(_ context.Context, o ListOptions) (*api.Listing, error) {
	g.calls = append(g.calls, o)
	if g.list != nil {
		return g.list(o)
	}
	start := 0
	if o.Cursor != "" {
		var err error
		start, err = strconv.Atoi(strings.TrimPrefix(o.Cursor, "upstream-opaque-"))
		if err != nil || start < 0 || start > len(g.objects) {
			return nil, errors.New("unexpected upstream cursor")
		}
	}
	end := min(start+o.Limit, len(g.objects))
	page := &api.Listing{Prefix: o.Prefix, Objects: g.objects[start:end]}
	if end < len(g.objects) {
		page.NextCursor = fmt.Sprintf("upstream-opaque-%d", end)
	}
	return page, nil
}

func searchObjects(n int, title string) []api.Object {
	objects := make([]api.Object, n)
	for i := range objects {
		objects[i] = api.Object{Path: fmt.Sprintf("team/%04d.txt", i), Title: title, Visibility: api.Private}
	}
	return objects
}

func TestSearchMetadataLiteralUnicode(t *testing.T) {
	t.Parallel()
	objects := []api.Object{
		{Path: "team/01.txt", Title: "ПрИвЕт мир", Visibility: api.Private},
		{Path: "team/02.txt", Description: "The quarterly Report", Visibility: api.Public},
		{Path: "team/03.txt", Tags: []string{"finance", "Important"}, Visibility: api.Unlisted},
		{Path: "team/04[ab]*.txt", Visibility: api.Private},
		{Path: "team/05.txt", Title: "hello world", Visibility: api.Private},
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{" пРИВет ", "team/01.txt"},
		{"quarterly report", "team/02.txt"},
		{"PORT", "team/02.txt,team/03.txt"},
		{"[ab]*", "team/04[ab]*.txt"},
		{".*", ""},
		{"hello world", "team/05.txt"},
		{"not in metadata", ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			g := &searchGateway{objects: objects}
			w := workspace(g, api.ScopeRO, "team/")
			result, err := w.Search(context.Background(), SearchOptions{Query: tc.query})
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, obj := range result.Files {
				paths = append(paths, obj.Path)
			}
			if strings.Join(paths, ",") != tc.want || result.Prefix != "team/" || !result.Complete || result.NextCursor != "" || result.Scanned != len(objects) {
				t.Fatalf("unexpected search result: %+v", result)
			}
			if len(g.calls) != 1 || !g.calls[0].Recursive || g.calls[0].Prefix != "team/" {
				t.Fatalf("unexpected backend search: %+v", g.calls)
			}
		})
	}
}

func TestSearchContinuationNeverDiscardsMatches(t *testing.T) {
	t.Parallel()
	objects := searchObjects(37, "needle")
	for i := range objects {
		if i%3 == 0 {
			objects[i].Title = "unrelated"
		}
	}
	g := &searchGateway{objects: objects}
	w := workspace(g, api.ScopeRO, "team/")
	w.identity.Subject = "alice"
	var got []string
	cursor := ""
	complete := false
	for invocation := 0; invocation < len(objects) && !complete; invocation++ {
		before := len(g.calls)
		result, err := w.Search(context.Background(), SearchOptions{Query: "needle", Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Files) > 2 || len(g.calls)-before > searchMaxPages {
			t.Fatal("search exceeded a per-invocation bound")
		}
		for _, obj := range result.Files {
			got = append(got, obj.Path)
		}
		if result.Complete == (result.NextCursor != "") {
			t.Fatal("completion and continuation disagree")
		}
		cursor, complete = result.NextCursor, result.Complete
	}
	var want []string
	for _, obj := range objects {
		if obj.Title == "needle" {
			want = append(want, obj.Path)
		}
	}
	if !complete || !reflect.DeepEqual(got, want) {
		t.Fatalf("search dropped or repeated matches: got %v; want %v", got, want)
	}
}

func TestSearchScanBudgetAndEmptyPartialPages(t *testing.T) {
	t.Parallel()
	g := &searchGateway{objects: searchObjects(600, "unrelated")}
	w := workspace(g, api.ScopeRO, "team/")
	result, err := w.Search(context.Background(), SearchOptions{Query: "needle", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 5 || result.Scanned != 500 || result.Complete || result.NextCursor == "" || result.Files == nil || len(result.Files) != 0 {
		t.Fatalf("expected a bounded, empty partial page: %+v", result)
	}
	result, err = w.Search(context.Background(), SearchOptions{Query: "needle", Limit: 100, Cursor: result.NextCursor})
	if err != nil || !result.Complete || result.Scanned != 100 || result.NextCursor != "" {
		t.Fatalf("could not resume bounded scan: %+v, %v", result, err)
	}
	for _, call := range g.calls {
		if call.Limit > 100 || call.Limit < 1 {
			t.Fatal("unsafe backend page size")
		}
	}

	g = &searchGateway{objects: searchObjects(101, "unrelated")}
	w = workspace(g, api.ScopeRO, "team/")
	result, err = w.Search(context.Background(), SearchOptions{Query: "needle"})
	if err != nil || len(g.calls) != 5 || result.Scanned != 100 || result.Complete || result.NextCursor == "" {
		t.Fatalf("default search must also bound upstream calls: %+v, %v", result, err)
	}
}

func TestSearchNarrowedAuthorityAndVisibility(t *testing.T) {
	t.Parallel()
	// Simulate a broad upstream key whose adapter ignores listing filters.
	g := &searchGateway{objects: []api.Object{
		{Path: "other/secret.txt", Title: "needle", Visibility: api.Private},
		{Path: "team/a.txt", Title: "needle", Visibility: api.Private},
		{Path: "team/b.txt", Title: "needle", Visibility: api.Public},
		{Path: "team/sub/c.txt", Title: "needle", Visibility: api.Private},
	}}
	w := workspace(g, api.ScopeRO, "team/")
	result, err := w.Search(context.Background(), SearchOptions{Query: "needle", Prefix: "/", Visibility: []api.Visibility{api.Private}})
	if err != nil || len(result.Files) != 2 || result.Files[0].Path != "team/a.txt" || result.Files[1].Path != "team/sub/c.txt" {
		t.Fatalf("narrowed authority leaked a match: %+v, %v", result, err)
	}
	if len(g.calls) != 1 || g.calls[0].Prefix != "team/" || g.calls[0].Visibility[0] != api.Private {
		t.Fatal("scope was not passed to upstream listing")
	}
	before := len(g.calls)
	if _, err = w.Search(context.Background(), SearchOptions{Query: "needle", Prefix: "other/"}); !errors.Is(err, access.ErrForbidden) || len(g.calls) != before {
		t.Fatal("unrelated prefix reached upstream")
	}
	result, err = w.Search(context.Background(), SearchOptions{Query: "needle", Prefix: "team/sub/"})
	if err != nil || len(result.Files) != 1 || result.Files[0].Path != "team/sub/c.txt" {
		t.Fatalf("requested subtree leaked sibling matches: %+v, %v", result, err)
	}
}

func TestSearchCursorBindsQueryFiltersAndCaller(t *testing.T) {
	t.Parallel()
	g := &searchGateway{objects: searchObjects(4, "needle")}
	w := workspace(g, api.ScopeRO, "team/")
	w.identity.Subject = "alice"
	options := SearchOptions{Query: "needle", Limit: 1, Visibility: []api.Visibility{api.Private, api.Public}}
	first, err := w.Search(context.Background(), options)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("could not prepare continuation: %+v, %v", first, err)
	}
	options.Cursor = first.NextCursor
	for _, tc := range []struct {
		name   string
		change func(*Workspace, *SearchOptions)
	}{
		{"query", func(_ *Workspace, o *SearchOptions) { o.Query = "different" }},
		{"prefix", func(_ *Workspace, o *SearchOptions) { o.Prefix = "team/sub/" }},
		{"visibility", func(_ *Workspace, o *SearchOptions) { o.Visibility = []api.Visibility{api.Private} }},
		{"subject", func(w *Workspace, _ *SearchOptions) { w.identity.Subject = "bob" }},
		{"scope", func(w *Workspace, _ *SearchOptions) { w.identity.Scope = api.ScopeRW }},
		{"authority prefix", func(w *Workspace, o *SearchOptions) { w.identity.Prefix, o.Prefix = "", "team/" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedWorkspace, changedOptions := *w, options
			tc.change(&changedWorkspace, &changedOptions)
			before := len(g.calls)
			if _, err := changedWorkspace.Search(context.Background(), changedOptions); err == nil || len(g.calls) != before {
				t.Fatal("mismatched cursor reached upstream")
			}
		})
	}
	// Equivalent canonical filters and a changed page limit remain resumable.
	options.Query, options.Prefix, options.Limit = " NEEDLE ", "team/", 2
	options.Visibility = []api.Visibility{api.Public, api.Private, api.Public}
	result, err := w.Search(context.Background(), options)
	if err != nil || len(result.Files) != 2 || result.Files[0].Path != "team/0001.txt" {
		t.Fatalf("canonical search continuation failed: %+v, %v", result, err)
	}
	lastVisibility := g.calls[len(g.calls)-1].Visibility
	if !sort.SliceIsSorted(lastVisibility, func(i, j int) bool { return lastVisibility[i] < lastVisibility[j] }) || len(lastVisibility) != 2 {
		t.Fatal("visibility binding was not canonicalized")
	}
}

func TestSearchValidationAndMalformedCursors(t *testing.T) {
	t.Parallel()
	for _, options := range []SearchOptions{
		{}, {Query: " \t\n"}, {Query: strings.Repeat("é", 257)}, {Query: string([]byte{0xff})},
		{Query: "x", Limit: -1}, {Query: "x", Limit: 101}, {Query: "x", Visibility: []api.Visibility{"invalid"}},
		{Query: "x", Cursor: "!"}, {Query: "x", Cursor: strings.Repeat("a", searchMaxCursor+1)},
		{Query: "x", Cursor: base64.RawURLEncoding.EncodeToString([]byte("{}"))},
		{Query: "x", Cursor: base64.RawURLEncoding.EncodeToString([]byte("null"))},
	} {
		g := &searchGateway{}
		w := workspace(g, api.ScopeRO, "team/")
		if _, err := w.Search(context.Background(), options); err == nil || len(g.calls) != 0 {
			t.Fatalf("invalid input reached upstream: %+v", options)
		}
	}
	w := workspace(&searchGateway{}, api.ScopeRO, "team/")
	binding := w.searchBinding("x", "team/", nil)
	valid, err := encodeSearchCursor("upstream", binding)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := base64.RawURLEncoding.DecodeString(valid)
	for _, malformed := range []string{
		strings.TrimSuffix(string(b), "}") + `,"unknown":true}`,
		string(b) + `{}`,
		strings.Replace(string(b), `"v":1`, `"v":2`, 1),
		strings.Replace(string(b), `"upstream":"upstream"`, `"upstream":""`, 1),
	} {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(malformed))
		if _, err := decodeSearchCursor(cursor, binding); err == nil {
			t.Fatalf("malformed cursor was accepted: %s", malformed)
		}
	}

	g := &searchGateway{}
	w = workspace(g, api.ScopeRO, "")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Search(cancelled, SearchOptions{Query: "x"}); !errors.Is(err, context.Canceled) || len(g.calls) != 0 {
		t.Fatal("cancelled search reached upstream")
	}
}

func TestSearchPreservesQueryBoundaryAuthorityAndUpstreamErrors(t *testing.T) {
	t.Parallel()
	g := &searchGateway{}
	w := workspace(g, api.ScopeRO, "team/")
	if result, err := w.Search(context.Background(), SearchOptions{Query: strings.Repeat("é", 256)}); err != nil || !result.Complete {
		t.Fatalf("256-rune query was incorrectly bounded by bytes: %+v, %v", result, err)
	}
	before := len(g.calls)
	w.identity.Scope = "invalid"
	if _, err := w.Search(context.Background(), SearchOptions{Query: "x"}); !errors.Is(err, access.ErrForbidden) || len(g.calls) != before {
		t.Fatal("unauthorized search reached upstream")
	}
	w.identity.Scope = api.ScopeRO
	sentinel := errors.New("upstream failed")
	g.list = func(ListOptions) (*api.Listing, error) { return nil, sentinel }
	if _, err := w.Search(context.Background(), SearchOptions{Query: "x"}); !errors.Is(err, sentinel) {
		t.Fatal("upstream failure was hidden as an empty search")
	}
}

func TestSearchRejectsBrokenUpstreamPages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		list func(ListOptions) (*api.Listing, error)
	}{
		{"nil page", func(ListOptions) (*api.Listing, error) { return nil, nil }},
		{"oversized", func(o ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: searchObjects(o.Limit+1, "needle")}, nil
		}},
		{"directories", func(ListOptions) (*api.Listing, error) { return &api.Listing{Dirs: []string{"team/"}}, nil }},
		{"empty with cursor", func(ListOptions) (*api.Listing, error) { return &api.Listing{NextCursor: "next"}, nil }},
		{"invalid path", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/../secret"}}}, nil
		}},
		{"noncanonical path", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "/team/secret"}}}, nil
		}},
		{"unsorted", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/b"}, {Path: "team/a"}}}, nil
		}},
		{"duplicate path", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/a"}, {Path: "team/a"}}}, nil
		}},
		{"repeated cursor", func(o ListOptions) (*api.Listing, error) {
			path, next := "team/a", "first"
			if o.Cursor != "" {
				path, next = "team/b", o.Cursor
			}
			return &api.Listing{Objects: []api.Object{{Path: path}}, NextCursor: next}, nil
		}},
		{"cursor cycle", func(o ListOptions) (*api.Listing, error) {
			path, next := "team/a", "first"
			switch o.Cursor {
			case "first":
				path, next = "team/b", "second"
			case "second":
				path, next = "team/c", "first"
			}
			return &api.Listing{Objects: []api.Object{{Path: path}}, NextCursor: next}, nil
		}},
		{"same rows different cursor", func(o ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/a"}}, NextCursor: o.Cursor + "next"}, nil
		}},
		{"oversized cursor", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/a"}}, NextCursor: strings.Repeat("a", searchMaxCursor+1)}, nil
		}},
		{"oversized wrapped cursor", func(ListOptions) (*api.Listing, error) {
			return &api.Listing{Objects: []api.Object{{Path: "team/a", Title: "needle"}}, NextCursor: strings.Repeat("a", 7000)}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &searchGateway{list: tc.list}
			w := workspace(g, api.ScopeRO, "team/")
			limit := 20
			if tc.name == "oversized wrapped cursor" {
				limit = 1
			}
			if _, err := w.Search(context.Background(), SearchOptions{Query: "needle", Limit: limit}); err == nil {
				t.Fatal("broken upstream page was accepted")
			}
		})
	}
}
