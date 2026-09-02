// Package api implements shardhive's client interface per spec 005 (Draft
// v0): API v1 over a Unix domain socket (mode 0600 — the socket permission
// is the access boundary; no TCP in this slice). JSON in/out, errors are
// {"error": {"code", "message", "candidates"?}}.
//
// Slice boundary (E1.B): network sources (swarm, HF, resolver) are loud,
// explicit reservations. /resolve is pure (local state only, never a
// network fetch); /ensure only fulfills local presence; /import/* answer
// 501 E_NOT_IMPLEMENTED.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/artifact"
	"github.com/Cyb3rDudu/shardr/internal/cas"
	"github.com/Cyb3rDudu/shardr/internal/importer"
	"github.com/Cyb3rDudu/shardr/internal/ref"
	"github.com/Cyb3rDudu/shardr/internal/swarm"
)

// APIVersion is the single supported major version (005 §7).
const APIVersion = "v1"

// DefaultSocketPath resolves the daemon socket path:
// $SHARDR_SOCKET, else $XDG_RUNTIME_DIR/shardhive.sock, else
// <tmp>/shardhive-<uid>/shardhive.sock. The fallback directory is created
// with mode 0700 — the socket itself is chmod'd 0600 after listen.
// (Spec 005 Draft leaves the socket path unspecified; filed as a spec
// comment on issue #4 — not canonical until specced.)
func DefaultSocketPath() (string, error) {
	if p := os.Getenv("SHARDR_SOCKET"); p != "" {
		return filepath.Abs(p)
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "shardhive.sock"), nil
	}
	uid := "0"
	if u, err := user.Current(); err == nil {
		uid = u.Uid
	}
	dir := filepath.Join(os.TempDir(), "shardhive-"+uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "shardhive.sock"), nil
}

// Server is the shardhive daemon API on a Unix socket.
type Server struct {
	store *cas.Store
	// Swarm is the BitTorrent v2 client (004); nil disables swarm fill and
	// /import/bt with loud E_SOURCE_UNAVAILABLE / E_NOT_IMPLEMENTED errors
	// naming the config knob ([swarm] enabled).
	Swarm *swarm.Client
	// HF is the Hugging Face client; nil disables /import/hf with a loud
	// error. Tests inject a stub via this field before Listen.
	HF      *importer.HFClient
	socket  string
	ln      net.Listener
	httpSrv *http.Server

	mu   sync.Mutex
	jobs map[string]*Job
}

// Job is an ensure/fill job (005 §3). In this slice jobs are fulfilled
// synchronously — local presence only — so they are terminal immediately
// after creation (done, or failed with E_SOURCE_UNAVAILABLE naming the
// reserved sources).
type Job struct {
	ID       string    `json:"id"`
	Ref      string    `json:"ref"`
	State    string    `json:"state"` // waiting|fetching|done|failed
	Manifest string    `json:"manifest,omitempty"`
	Error    *APIError `json:"error,omitempty"`

	// Import jobs (001 §8). Jobs are immutable once published: every state
	// transition publishes a fresh instance via publishJob (atomic swap).
	Kind       string        `json:"kind,omitempty"` // import-local|import-hf|ensure
	As         string        `json:"as,omitempty"`
	FilesDone  int           `json:"filesDone,omitempty"`
	FilesTotal int           `json:"filesTotal,omitempty"`
	Result     *ImportResult `json:"result,omitempty"`
}

// ImportResult is the terminal payload of an import job.
type ImportResult struct {
	Manifests   []string `json:"manifests"`
	IndexDigest string   `json:"indexDigest"`
	Quants      []string `json:"quants"`
	Warnings    []string `json:"warnings,omitempty"`
	Skipped     int      `json:"skipped"`
	// Infohash is set for import-bt jobs (btmh:1220<hex>).
	Infohash string `json:"infohash,omitempty"`
}

// publishJob atomically replaces a job's map entry with a fresh immutable
// instance (terminal-then-publish per transition; concurrent /jobs readers
// always see a consistent snapshot).
func (s *Server) publishJob(job *Job) {
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()
}

// APIError is the wire error form (005 §3): {"code","message","candidates"?}.
type APIError struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	Candidates []string `json:"candidates,omitempty"`
}

func (e *APIError) Error() string {
	if len(e.Candidates) > 0 {
		return fmt.Sprintf("%s: %s (candidates: %s)", e.Code, e.Message, strings.Join(e.Candidates, ", "))
	}
	return e.Code + ": " + e.Message
}

// httpError pairs a wire error with an HTTP status for internal handlers.
type httpError struct {
	status int
	err    *APIError
}

