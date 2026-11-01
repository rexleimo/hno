package pubmed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pubmedServer stands in for NCBI E-utilities.
//
// The tests used to call eutils.ncbi.nlm.nih.gov for real, so the suite failed whenever
// upstream answered 500 — observed on Linux CI, where these tests had never actually run
// because a lint timeout skipped the test step. A test whose verdict depends on a
// third-party service is not a test of this package.
func pubmedServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/esearch.fcgi"):
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(`<eSearchResult><Count>2</Count><IdList><Id>111</Id><Id>222</Id></IdList></eSearchResult>`))
		case strings.HasSuffix(r.URL.Path, "/esummary.fcgi"):
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(`<eSummaryResult><DocSum><Id>111</Id>
<Item Name="Title" Type="String">A study of cancer research</Item>
<Item Name="FullJournalName" Type="String">Journal of Tests</Item>
<Item Name="PubDate" Type="String">2024 Jan</Item>
<Item Name="Author" Type="String">Doe J</Item>
<Item Name="DOI" Type="String">10.1000/test</Item>
</DocSum></eSummaryResult>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestToolkit(t *testing.T, status int) *PubMedToolkit {
	t.Helper()
	tk := New()
	tk.baseURL = pubmedServer(t, status).URL
	return tk
}

func TestPubMedToolkit_SearchArticles(t *testing.T) {
	toolkit := newTestToolkit(t, http.StatusOK)

	// Test searching for articles
	result, err := toolkit.searchArticles(context.Background(), map[string]interface{}{
		"query":       "cancer research",
		"max_results": 5,
	})

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map result, got: %T", result)
	}

	// Check query
	query, ok := resultMap["query"].(string)
	if !ok {
		t.Fatalf("Expected query string, got: %T", resultMap["query"])
	}

	if query != "cancer research" {
		t.Errorf("Expected query 'cancer research', got '%s'", query)
	}

	// Check results
	results, ok := resultMap["results"].([]map[string]interface{})
	if !ok {
		t.Fatalf("Expected results array, got: %T", resultMap["results"])
	}

	// Should have some results (may be 0 if API is down, but structure should be valid)
	if len(results) > 0 {
		firstResult := results[0]

		// Check required fields
		if _, ok := firstResult["article_id"].(string); !ok {
			t.Error("Expected article_id in result")
		}
		if _, ok := firstResult["title"].(string); !ok {
			t.Error("Expected title in result")
		}
		if _, ok := firstResult["authors"].([]string); !ok {
			t.Error("Expected authors in result")
		}
	}

	// Check max results
	maxResults, ok := resultMap["max_results"].(int)
	if !ok {
		t.Fatalf("Expected max_results integer, got: %T", resultMap["max_results"])
	}

	if maxResults != 5 {
		t.Logf("Expected max_results 5, got %d (API may have returned different count)", maxResults)
	}
}

func TestPubMedToolkit_GetArticleDetails(t *testing.T) {
	toolkit := newTestToolkit(t, http.StatusOK)

	// Test getting article details (using a known article ID)
	result, err := toolkit.getArticleDetails(context.Background(), map[string]interface{}{
		"article_id": "12345678",
	})

	// This might fail if the article doesn't exist, but we should get structured error
	if err != nil {
		// Check if it's a "not found" error
		if !strings.Contains(err.Error(), "not found") {
			t.Logf("Expected article not found or API error: %v", err)
		}
		return
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected map result, got: %T", result)
	}

	article, ok := resultMap["article"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected article object, got: %T", resultMap["article"])
	}

	// Check required fields
	if _, ok := article["article_id"].(string); !ok {
		t.Error("Expected article_id in article details")
	}
	if _, ok := article["title"].(string); !ok {
		t.Error("Expected title in article details")
	}
	if _, ok := article["authors"].([]string); !ok {
		t.Error("Expected authors in article details")
	}
}

func TestPubMedToolkit_New(t *testing.T) {
	toolkit := New()

	if toolkit == nil {
		t.Fatal("Expected toolkit to be created")
	}

	// Check that functions are registered
	functions := toolkit.Functions()
	if len(functions) != 2 {
		t.Errorf("Expected 2 functions, got %d", len(functions))
	}

	expectedFunctions := []string{"search_articles", "get_article_details"}
	for _, expected := range expectedFunctions {
		found := false
		for _, function := range functions {
			if function.Name == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected function '%s' not found", expected)
		}
	}
}

// The upstream 500 the suite used to depend on is now a controlled input: this is the
// assertion that a failing API is reported as an error rather than as empty results.
func TestSearchArticlesReportsAnUpstreamFailure(t *testing.T) {
	tk := newTestToolkit(t, http.StatusInternalServerError)
	if _, err := tk.searchArticles(context.Background(), map[string]interface{}{"query": "cancer"}); err == nil {
		t.Fatal("an upstream 500 was reported as a successful search")
	}
}

// And the reverse: a served page must actually populate results, so "no network" cannot
// pass by returning an empty list.
func TestSearchArticlesReadsAServedResponse(t *testing.T) {
	tk := newTestToolkit(t, http.StatusOK)
	out, err := tk.searchArticles(context.Background(), map[string]interface{}{"query": "cancer research"})
	if err != nil {
		t.Fatalf("searchArticles: %v", err)
	}
	m, _ := out.(map[string]interface{})
	results, _ := m["results"].([]map[string]interface{})
	if len(results) == 0 {
		t.Fatal("a served esearch response produced no results")
	}
	if got, _ := results[0]["title"].(string); !strings.Contains(got, "cancer research") {
		t.Fatalf("first result title = %q, want the served title", got)
	}
}
