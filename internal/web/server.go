// Package web is the local web interface: the same status, plan and sync as the command line,
// on a page that shows every service and pair at once. It listens only on the loopback address,
// asks for a random token, and writes to GitHub only through [workflow.Workflow], which asks for
// the exact confirmation and plans again before it does.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/app"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/config"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/domain"
	"github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/workflow"
)

//go:embed static
var staticFiles embed.FS

const (
	cookieName      = "mors_session"
	maxBodyBytes    = 64 * 1024
	requestTimeout  = 3 * time.Minute
	reportCacheFor  = 30 * time.Second
	mutationTimeout = 5 * time.Minute
)

// Server serves the interface for one manifest.
type Server struct {
	cfg      config.Config
	flow     workflow.Workflow
	reporter app.Reporter
	token    string

	// One write at a time: two applies for the same branch would race on the candidate branch.
	applying sync.Mutex
}

// NewToken makes the random secret the page is opened with.
func NewToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func New(cfg config.Config, flow workflow.Workflow, token string) *Server {
	return &Server{
		cfg:      cfg,
		flow:     flow,
		reporter: app.Reporter{Reader: flow.Reader, Inspector: flow.Inspector},
		token:    token,
	}
}

// RequireLoopback refuses an address that other machines could reach.
func RequireLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("address %q is not host:port: %w", address, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("address %q is not a loopback address; this interface can open pull requests and only listens on 127.0.0.1, ::1 or localhost", address)
	}
	return nil
}

// Handler is the whole interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(assets))
	mux.Handle("GET /", s.guard(false, files))
	mux.Handle("GET /api/manifest", s.guard(true, http.HandlerFunc(s.manifest)))
	mux.Handle("GET /api/report", s.guard(true, http.HandlerFunc(s.report)))
	mux.Handle("POST /api/sync/propose", s.guard(true, http.HandlerFunc(s.propose)))
	mux.Handle("POST /api/sync/apply", s.guard(true, http.HandlerFunc(s.apply)))
	mux.Handle("GET /api/branches", s.guard(true, http.HandlerFunc(s.branches)))
	mux.Handle("POST /api/propagate/propose", s.guard(true, http.HandlerFunc(s.proposePropagation)))
	mux.Handle("POST /api/propagate/apply", s.guard(true, http.HandlerFunc(s.applyPropagation)))
	return mux
}

// guard keeps out everything that is not the person at this machine: a Host that is not the
// loopback address (DNS rebinding), a page from another origin (CSRF) and a missing token.
func (s *Server) guard(api bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		// The first visit carries the token in the address; it becomes a cookie and leaves the URL.
		if given := r.URL.Query().Get("token"); given != "" && r.Method == http.MethodGet && !api {
			if s.validToken(given) {
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || !s.validToken(cookie.Value) {
			if api {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "open the address printed by the command line"})
				return
			}
			http.Error(w, "open the address printed by the command line (it carries the access token)", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content_type", "message": "send JSON"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validToken(given string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) == 1
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, err error) {
	if problem, ok := workflow.IsProblem(err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": problem.Code, "message": problem.Message})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "timeout", "message": "GitHub demorou demais; tente de novo"})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed", "message": err.Error()})
}

type manifestOrganization struct {
	Label string `json:"label"`
	Owner string `json:"owner"`
}

type manifestService struct {
	Name  string   `json:"name"`
	Pairs []string `json:"pairs"`
}

type manifestView struct {
	Organizations []manifestOrganization `json:"organizations"`
	Pairs         []string               `json:"pairs"`
	Services      []manifestService      `json:"services"`
	SyncBranches  []string               `json:"sync_branches"`
	Preserved     []string               `json:"preserve_only_branches"`
}

func (s *Server) manifest(w http.ResponseWriter, _ *http.Request) {
	view := manifestView{SyncBranches: s.cfg.SyncBranches, Preserved: s.cfg.PreserveOnlyBranches}
	for _, organization := range s.cfg.Organizations {
		view.Organizations = append(view.Organizations, manifestOrganization{Label: organization.Label, Owner: organization.Owner})
	}
	for _, pair := range s.cfg.Pairs() {
		view.Pairs = append(view.Pairs, pair.Name())
	}
	for _, service := range s.cfg.Services {
		entry := manifestService{Name: service, Pairs: []string{}}
		for _, pair := range s.cfg.PairsFor(service) {
			entry.Pairs = append(entry.Pairs, pair.Name())
		}
		view.Services = append(view.Services, entry)
	}
	writeJSON(w, http.StatusOK, view)
}

// report is status (cheap: only GitHub's API) or plan (fetches the histories). It can be limited
// to one service and one pair, which is what the page asks for when it analyzes a single row.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "status"
	}
	if mode != "status" && mode != "plan" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode", "message": "mode must be status or plan"})
		return
	}
	scoped := s.cfg
	if service := r.URL.Query().Get("service"); service != "" {
		pair, err := workflow.ChoosePair(s.cfg, service, r.URL.Query().Get("pair"))
		if err != nil && r.URL.Query().Get("pair") == "" {
			// A whole service: every pair it is in.
			scoped.Services = []string{service}
		} else if err != nil {
			fail(w, err)
			return
		} else {
			scoped.Services = []string{service}
			scoped.PairLabels = [][]string{{pair.Left.Label, pair.Right.Label}}
		}
	} else if name := r.URL.Query().Get("pair"); name != "" {
		pair, ok := s.cfg.Pair(name)
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown_pair", "message": fmt.Sprintf("pair %q is not configured", name)})
			return
		}
		scoped.PairLabels = [][]string{{pair.Left.Label, pair.Right.Label}}
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	report, err := s.reporter.Build(ctx, scoped, mode)
	if err != nil {
		fail(w, err)
		return
	}
	if report.Repositories == nil {
		report.Repositories = []domain.RepositoryStatus{}
	}
	writeJSON(w, http.StatusOK, report)
}