// Error codes. 005 Draft does not enumerate the full class list ("error
// classes … vectors pending"); this set is the implementation proposal,
// reported as a spec comment.
const (
	ErrBadRequest         = "E_BAD_REQUEST"
	ErrInvalidRef         = "E_INVALID_REF"
	ErrUnknownRef         = "E_UNKNOWN_REF"
	ErrNoIndex            = "E_NO_INDEX"
	ErrSourceUnavail      = "E_SOURCE_UNAVAILABLE"
	ErrNotImplemented     = "E_NOT_IMPLEMENTED"
	ErrNotFound           = "E_NOT_FOUND"
	ErrUnsupportedVersion = "E_UNSUPPORTED_VERSION"
	ErrInvalidIndex       = "E_INVALID_INDEX"
	ErrRangeInvalid       = "E_RANGE_INVALID"
	ErrInternal           = "E_INTERNAL"
)

// New creates a server for the store. socket == "" → DefaultSocketPath.
func New(store *cas.Store, socket string) (*Server, error) {
	if socket == "" {
		var err error
		socket, err = DefaultSocketPath()
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		store:  store,
		HF:     importer.NewHFClient(),
		socket: socket,
		jobs:   map[string]*Job{},
	}, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.socket }

// Listen creates the Unix socket with mode 0600 (the access boundary,
// 005 §3). A path left over from a crashed daemon is removed only if it
// is a verifiably orphaned Unix socket: nothing accepts connections on it
// AND lstat says it is a socket. Regular files, directories, and symlinks
// are never touched — a wrong SHARDR_SOCKET must fail loudly, not delete
// user data.
func (s *Server) Listen() error {
	if conn, err := net.Dial("unix", s.socket); err == nil {
		conn.Close()
		return fmt.Errorf("api: socket %s is in use by another shardhive", s.socket)
	}
	if fi, err := os.Lstat(s.socket); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			kind := "regular file"
			switch {
			case fi.Mode().IsDir():
				kind = "directory"
			case fi.Mode()&fs.ModeSymlink != 0:
				kind = "symlink"
			}
			return fmt.Errorf("api: refusing to remove %s at socket path %s (not a Unix socket; unset or fix SHARDR_SOCKET/--socket)", kind, s.socket)
		}
		// Orphaned socket (no listener): safe to replace.
		if err := os.Remove(s.socket); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.ln = ln
	s.httpSrv = &http.Server{Handler: s.router()}
	return nil
}

// Serve blocks serving HTTP over the listener; returns nil after Close.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("api: listen before serve")
	}
	err := s.httpSrv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close shuts the server down.
func (s *Server) Close() error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Close()
}

func (s *Server) router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/resolve", s.handleResolve)
	mux.HandleFunc("GET /v1/open", s.handleOpen)
	mux.HandleFunc("POST /v1/ensure", s.handleEnsure)
	mux.HandleFunc("GET /v1/jobs/{id}", s.handleJob)
	mux.HandleFunc("GET /v1/blob/{digest}", s.handleBlob)
	mux.HandleFunc("POST /v1/import/local", s.handleImportLocal)
	mux.HandleFunc("POST /v1/import/hf", s.handleImportHF)
	mux.HandleFunc("POST /v1/import/bt", s.handleImportBT)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	// Everything else: version negotiation (loud) or plain 404.
	mux.HandleFunc("/", s.handleUnknown)
	return mux
}

// writeErr emits the canonical error envelope.
func writeErr(w http.ResponseWriter, status int, code, msg string, candidates ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": APIError{
		Code: code, Message: msg, Candidates: candidatesOrNil(candidates),
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func candidatesOrNil(c []string) []string {
	if len(c) == 0 {
		return nil
	}
	return c
}

func writeHTTPError(w http.ResponseWriter, he *httpError) {
	if he == nil {
		return
	}
	writeErr(w, he.status, he.err.Code, he.err.Message, he.err.Candidates...)
}

// handleUnknown implements version negotiation (005 §7): requests aimed at
// another major version are rejected loudly with the supported version;
// anything else is a plain 404.
func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	if m := versionedPath.FindStringSubmatch(r.URL.Path); m != nil && m[1] != "/"+APIVersion+"/" {
		writeErr(w, http.StatusBadRequest, ErrUnsupportedVersion,
			"unsupported API major version "+m[1]+" ("+r.URL.Path+"); this shardhive supports "+APIVersion,
			APIVersion)
		return
	}
	writeErr(w, http.StatusNotFound, ErrNotFound, "no such endpoint: "+r.URL.Path)
}

