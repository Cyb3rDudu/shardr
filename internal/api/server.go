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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Cyb3rDudu/shardr/internal/cas"
	"github.com/Cyb3rDudu/shardr/internal/ref"
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
	store   *cas.Store
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
		socket: socket,
		jobs:   map[string]*Job{},
	}, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.socket }

// Listen creates the Unix socket with mode 0600 (the access boundary,
// 005 §3). A socket file left by a crashed daemon is removed only if
// nothing accepts connections on it.
func (s *Server) Listen() error {
	if conn, err := net.Dial("unix", s.socket); err == nil {
		conn.Close()
		return fmt.Errorf("api: socket %s is in use by another shardhive", s.socket)
	}
	if err := os.Remove(s.socket); err != nil && !os.IsNotExist(err) {
		return err
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
	mux.HandleFunc("POST /v1/import/local", s.handleImportReserved)
	mux.HandleFunc("POST /v1/import/hf", s.handleImportReserved)
	mux.HandleFunc("POST /v1/import/bt", s.handleImportReserved)
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

// parseRefParam extracts and parses ?ref= (005 §3 accepts short refs; the
// canonical URI is also accepted). Parse failures are loud with candidates.
func parseRefParam(r *http.Request) (*ref.Ref, *httpError) {
	raw := r.URL.Query().Get("ref")
	if raw == "" {
		return nil, &httpError{http.StatusBadRequest, &APIError{Code: ErrBadRequest, Message: "missing required query parameter: ref"}}
	}
	p, rerr := ref.ParseAny(raw)
	if rerr != nil {
		return nil, &httpError{http.StatusBadRequest, &APIError{Code: ErrInvalidRef, Message: rerr.Message, Candidates: rerr.Candidates}}
	}
	return p, nil
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
		tags, err := s.store.TagAliases()
		if err != nil {
			return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal, Message: "state: " + err.Error()}}
		}
		d, ok := tags[p.Tag]
		if !ok {
			return nil, &httpError{http.StatusBadRequest, &APIError{
				Code: ErrInvalidRef, Message: "unknown tag " + p.Tag, Candidates: []string{ref.ErrUnknownTag}}}
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

// indexMemberDoc is the minimal 001 model-index reader this slice needs:
// artifactType "model-index" with members[{quant, manifest}].
type indexMemberDoc struct {
	ArtifactType string       `json:"artifactType"`
	Members      []ref.Member `json:"members"`
}

// readIndexMembers loads the index blob and decodes its members. A missing
// index blob is E_NO_INDEX (loud; ensure's metadata phase would fetch it in
// a later slice), a malformed one is E_INTERNAL.
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
	var doc indexMemberDoc
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal,
			Message: "index " + indexDigest + ": not a valid model-index: " + err.Error()}}
	}
	if doc.ArtifactType != "model-index" {
		return nil, &httpError{http.StatusInternalServerError, &APIError{Code: ErrInternal,
			Message: "index " + indexDigest + ": artifactType " + doc.ArtifactType + " != model-index"}}
	}
	return doc.Members, nil
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

// handleEnsure: start a fill job. Slice semantics (005 §3, E1.B): local
// presence only — resolved manifest present → done; anything missing →
// failed with E_SOURCE_UNAVAILABLE naming the reserved sources. Importers
// and the swarm client are later slices.
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
	p, rerr := ref.ParseAny(body.Ref)
	if rerr != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidRef, rerr.Message, rerr.Candidates...)
		return
	}
	job := &Job{ID: newJobID(), Ref: p.Canonical, State: "waiting"}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.mu.Unlock()

	// Fulfil synchronously: local presence only.
	res, he := s.resolveLocal(p)
	if he != nil {
		job.State = "failed"
		job.Error = he.err
		writeJSON(w, http.StatusCreated, job)
		return
	}
	job.Manifest = res.ManifestDigest
	if s.store.Has(strings.TrimPrefix(res.ManifestDigest, ref.DigestSchemePrefix)) {
		job.State = "done"
	} else {
		job.State = "failed"
		job.Error = &APIError{
			Code: ErrSourceUnavail,
			Message: "manifest " + res.ManifestDigest + " is not local; import/swarm sources are not implemented " +
				"in this build (reserved: POST /v1/import/local, POST /v1/import/hf, POST /v1/import/bt, swarm 004)",
		}
	}
	writeJSON(w, http.StatusCreated, job)
}

// handleJob returns a job by id.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, ErrNotFound, "no such job: "+id)
		return
	}
	writeJSON(w, http.StatusOK, job)
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
	// ServeContent implements Range/If-Range semantics incl. 206/416.
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// handleImportReserved: 001 §8 imports are the next slice — loud, explicit
// reservation, never a silent no-op.
func (s *Server) handleImportReserved(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, ErrNotImplemented,
		r.Method+" "+r.URL.Path+" is reserved: imports land in the next slice (001 §8)")
}

// handleModels: inventory from state + CAS presence (005 §3).
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
		NS           string `json:"ns"`
		Name         string `json:"name"`
		IndexDigest  string `json:"indexDigest"`
		IndexPresent bool   `json:"indexPresent"`
	}
	type tagEntry struct {
		Alias       string `json:"alias"`
		Digest      string `json:"digest"`
		BlobPresent bool   `json:"blobPresent"`
	}
	out := struct {
		Namespaces []nsEntry  `json:"namespaces"`
		Tags       []tagEntry `json:"tags"`
	}{Namespaces: []nsEntry{}, Tags: []tagEntry{}}
	for k, d := range ns {
		e := nsEntry{IndexDigest: d}
		e.NS, e.Name, _ = strings.Cut(k, "/")
		e.IndexPresent = s.store.Has(strings.TrimPrefix(d, ref.DigestSchemePrefix))
		out.Namespaces = append(out.Namespaces, e)
	}
	for a, d := range tags {
		out.Tags = append(out.Tags, tagEntry{Alias: a, Digest: d,
			BlobPresent: s.store.Has(strings.TrimPrefix(d, ref.DigestSchemePrefix))})
	}
	writeJSON(w, http.StatusOK, out)
}

func newJobID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
