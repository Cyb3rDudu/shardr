package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/shardr/internal/importer"
)

// Real HF tree payload for arnir0/Tiny-LLM@b784a70a… (captured 2026-10-01,
// trimmed to the fields the anchor reads).
const realTreeBody = `[
 {"type":"file","oid":"a6344aac8c09253b3b630fb776ae94478aa0275b","size":1519,"path":".gitattributes"},
 {"type":"file","oid":"6d649332a87dc6bb7130103cc43a2d1bd1ca7a6f","size":930,"path":"Inferencecode.py"},
 {"type":"file","oid":"b26e5a0c1f71f3e86612522bc22f64217c011585","size":1394,"path":"README.md"},
 {"type":"file","oid":"2be6ef5745e1d91f59cb4c0fe3b5ba600acd4df1","size":602,"path":"config.json"},
 {"type":"file","oid":"1c67d9f21914d1d97de53bde3456550ca6598ef2","size":25979272,
  "lfs":{"oid":"7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628","size":25979272},
  "path":"model.safetensors"},
 {"type":"file","oid":"d85ba6cb6820b01226ef8bd40b46bb489041c6a8","size":411,"path":"special_tokens_map.json"},
 {"type":"file","oid":"0f8365011817d2a70d50d8a6694a031630666b28","size":1842792,"path":"tokenizer.json"},
 {"type":"file","oid":"6c00c742ce03c627d6cd5b795984876fa49fa899","size":499723,
  "lfs":{"oid":"9e556afd44213b6bd1be2b850ebbbd98f5481437a8021afaf58ee7fb1818d347","size":499723},
  "path":"tokenizer.model"},
 {"type":"file","oid":"4c576a66835747d5ed09fd965d4200bd094b2d65","size":930,"path":"tokenizer_config.json"}
]`

func testResolved() *Resolved {
	return &Resolved{
		Repo: "arnir0/Tiny-LLM", Revision: "b784a70a5e6908c9148820a245d60a3347279868",
		Infohash: "7a652fe9785fa07371656444e5b7eac301cefbf8",
	}
}

func TestResolveAnchorLiveCoversEveryFile(t *testing.T) {
	hfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/models/arnir0/Tiny-LLM/tree/b784a70a5e6908c9148820a245d60a3347279868") {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, realTreeBody)
	}))
	defer hfSrv.Close()
	hf := &importer.HFClient{BaseURL: hfSrv.URL, HTTP: hfSrv.Client()}
	a, err := ResolveAnchor(context.Background(), hf, nil, testResolved(), false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != "huggingface" || a.Rescued {
		t.Fatalf("anchor: %+v", a)
	}
	byPath := map[string]AnchorFile{}
	for _, f := range a.Files {
		byPath[f.Path] = f
	}
	if len(byPath) != 8 { // 9 tree files minus .gitattributes
		t.Fatalf("want 8 anchored files, got %d", len(byPath))
	}
	st := byPath["model.safetensors"]
	if st.SHA256 != "7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628" || st.Size != 25979272 {
		t.Fatalf("lfs anchor: %+v", st)
	}
	cfg := byPath["config.json"]
	if cfg.GitSHA1 != "2be6ef5745e1d91f59cb4c0fe3b5ba600acd4df1" || cfg.SHA256 != "" {
		t.Fatalf("non-lfs anchor: %+v", cfg)
	}
	if a.Warning() != "" {
		t.Fatalf("live anchor must carry no trust-shift warning")
	}
}

// The rescued refusal (Epic #65 trust chain 3): no HF anchor and no flag
// → loud ErrNotAnchored naming the reason and the flag. Mutation-proofing:
// deleting the refusal (or gating it on something else) turns this red.
func TestResolveAnchorRescuedRefusalWithoutFlag(t *testing.T) {
	hfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound) // HF repo removed
	}))
	defer hfSrv.Close()
	hf := &importer.HFClient{BaseURL: hfSrv.URL, HTTP: hfSrv.Client()}
	_, err := ResolveAnchor(context.Background(), hf, nil, testResolved(), false)
	if !errors.Is(err, ErrNotAnchored) {
		t.Fatalf("rescued without --trust-catalog must refuse with ErrNotAnchored, got %v", err)
	}
	if !strings.Contains(err.Error(), "--trust-catalog") || !strings.Contains(err.Error(), "rescued") {
		t.Fatalf("refusal must name the reason and the escape hatch: %v", err)
	}
}

func TestResolveAnchorRescuedTrustCatalog(t *testing.T) {
	hfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer hfSrv.Close()
	pf := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<x>{\"manifest\":\"7198d242c2903a94acc187bbaaf864637f4af8890b13b220fdf4e4af99590628  model.safetensors\n9e556afd44213b6bd1be2b850ebbbd98f5481437a8021afaf58ee7fb1818d347  tokenizer.model\"}</x>`)
	})
	hf := &importer.HFClient{BaseURL: hfSrv.URL, HTTP: hfSrv.Client()}
	a, err := ResolveAnchor(context.Background(), hf, pf, testResolved(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Rescued || a.Source != "catalog" || len(a.Files) != 2 {
		t.Fatalf("catalog anchor: %+v", a)
	}
	if !strings.Contains(a.Warning(), "trust shifts from Hugging Face to the catalog provider") {
		t.Fatalf("warning must state the trust shift loudly: %q", a.Warning())
	}
}

// A network failure is NOT a rescue: unreachable HF must propagate, never
// silently fall through to catalog trust.
func TestResolveAnchorHFUnreachableIsNotARescue(t *testing.T) {
	hfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer hfSrv.Close()
	hf := &importer.HFClient{BaseURL: hfSrv.URL, HTTP: hfSrv.Client()}
	if _, err := ResolveAnchor(context.Background(), hf, nil, testResolved(), true); err == nil || errors.Is(err, ErrNotAnchored) {
		t.Fatalf("HF 500 must be a loud error even with --trust-catalog (never a silent rescue), got %v", err)
	}
}