var versionedPath = regexp.MustCompile(`^(/v[0-9]+/)`)

// parseRefParam extracts and parses ?ref=. The API accepts only the
// canonical URI form (005 §1: APIs use the canonical URI — short forms
// are interactive-CLI sugar, canonicalized by the client before the
// call). Parse failures are loud; a parseable short form is reported
// back with its canonical spelling so clients can self-correct.
func parseRefParam(r *http.Request) (*ref.Ref, *httpError) {
	raw := r.URL.Query().Get("ref")
	if raw == "" {
		return nil, &httpError{http.StatusBadRequest, &APIError{Code: ErrBadRequest, Message: "missing required query parameter: ref"}}
	}
	p, rerr := ref.Parse(raw)
	if rerr != nil {
		return nil, &httpError{http.StatusBadRequest, &APIError{Code: rerr.Class, Message: shortRefHint(raw, rerr), Candidates: rerr.Candidates}}
	}
	return p, nil
}

// shortRefHint upgrades the parse error for non-canonical input: if the
// input parses as a CLI short form, the message names its canonical
// spelling; otherwise the raw parse error stands.
func shortRefHint(raw string, rerr *ref.Error) string {
	if !strings.HasPrefix(raw, ref.SchemePrefix) {
		if sp, serr := ref.ParseShort(raw); serr == nil {
			return "references at the API must be canonical URIs; use " + sp.Canonical
		}
	}
	return rerr.Message
}

// resolveResult is the /resolve and /open response base. plan stays
// "pending" until manifest parsing lands with the imports slice (001 §8)
// — reported as a spec comment on 005 §3's plan fields.
type resolveResult struct {
	Ref            string       `json:"ref"`
	NS             string       `json:"ns"`
	Name           string       `json:"name"`
	Quant          string       `json:"quant,omitempty"`
	Tag            string       `json:"tag,omitempty"`
	ManifestDigest string       `json:"manifestDigest,omitempty"`
	IndexDigest    string       `json:"indexDigest,omitempty"`
	Plan           string       `json:"plan"`
	PlanReason     string       `json:"planReason,omitempty"`
	Files          []fileRecord `json:"files,omitempty"`
	Missing        []string     `json:"missing,omitempty"`
}

type fileRecord struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

// resolveLocal resolves a parsed reference against local state only (005
// §6, step 1 of the chain: local state → configured resolvers → fail;
// resolvers are a later slice, so here resolution fails loudly instead of
// ever touching the network). Pure: no CAS writes.
func (s *Server) resolveLocal(p *ref.Ref) (*resolveResult, *httpError) {
	res := &resolveResult{
		Ref: p.Canonical, NS: p.NS, Name: p.Name,
		Quant: p.Quant, Tag: p.Tag,
		Plan:       "pending",
		PlanReason: "manifest document parsing lands with imports (001 §8)",
	}
	// @digest manifest-addressing form (000 §3.3): no state lookup needed.
	if p.Quant == "" {
		res.ManifestDigest = p.Digest
		return res, nil
	}
	var indexDigest string
	if p.Tag != "" {
		// R3 (000 §3.4): tags are scoped per repository — the state key is
		// ns/name:tag, and a tag from another repository never resolves.
		tags, err := s.store.TagAliases()
		if err != nil {
			return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "state: " + err.Error()}}
		}
		tagKey := p.NS + "/" + p.Name + ":" + p.Tag
		d, ok := tags[tagKey]
		if !ok {
			// Same loudness as an unknown repo ref: list the tags that DO
			// exist for this ns/name so the caller can self-correct.
			prefix := p.NS + "/" + p.Name + ":"
			var candidates []string
			for k := range tags {
				if strings.HasPrefix(k, prefix) {
					candidates = append(candidates, strings.TrimPrefix(k, prefix))
				}
			}
			sort.Strings(candidates)
			return nil, &httpError{http.StatusNotFound, &APIError{
				Code: ErrUnknownRef,
				Message: "no tag " + p.Tag + " for " + p.NS + "/" + p.Name +
					"; tags are scoped per repository (000 §3.4)",
				Candidates: candidates}}
		}
		indexDigest = d
	} else {
		ns, err := s.store.Namespaces()
		if err != nil {
			return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "state: " + err.Error()}}
		}
		nsKey := p.NS + "/" + p.Name
		d, ok := ns[nsKey]
		if !ok {
			// Loud unknown-ref with same-namespace candidates; resolvers are
			// a reserved source, never an implicit fallback (005 §2, §6).
			var candidates []string
			for k := range ns {
				if strings.HasPrefix(k, p.NS+"/") {
					candidates = append(candidates, k)
				}
			}
			return nil, &httpError{http.StatusNotFound, &APIError{
				Code: ErrUnknownRef,
				Message: "no local index for " + nsKey +
					"; network resolver fetch is not implemented in this shardhive build",
				Candidates: candidates}}
		}
		indexDigest = d
	}
	res.IndexDigest = indexDigest

	// Select the member from the index blob (001 model-index members).
	members, he := s.readIndexMembers(indexDigest)
	if he != nil {
		return nil, he
	}
	rr, rerr := ref.Resolve(p, members)
	if rerr != nil {
		return nil, &httpError{http.StatusBadRequest, &APIError{Code: rerr.Class, Message: rerr.Message, Candidates: rerr.Candidates}}
	}
	if rr != nil {
		res.ManifestDigest = rr.Manifest
	}
	return res, nil
}

