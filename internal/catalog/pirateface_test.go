package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fixtures are REAL bytes copied from the live API on 2026-10-01 (Epic
// #65 provider facts). The webseed+revision extraction is the core
// behavior under test — if the provider changes its line format, these
// tests are the tripwire.

const realSearchBody = "arnir0/Tiny-LLM  26 MB  1 seeds  magnet:?xt=urn:btih:7a652fe9785fa07371656444e5b7eac301cefbf8&tr=udp%3A%2F%2Ftracker.pirateface.co%3A6969%2Fannounce&tr=http%3A%2F%2Ftracker.pirateface.co%3A6969%2Fannounce&ws=https%3A%2F%2Fpirateface.co%2Fapi%2Fws%2Farnir0%2FTiny-LLM%2Fb784a70a5e6908c9148820a245d60a3347279868%2F\n"

// realModelPageHead is the model-page payload around the checksum record
// prop, copied from GET /arnir0/Tiny-LLM (RSC payload embedded in HTML;
// quotes escaped as \", record lines joined by a literal \n escape).
const realModelPageSnippet = `directUrl":null}],[\"$\",\"$L1d\",null,{\"manifest\":\"7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628  model.safetensors\n9e556afd44213b6bd1be2b850ebbbd98f5481437a8021afaf58ee7fb1818d347  tokenizer.model\",\"count\":2}]]`

func TestParseSearchLineReal(t *testing.T) {
	ln := strings.TrimRight(realSearchBody, "\n")
	m, err := ParseSearchLine(ln)
	if err != nil {
		t.Fatal(err)
	}
	if m.Repo != "arnir0/Tiny-LLM" || m.Size != "26 MB" || m.Seeds != 1 {
		t.Fatalf("fields: %+v", m)
	}
	if !strings.HasPrefix(m.Magnet, "magnet:?xt=urn:btih:7a652fe9785fa07371656444e5b7eac301cefbf8") {
		t.Fatalf("magnet: %s", m.Magnet)
	}
}

func TestParseSearchLineSingularSeedAndRejections(t *testing.T) {
	if m, err := ParseSearchLine("a/b  1 MB  0 seeds  magnet:?xt=urn:btih:" + strings.Repeat("a", 40)); err != nil || m.Seeds != 0 {
		t.Fatalf("0 seeds: %+v %v", m, err)
	}
	for _, bad := range []string{
		"arnir0/Tiny-LLM 26 MB 1 seeds magnet:x", // single spaces
		"no-slash  1 MB  1 seeds  magnet:?xt=urn:btih:" + strings.Repeat("a", 40),
		"a/b  1 MB  x seeds  magnet:?xt=urn:btih:" + strings.Repeat("a", 40),
		"a/b  1 MB  1 seeds  not-a-magnet",
		"a/b  1 MB  1 seeds  magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "  extra",
	} {
		if _, err := ParseSearchLine(bad); err == nil {
			t.Fatalf("line %q must be rejected", bad)
		}
	}
}

func TestFromListingWebseedRevisionExtraction(t *testing.T) {
	m, err := ParseSearchLine(strings.TrimRight(realSearchBody, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := FromListing(m)
	if err != nil {
		t.Fatal(err)
	}
	if r.Repo != "arnir0/Tiny-LLM" {
		t.Fatalf("repo: %s", r.Repo)
	}
	if r.Revision != "b784a70a5e6908c9148820a245d60a3347279868" {
		t.Fatalf("revision from webseed: %s", r.Revision)
	}
	if r.Infohash != "7a652fe9785fa07371656444e5b7eac301cefbf8" {
		t.Fatalf("infohash: %s", r.Infohash)
	}
	if len(r.Trackers) != 2 || r.Trackers[0] != "udp://tracker.pirateface.co:6969/announce" {
		t.Fatalf("trackers: %v", r.Trackers)
	}
	if len(r.Webseeds) != 1 || !strings.HasPrefix(r.Webseeds[0], "https://pirateface.co/api/ws/arnir0/Tiny-LLM/b784a70a") {
		t.Fatalf("webseeds: %v", r.Webseeds)
	}
}

func TestFromListingRejectsAnchorlessMagnet(t *testing.T) {
	m := &Model{Repo: "a/b", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}
	if _, err := FromListing(m); err == nil {
		t.Fatal("magnet without webseed/revision must be refused — no anchor is derivable")
	}
}

func TestParseChecksumRecordReal(t *testing.T) {
	cs, err := ParseChecksumRecord(realModelPageSnippet)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 checksums, got %d", len(cs))
	}
	if cs[0].Path != "model.safetensors" || cs[0].SHA256 != "7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628" {
		t.Fatalf("cs[0]: %+v", cs[0])
	}
	if cs[1].Path != "tokenizer.model" || cs[1].SHA256 != "9e556afd44213b6bd1be2b850ebbbd98f5481437a8021afaf58ee7fb1818d347" {
		t.Fatalf("cs[1]: %+v", cs[1])
	}
}

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Pirateface {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Pirateface{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestSearchAndResolveOverHTTP(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if ep := r.URL.EscapedPath(); ep == "/s/arnir0%2FTiny-LLM" || ep == "/s/tinyllm" {
			fmt.Fprint(w, realSearchBody)
			return
		}
		http.Error(w, "no route "+r.URL.Path, http.StatusNotFound)
	})
	models, err := p.Search(context.Background(), "arnir0/Tiny-LLM")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Repo != "arnir0/Tiny-LLM" {
		t.Fatalf("models: %+v", models)
	}
	r, err := p.Resolve(context.Background(), "arnir0/Tiny-LLM")
	if err != nil {
		t.Fatal(err)
	}
	if r.Revision != "b784a70a5e6908c9148820a245d60a3347279868" {
		t.Fatalf("resolve: %+v", r)
	}
	// Resolve for an unlisted repo: search answers, no exact match → loud.
	if _, err := p.Resolve(context.Background(), "other/repo"); err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("unlisted resolve must fail loudly, got %v", err)
	}
}

func TestChecksumsOverHTTP(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/arnir0/Tiny-LLM" {
			fmt.Fprint(w, `<html><script>self.__next_f.push([1,"x:[\"$\",\"$L1d\",null,{\"manifest\":\"`+
				`7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628  model.safetensors\n`+
				`9e556afd44213b6bd1be2b850ebbbd98f5481437a8021afaf58ee7fb1818d347  tokenizer.model\",\"count\":2}"])</script></html>`)
			return
		}
		http.Error(w, "no route", http.StatusNotFound)
	})
	cs, err := p.Checksums(context.Background(), "arnir0/Tiny-LLM")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Path != "model.safetensors" {
		t.Fatalf("checksums: %+v", cs)
	}
	// 404 model page → ErrNotListed, not a parse error.
	if _, err := p.Checksums(context.Background(), "gone/repo"); err == nil {
		t.Fatal("404 page must be an error")
	}
}

func TestUnreachableProvider(t *testing.T) {
	p := &Pirateface{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{}}
	if _, err := p.Search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("want unreachable error, got %v", err)
	}
}