type proposeRequest struct {
	Service string `json:"service"`
	Branch  string `json:"branch"`
	Pair    string `json:"pair"`
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON body"})
		return false
	}
	return true
}

func (s *Server) propose(w http.ResponseWriter, r *http.Request) {
	var request proposeRequest
	if !decode(w, r, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	proposal, err := s.flow.Propose(ctx, s.cfg, request.Service, request.Branch, request.Pair)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var request workflow.ApplyRequest
	if !decode(w, r, &request) {
		return
	}
	if !s.applying.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "busy", "message": "outra sincronização está em andamento; espere ela terminar"})
		return
	}
	defer s.applying.Unlock()
	// A write is not cut short because the browser went away: half a push is worse than a slow one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mutationTimeout)
	defer cancel()
	results, err := s.flow.Apply(ctx, s.cfg, request)
	if err != nil {
		// Whatever was done before the failure is reported with it; nothing is rolled back.
		if problem, ok := workflow.IsProblem(err); ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": problem.Code, "message": problem.Message, "results": results})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed", "message": err.Error() + " — nenhum branch existente foi sobrescrito ou apagado; um branch candidato já enviado continua lá para revisão ou nova tentativa", "results": results})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// branches lists the branches of one organization's copy of a service that can be propagated:
// not the synchronized ones, not the preserved one, not the candidates this tool made.
func (s *Server) branches(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	label := r.URL.Query().Get("org")
	organization, ok := s.cfg.Organizations.Find(label)
	if !ok || !s.cfg.HasService(label, service) || !contains(s.cfg.Services, service) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown_source", "message": "unknown service or organization"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	snapshot, err := s.flow.Reader.ReadRepository(ctx, organization.Owner, service, organization.Label)
	if err != nil {
		fail(w, err)
		return
	}
	names := []string{}
	for name := range snapshot.Branches {
		if contains(s.cfg.SyncBranches, name) || contains(s.cfg.PreserveOnlyBranches, name) || name == "dev-test" || strings.HasPrefix(name, "sync/") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"branches": names})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type proposePropagationRequest struct {
	Service string   `json:"service"`
	Branch  string   `json:"branch"`
	Source  string   `json:"source"`
	Targets []string `json:"targets"`
	Base    string   `json:"base"`
}

func (s *Server) proposePropagation(w http.ResponseWriter, r *http.Request) {
	var request proposePropagationRequest
	if !decode(w, r, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	proposal, err := s.flow.ProposePropagation(ctx, s.cfg, request.Service, request.Branch, request.Source, request.Targets, request.Base)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (s *Server) applyPropagation(w http.ResponseWriter, r *http.Request) {
	var request workflow.PropagateRequest
	if !decode(w, r, &request) {
		return
	}
	if !s.applying.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "busy", "message": "outra operação está em andamento; espere ela terminar"})
		return
	}
	defer s.applying.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mutationTimeout)
	defer cancel()
	results, err := s.flow.ApplyPropagation(ctx, s.cfg, request)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