// readIndexMembers loads the index blob and validates it via the central
// index validator in internal/artifact (the same rule set the importer's
// index merge and the spec vectors use): schemaVersion 1, artifactType
// model-index, non-empty members, quant syntax per 000 Appendix A,
// canonical sha256:<hex> manifests, unique quants. A missing index blob is
// E_NO_INDEX (loud; ensure's metadata phase would fetch it in a later
// slice), an invalid one is E_INVALID_INDEX.
//
// Trust note (no re-hash before parsing): a blob in blobs/sha256/ entered
// the store through the verifying write path (003 §3) or was re-hashed by
// an explicit verify; it is immutable by contract (0444, never mutated in
// place). Reading therefore cannot diverge from the digest — an implicit
// re-hash before every parse would duplicate verification the CAS already
// owns. Explicit re-verification remains `shardhive cas verify`.
func (s *Server) readIndexMembers(indexDigest string) ([]ref.Member, *httpError) {
	hexDigest := strings.TrimPrefix(indexDigest, ref.DigestSchemePrefix)
	if !s.store.Has(hexDigest) {
		return nil, &httpError{http.StatusNotFound, &APIError{Code: ErrNoIndex,
			Message: "index " + indexDigest + " is not present in the CAS; metadata fetch (ensure) is not implemented in this build"}}
	}
	f, err := s.store.Open(hexDigest)
	if err != nil {
		return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: err.Error()}}
	}
	defer f.Close()
	var idx artifact.Index
	if err := json.NewDecoder(f).Decode(&idx); err != nil {
		return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInvalidIndex,
			Message: "index " + indexDigest + ": not a valid model-index: " + err.Error()}}
	}
	if verr := artifact.ValidateIndex(&idx); verr != nil {
		return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInvalidIndex,
			Message: "index " + indexDigest + ": " + verr.Error()}}
	}
	members := make([]ref.Member, len(idx.Members))
	for i, m := range idx.Members {
		members[i] = ref.Member{Quant: m.Quant, Manifest: m.Manifest}
	}
	return members, nil
}

