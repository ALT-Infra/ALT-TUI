package tooling

import (
	"reflect"
	"testing"
)

func TestSearchDefaultsToQueryFocusedHighlights(t *testing.T) {
	contents, err := buildExaContents(nil, contentDefaults{
		highlightsQuery: "the actual question",
		highlightsLimit: 4_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"highlights": map[string]any{
		"query": "the actual question", "maxCharacters": 4_000,
	}}
	if !reflect.DeepEqual(contents, want) {
		t.Fatalf("contents = %#v, want %#v", contents, want)
	}
}

func TestFetchDefaultsToBoundedFullText(t *testing.T) {
	contents, err := buildExaContents(&WebContentsInput{}, contentDefaults{textLimit: 50_000})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"text": map[string]any{"maxCharacters": 50_000}}
	if !reflect.DeepEqual(contents, want) {
		t.Fatalf("contents = %#v, want %#v", contents, want)
	}
}

func TestCurrentContentControlsMapExactlyToExa(t *testing.T) {
	fresh := 0
	filterEmpty := false
	contents, err := buildExaContents(&WebContentsInput{
		Text: &WebTextInput{
			MaxCharacters: 75_000,
			Verbosity:     "full",
			IncludeSections: []string{
				"body", "metadata",
			},
		},
		Highlights: &WebHighlightsInput{Query: "recovery boundary", MaxCharacters: 5_000},
		Summary: &WebSummaryInput{
			Query: "State the observed invariant.",
			Schema: map[string]any{
				"type": "object",
			},
		},
		MaxAgeHours:        &fresh,
		LivecrawlTimeoutMS: 30_000,
		FilterEmptyResults: &filterEmpty,
		Subpages:           3,
		SubpageTargets:     []string{"methodology", "sources", "sources"},
		ExtraLinks:         10,
		ExtraImageLinks:    2,
	}, contentDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := contents["text"].(map[string]any)
	if !ok || text["verbosity"] != "full" || !reflect.DeepEqual(text["includeSections"], []string{"body", "metadata"}) {
		t.Fatalf("text controls = %#v", contents["text"])
	}
	if contents["maxAgeHours"] != 0 || contents["livecrawlTimeout"] != 30_000 || contents["filterEmptyResults"] != false {
		t.Fatalf("freshness controls = %#v", contents)
	}
	if !reflect.DeepEqual(contents["subpageTarget"], []string{"methodology", "sources"}) {
		t.Fatalf("subpageTarget = %#v", contents["subpageTarget"])
	}
	extras, ok := contents["extras"].(map[string]any)
	if !ok || extras["links"] != 10 || extras["imageLinks"] != 2 {
		t.Fatalf("extras = %#v", contents["extras"])
	}
}

func TestFreshRenderingControlsFailBeforeExaWhenFreshnessIsAmbiguous(t *testing.T) {
	_, err := buildExaContents(&WebContentsInput{
		Text: &WebTextInput{Verbosity: "full"},
	}, contentDefaults{})
	if err == nil {
		t.Fatal("fresh-rendering control was accepted without max_age_hours 0")
	}
}

func TestAdditionalQueriesAreReservedForDeepSearch(t *testing.T) {
	if searchType, err := validateSearchType("deep-reasoning"); err != nil || searchType != "deep-reasoning" {
		t.Fatalf("deep-reasoning = (%q, %v)", searchType, err)
	}
	if _, err := validateSearchType("hybrid"); err == nil {
		t.Fatal("undocumented legacy search type was accepted")
	}
}
