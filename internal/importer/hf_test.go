package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HF request URLs are never raw-concatenated: hostile revision/file-path
// segments ("../…", ".", empty, backslash) are rejected before any HTTP
// traffic; legitimate multi-segment and unsafe-character paths are
// percent-encoded per segment.
func TestHFURLBoundary(t *testing.T) {
	c := &HFClient{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{}} // unreachable on purpose
	for _, rev := range []string{"../main", "a/../../b", "main/..", ".", "main/"} {
		if _, err := c.ListRepo(context.Background(), "org/repo", rev); err == nil || !strings.Contains(err.Error(), "invalid revision") {
			t.Fatalf("revision %q: want loud rejection, got %v", rev, err)
		}
	}
	for _, p := range []string{"../../etc/passwd", "a/../b", ".", "a//b"} {
		if _, err := c.OpenFile(context.Background(), "org/repo", "deadbeef", p); err == nil || !strings.Contains(err.Error(), "invalid file path") {
			t.Fatalf("file path %q: want loud rejection, got %v", p, err)
		}
	}
}

func TestHFURLSegmentsEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			json.NewEncoder(w).Encode(map[string]any{
				"sha":      "4ca7207fa5502f4a0f8b3d1e2c3d4e5f60718293",
				"siblings": []map[string]string{{"rfilename": "f.bin"}},
			})
			return
		}
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()
	c := &HFClient{BaseURL: srv.URL, HTTP: srv.Client()}

	if _, err := c.ListRepo(context.Background(), "org/repo", "refs/convert/parquet"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/models/org/repo/revision/refs/convert/parquet"; gotPath != want {
		t.Fatalf("listing path: got %q want %q", gotPath, want)
	}
	if _, err := c.OpenFile(context.Background(), "org/repo", "deadbeef", "my file.bin"); err != nil {
		t.Fatal(err)
	}
	if want := "/org/repo/resolve/deadbeef/my%20file.bin"; gotPath != want {
		t.Fatalf("resolve path: got %q want %q (space must be escaped)", gotPath, want)
	}
}