// handleResolve: pure name→digest+plan against local state (005 §3).
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	p, he := parseRefParam(r)
	if he != nil {
		writeHTTPError(w, he)
		return
	}
	res, he := s.resolveLocal(p)
	if he != nil {
		writeHTTPError(w, he)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOpen: resolve + CAS paths for present files; missing listed, never
// auto-filled (005 §3). In this slice the concretely known files are the
// index and (if resolved) manifest blobs.
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	p, he := parseRefParam(r)
	if he != nil {
		writeHTTPError(w, he)
		return
	}
	res, he := s.resolveLocal(p)
	if he != nil {
		writeHTTPError(w, he)
		return
	}
	for _, d := range []string{res.IndexDigest, res.ManifestDigest} {
		if d == "" {
			continue
		}
		hex := strings.TrimPrefix(d, ref.DigestSchemePrefix)
		path, err := s.store.BlobPath(hex)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(path); err == nil {
			res.Files = append(res.Files, fileRecord{Digest: d, Path: path, Size: fi.Size()})
		} else {
			res.Missing = append(res.Missing, d)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// handleEnsure: start a fill job (005 §3). Resolution is local-only
// (resolve stays pure); the manifest must be present. Manifest present +
// files missing → swarm fill (004 §5) as an async job; swarm disabled or
// no record link → failed E_SOURCE_UNAVAILABLE naming exactly why.
func (s *Server) handleEnsure(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "body must be JSON {\"ref\": …}: "+err.Error())
		return
	}
	if body.Ref == "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: ref")
		return
	}
	p, rerr := ref.Parse(body.Ref) // canonical form only — see parseRefParam
	if rerr != nil {
		writeErr(w, http.StatusBadRequest, rerr.Class, shortRefHint(body.Ref, rerr), rerr.Candidates...)
		return
	}
	// Compute the terminal state FIRST, publish afterwards: jobs are
	// immutable once visible, so concurrent /jobs readers can never see a
	// half-mutated Job (data race on the map entry).
	job := &Job{ID: newJobID(), Ref: p.Canonical, State: "waiting", Kind: "ensure"}
	res, he := s.resolveLocal(p)
	switch {
	case he != nil:
		job.State = "failed"
		job.Error = he.err
	default:
		job.Manifest = res.ManifestDigest
		manifestHex := strings.TrimPrefix(res.ManifestDigest, ref.DigestSchemePrefix)
		if !s.store.Has(manifestHex) {
			job.State = "failed"
			job.Error = &APIError{
				Code: ErrSourceUnavail,
				Message: "manifest " + res.ManifestDigest + " is not local; network resolver fetch is not implemented " +
					"in this build (POST /v1/import/local, /import/hf, /import/bt can bring manifests in)",
			}
			break
		}
		m, mbytes, herr := s.loadManifest(manifestHex)
		if herr != nil {
			job.State = "failed"
			job.Error = herr.err
			break
		}
		missing := swarm.MissingFiles(s.store, m)
		job.FilesTotal = len(m.Files)
		job.FilesDone = len(m.Files) - len(missing)
		if len(missing) == 0 {
			job.State = "done"
			break
		}
		if s.Swarm == nil {
			job.State = "failed"
			job.Error = &APIError{
				Code:    ErrSourceUnavail,
				Message: fmt.Sprintf("%d of %d blobs missing and the swarm client is disabled — set [swarm] enabled = true in ~/.config/shardr/config.toml (004 §7)", len(missing), len(m.Files)),
			}
			break
		}
		s.startFillJob(w, job, m, mbytes)
		return // async: job published + answered by startFillJob
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, job)
}

// loadManifest reads and validates a manifest blob from the CAS.
func (s *Server) loadManifest(hexDigest string) (*artifact.Manifest, []byte, *httpError) {
	f, err := s.store.Open(hexDigest)
	if err != nil {
		return nil, nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "manifest blob unreadable: " + err.Error()}}
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "manifest blob read: " + err.Error()}}
	}
	var m artifact.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "manifest blob " + hexDigest + " is not valid JSON (CAS corruption — never silently rebuilt): " + err.Error()}}
	}
	if err := artifact.ValidateManifest(&m); err != nil {
		return nil, nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "manifest blob " + hexDigest + " fails validation (CAS corruption): " + err.Error()}}
	}
	return &m, b, nil
}

// startFillJob publishes the waiting job, answers 201, and runs the
// swarm fill asynchronously (terminal-then-publish per transition;
// progress from the fill engine's callback).
func (s *Server) startFillJob(w http.ResponseWriter, job *Job, m *artifact.Manifest, manifestBytes []byte) {
	next := *job
	next.State = "fetching"
	s.mu.Lock()
	s.jobs[job.ID] = &next
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, &next)
	go func() {
		base := next
		err := s.Swarm.Fill(context.Background(), m, manifestBytes, swarm.Hints{},
			func(done, total int) {
				prog := base
				prog.State = "fetching"
				prog.FilesDone, prog.FilesTotal = done, total
				s.publishJob(&prog)
			})
		term := base
		if err != nil {
			term.State = "failed"
			term.Error = mapSwarmError(err)
			s.publishJob(&term)
			return
		}
		term.State = "done"
		term.FilesDone = term.FilesTotal
		s.publishJob(&term)
	}()
}

// mapSwarmError maps swarm failures to the 005 §3 inventory.
func mapSwarmError(err error) *APIError {
	switch {
	case errors.Is(err, swarm.ErrBinding):
		return &APIError{Code: "E_NOT_IMPORTABLE", Message: err.Error()}
	case errors.Is(err, swarm.ErrLayersUnavailable):
		return &APIError{Code: ErrSourceUnavail, Message: err.Error()}
	case errors.Is(err, cas.ErrDigestMismatch):
		return &APIError{Code: "E_NOT_IMPORTABLE", Message: "verify-write rejected the fetched bytes: " + err.Error()}
	default:
		return &APIError{Code: ErrSourceUnavail, Message: err.Error()}
	}
}

// handleImportBT: POST {magnet|infohash, manifestDigest, trackers?,
// webseeds?, peers?} — the pinned BT import (001 §8.7, 005 §5). The pin
// is mandatory: a magnet alone is never trusted.
func (s *Server) handleImportBT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Magnet         string   `json:"magnet"`
		Infohash       string   `json:"infohash"`
		ManifestDigest string   `json:"manifestDigest"`
		Trackers       []string `json:"trackers"`
		Webseeds       []string `json:"webseeds"`
		Peers          []string `json:"peers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "body must be JSON {magnet|infohash, manifestDigest}: "+err.Error())
		return
	}
	if body.ManifestDigest == "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: manifestDigest — the pin is mandatory; a magnet alone is never trusted (005 §5)")
		return
	}
	if body.Magnet == "" && body.Infohash == "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: magnet or infohash")
		return
	}
	if body.Magnet != "" && body.Infohash != "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "magnet and infohash are mutually exclusive")
		return
	}
	loc := body.Magnet
	if loc == "" {
		loc = body.Infohash
	}
	if s.Swarm == nil {
		writeErr(w, http.StatusNotImplemented, ErrNotImplemented,
			"swarm client disabled — set [swarm] enabled = true in ~/.config/shardr/config.toml (004 §7)")
		return
	}
	job := &Job{ID: newJobID(), Ref: loc, Kind: "import-bt", State: "waiting", Manifest: body.ManifestDigest}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, job)
	go func() {
		next := *job
		next.State = "fetching"
		s.publishJob(&next)
		res, err := s.Swarm.ImportBT(context.Background(), loc, body.ManifestDigest, swarm.Hints{
			Trackers: body.Trackers,
			Webseeds: body.Webseeds,
			Peers:    body.Peers,
		})
		term := next
		if err != nil {
			term.State = "failed"
			term.Error = mapSwarmError(err)
			s.publishJob(&term)
			return
		}
		term.State = "done"
		term.Result = &ImportResult{
			Manifests: []string{res.ManifestDigest},
			Infohash:  res.Infohash,
		}
		s.publishJob(&term)
	}()
}

// handleJob returns a job by id. The stored pointer is immutable after
// publication; the copy keeps future mutation-safety obvious.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, ErrNotFound, "no such job: "+id)
		return
	}
	jobCopy := *job
	writeJSON(w, http.StatusOK, &jobCopy)
}

// envelopeWriter rewrites any >=400 status emitted by http.ServeContent
// into the API error envelope — ServeContent's built-in 416 (and any other
// error it writes itself) is plain text otherwise, breaking the error
// contract that every API error is {"error":{code,message,candidates?}}.
type envelopeWriter struct {
	http.ResponseWriter
	errStatus int
	wrote     bool
}

func (e *envelopeWriter) WriteHeader(code int) {
	if code >= 400 {
		e.errStatus = code
		e.Header().Set("Content-Type", "application/json")
	}
	e.ResponseWriter.WriteHeader(code)
}

func (e *envelopeWriter) Write(p []byte) (int, error) {
	if e.errStatus != 0 && !e.wrote {
		e.wrote = true
		code := ErrInternal
		if e.errStatus == http.StatusRequestedRangeNotSatisfiable {
			code = ErrRangeInvalid
		}
		body, _ := json.Marshal(map[string]any{"error": APIError{
			Code: code, Message: strings.TrimSpace(string(p))}})
		_, err := e.ResponseWriter.Write(body)
		return len(p), err // report the original write length to the caller
	}
	return e.ResponseWriter.Write(p)
}

// handleBlob serves blob bytes read-through (005 §3): zero-copy, range
// support mandatory (206 valid / 416 invalid / 200 full). incoming/*.part
// is never addressable — only validated blobs/sha256 paths are.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("digest")
	hex := strings.TrimPrefix(digest, ref.DigestSchemePrefix)
	path, err := s.store.BlobPath(hex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "invalid digest "+digest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, ErrNotFound, "no such blob: "+digest)
		return
	}
	defer f.Close()
	// ServeContent implements Range/If-Range semantics incl. 206/416; the
	// envelopeWriter guarantees error responses stay in the API error form.
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(&envelopeWriter{ResponseWriter: w}, r, "", time.Time{}, f)
}

// ---------------------------------------------------------------------------
// Imports (001 §8) — local and HF are real; BT stays reserved (swarm is
// the next slice). Jobs run asynchronously; every transition publishes a
// fresh immutable Job instance.
// ---------------------------------------------------------------------------

// mapImportError maps importer sentinel outcomes to wire error classes.
func mapImportError(err error) *APIError {
	var verr *artifact.ValidationError
	switch {
	case errors.Is(err, importer.ErrNotImportable):
		return &APIError{Code: "E_NOT_IMPORTABLE", Message: err.Error()}
	case errors.Is(err, importer.ErrSourceNotRegular):
		return &APIError{Code: "E_SOURCE_NOT_REGULAR", Message: err.Error()}
	case errors.As(err, &verr):
		// A 001 rule violation surfacing in a job (e.g. corrupt current
		// index) maps to the same class the resolve path reports it under.
		return &APIError{Code: ErrInvalidIndex, Message: err.Error()}
	case errors.Is(err, importer.ErrRateLimited):
		return &APIError{Code: "E_RATE_LIMITED", Message: err.Error()}
	case errors.Is(err, importer.ErrForbidden):
		return &APIError{Code: "E_SOURCE_FORBIDDEN", Message: err.Error()}
	case errors.Is(err, importer.ErrUnknownRepo):
		return &APIError{Code: "E_UNKNOWN_REF", Message: err.Error()}
	case errors.Is(err, importer.ErrHFUnreachable):
		return &APIError{Code: ErrSourceUnavail, Message: err.Error()}
	default:
		return &APIError{Code: ErrInternal, Message: err.Error()}
	}
}

// handleImportLocal: POST {paths[], as} — namespace is REQUIRED (001 §8.6).
func (s *Server) handleImportLocal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
		As    string   `json:"as"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "body must be JSON {\"paths\":[…],\"as\":\"ns/name\"}: "+err.Error())
		return
	}
	if len(body.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: paths (non-empty array)")
		return
	}
	if body.As == "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: as (namespace, 001 §8.6 — never optional)")
		return
	}
	if p, rerr := ref.Parse(ref.SchemePrefix + body.As + ":q8_0"); rerr != nil || p.NS+"/"+p.Name != body.As {
		_ = p
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "field as must be a well-formed ns/name per spec 000")
		return
	}
	sources, err := importer.LocalSources(body.Paths)
	if err != nil {
		if errors.Is(err, importer.ErrSourceNotRegular) {
			writeErr(w, http.StatusBadRequest, "E_SOURCE_NOT_REGULAR", err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "paths: "+err.Error())
		return
	}

	job := &Job{ID: newJobID(), Kind: "import-local", Ref: body.As, As: body.As, State: "waiting", FilesTotal: len(sources)}
	s.publishJob(job)
	go s.runImport(job, func(progress func(int, int)) (*importer.ImportResult, error) {
		// Background context: the request dies with the 201 response;
		// job cancellation support lands with job control.
		return importer.Import(context.Background(), s.store, sources, importer.ImportOptions{
			As: body.As, Progress: progress,
		})
	})
	writeJSON(w, http.StatusCreated, job)
}

// handleImportHF: POST {repo, revision?} — same classification, same
// convergence; revision is pinned to the resolved commit SHA (001 §8.8).
func (s *Server) handleImportHF(w http.ResponseWriter, r *http.Request) {
	if s.HF == nil {
		writeErr(w, http.StatusServiceUnavailable, ErrSourceUnavail, "HF client disabled")
		return
	}
	var body struct {
		Repo     string `json:"repo"`
		Revision string `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "body must be JSON {\"repo\":…,\"revision\":?}: "+err.Error())
		return
	}
	if body.Repo == "" {
		writeErr(w, http.StatusBadRequest, ErrBadRequest, "missing required field: repo")
		return
	}
	info, err := s.HF.ListRepo(r.Context(), body.Repo, body.Revision)
	if err != nil {
		he := mapImportError(err)
		writeErr(w, http.StatusBadGateway, he.Code, he.Message)
		return
	}
	// B2: pin every byte fetch to the RESOLVED commit SHA, never the
	// mutable branch — otherwise a branch move between listing and fetch
	// publishes bytes from commit Y under the provenance of commit X
	// (001 §8.8 pinning, §7.5 convergence).
	pinned := info.CommitSHA
	if pinned == "" {
		pinned = body.Revision // API did not resolve a SHA; pin the named revision
		if pinned == "" {
			pinned = "main"
		}
	}
	ns := strings.ToLower(body.Repo) // ns/name from the repo id (original case preserved in annotations)
	var sources []importer.Source
	for _, f := range info.Files {
		path := f
		sources = append(sources, importer.Source{Name: path, Open: func() (io.ReadCloser, error) {
			// Background context: the request dies with the 201 response.
			return s.HF.OpenFile(context.Background(), body.Repo, pinned, path)
		}})
	}

	job := &Job{ID: newJobID(), Kind: "import-hf", Ref: body.Repo, As: ns, State: "waiting", FilesTotal: len(sources)}
	s.publishJob(job)
	go s.runImport(job, func(progress func(int, int)) (*importer.ImportResult, error) {
		return importer.Import(context.Background(), s.store, sources, importer.ImportOptions{
			As: ns, HFRepo: body.Repo, HFRevision: pinned, Progress: progress,
		})
	})
	writeJSON(w, http.StatusCreated, job)
}

// runImport executes an import with progress-driven job transitions and a
// terminal publish. The context of the originating request is dead by the
// time the goroutine runs — cancellation support lands with job control.
func (s *Server) runImport(job *Job, run func(progress func(int, int)) (*importer.ImportResult, error)) {
	next := *job
	next.State = "fetching"
	s.publishJob(&next)
	res, err := run(func(done, total int) {
		prog := *s.currentJob(job.ID)
		prog.State = "fetching"
		prog.FilesDone, prog.FilesTotal = done, total
		s.publishJob(&prog)
	})
	term := *job
	term.FilesDone = term.FilesTotal
	if err != nil {
		term.State = "failed"
		term.Error = mapImportError(err)
		s.publishJob(&term)
		return
	}
	term.State = "done"
	term.Result = &ImportResult{
		IndexDigest: res.IndexDigest,
		Warnings:    res.Warnings,
		Skipped:     res.Skipped,
	}
	for _, m := range res.Members {
		term.Result.Manifests = append(term.Result.Manifests, m.Manifest)
		term.Result.Quants = append(term.Result.Quants, m.Quant)
		if term.Manifest == "" {
			term.Manifest = m.Manifest
		}
	}
	s.publishJob(&term)
}

// currentJob returns the current published instance (or the given fallback).
func (s *Server) currentJob(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		return j
	}
	return &Job{ID: id}
}

// handleImportReserved: 001 §8 imports are the next slice — loud, explicit
// reservation, never a silent no-op.
func (s *Server) handleImportReserved(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, ErrNotImplemented,
		r.Method+" "+r.URL.Path+" is reserved: imports land in the next slice (001 §8)")
}

// handleModels: inventory from state + CAS presence (005 §3). This is a
// SKELETON inventory: quants come from the current index when it is
// present and valid, but seed state and sizes land with later slices —
// the response says so explicitly instead of posing as complete.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ns, err := s.store.Namespaces()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, ErrInternal, "state: "+err.Error())
		return
	}
	tags, err := s.store.TagAliases()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, ErrInternal, "state: "+err.Error())
		return
	}
	type nsEntry struct {
		NS           string   `json:"ns"`
		Name         string   `json:"name"`
		IndexDigest  string   `json:"indexDigest"`
		IndexPresent bool     `json:"indexPresent"`
		Quants       []string `json:"quants,omitempty"`
	}
	type tagEntry struct {
		Repo        string `json:"repo"` // ns/name the tag is scoped to (R3)
		Tag         string `json:"tag"`
		Digest      string `json:"digest"`
		BlobPresent bool   `json:"blobPresent"`
	}
	out := struct {
		Skeleton   bool       `json:"skeleton"`
		Note       string     `json:"note"`
		Namespaces []nsEntry  `json:"namespaces"`
		Tags       []tagEntry `json:"tags"`
	}{
		Skeleton:   true,
		Note:       "skeleton inventory: seed state and sizes land with later slices (005 §3)",
		Namespaces: []nsEntry{}, Tags: []tagEntry{},
	}
	for k, d := range ns {
		e := nsEntry{IndexDigest: d}
		e.NS, e.Name, _ = strings.Cut(k, "/")
		e.IndexPresent = s.store.Has(strings.TrimPrefix(d, ref.DigestSchemePrefix))
		if e.IndexPresent {
			if members, he := s.readIndexMembers(d); he == nil {
				for _, m := range members {
					e.Quants = append(e.Quants, m.Quant)
				}
				sort.Strings(e.Quants)
			}
		}
		out.Namespaces = append(out.Namespaces, e)
	}
	sort.Slice(out.Namespaces, func(i, j int) bool {
		return out.Namespaces[i].NS+"/"+out.Namespaces[i].Name < out.Namespaces[j].NS+"/"+out.Namespaces[j].Name
	})
	for k, d := range tags {
		repo, tag, _ := strings.Cut(k, ":")
		out.Tags = append(out.Tags, tagEntry{Repo: repo, Tag: tag, Digest: d,
			BlobPresent: s.store.Has(strings.TrimPrefix(d, ref.DigestSchemePrefix))})
	}
	sort.Slice(out.Tags, func(i, j int) bool {
		return out.Tags[i].Repo+":"+out.Tags[i].Tag < out.Tags[j].Repo+":"+out.Tags[j].Tag
	})
	writeJSON(w, http.StatusOK, out)
}

func newJobID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
