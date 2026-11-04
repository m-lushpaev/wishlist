package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/m-lushpaev/wishlist/internal/catalog"
	"github.com/m-lushpaev/wishlist/internal/profile"
	"github.com/m-lushpaev/wishlist/internal/store"
)

//go:embed templates/*.html static/*
var webFiles embed.FS

type Config struct {
	ContentRoot, PublicHost, AdminHost string
	FamilyDisabled                     bool
}

type Server struct {
	config                                Config
	database                              *store.DB
	templates                             *template.Template
	static                                fs.FS
	limiter                               *rateLimiter
	requests, reserved, cancelled, errors atomic.Uint64
}

type wishView struct {
	Wish                     catalog.Wish
	Reserved                 bool
	CSRF, CancelToken, Error string
}

type homeView struct {
	Profile                            profile.Profile
	ProfilePath                        string
	Wishes                             []wishView
	Categories                         []string
	ActiveCategory, ActiveStatus, CSRF string
}

type reservationView struct {
	Wish                     catalog.Wish
	Token, CSRF, ProfilePath string
}

type entry struct {
	Wish           catalog.Wish
	Hidden, HasGit bool
	Revision       int64
}

type profileCatalog struct {
	Snapshot catalog.Snapshot
	Entries  []entry
}

func New(config Config, database *store.DB) (*Server, error) {
	if config.PublicHost == "" || config.AdminHost == "" || config.PublicHost == config.AdminHost {
		return nil, errors.New("distinct public and admin hosts required")
	}
	templates, err := template.New("").Funcs(template.FuncMap{"date": func(t time.Time) string { return t.Local().Format("02.01.2006 15:04") }}).ParseFS(webFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(webFiles, "static")
	if err != nil {
		return nil, err
	}
	if _, err = catalog.Load(config.ContentRoot); err != nil {
		return nil, err
	}
	return &Server{config: config, database: database, templates: templates, static: static, limiter: newRateLimiter(8, 10*time.Minute)}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	for _, p := range profile.All() {
		slug := p.Slug
		bind := func(h http.HandlerFunc) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("profile", slug); h(w, r) }
		}
		mux.HandleFunc("GET /"+slug, bind(s.profileHome))
		mux.HandleFunc("GET /"+slug+"/{$}", bind(s.canonicalProfile))
		mux.HandleFunc("POST /"+slug+"/reserve/{id}", bind(s.reserve))
	}
	mux.HandleFunc("POST /reserve/{id}", s.reserve)
	mux.HandleFunc("GET /reservation/{token}", s.reservation)
	mux.HandleFunc("POST /reservation/{token}/cancel", s.cancel)
	mux.HandleFunc("GET /admin", s.admin)
	mux.HandleFunc("GET /admin/{profile}", s.adminProfile)
	mux.HandleFunc("GET /admin/{profile}/new", s.adminNew)
	mux.HandleFunc("GET /admin/{profile}/wishes/{id}/edit", s.adminEdit)
	mux.HandleFunc("POST /admin/{profile}/wishes", s.adminSave)
	mux.HandleFunc("POST /admin/{profile}/wishes/{id}", s.adminSave)
	mux.HandleFunc("POST /admin/{profile}/wishes/{id}/hide", s.adminHide)
	mux.HandleFunc("POST /admin/{profile}/wishes/{id}/reset", s.adminReset)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(s.static)))))
	mux.HandleFunc("GET /assets/{path...}", s.asset)
	mux.HandleFunc("GET /media/{profile}/{digest}", s.media)
	mux.HandleFunc("GET /styles.css", s.styles)
	return s.logRequests(mux)
}

func (s *Server) enabled(p string) bool {
	_, ok := profile.Lookup(p)
	return ok && (!s.config.FamilyDisabled || p == "maksim")
}

func (s *Server) profilePath(p string) string {
	if s.config.FamilyDisabled && p == "maksim" {
		return "/"
	}
	return "/" + p
}

func (s *Server) effective(ctx context.Context, p string) (profileCatalog, error) {
	snapshot, err := catalog.LoadSnapshot(s.config.ContentRoot)
	if err != nil {
		return profileCatalog{}, err
	}
	overrides, err := s.database.ListOverrides(ctx, p)
	if err != nil {
		return profileCatalog{}, err
	}
	result := profileCatalog{Snapshot: snapshot}
	seen := map[string]bool{}
	for _, w := range snapshot.Wishes {
		if w.Profile != p {
			continue
		}
		e := entry{Wish: w, HasGit: true}
		if o, ok := overrides[w.ID]; ok {
			e.Wish = o.Wish
			e.Hidden = o.Hidden
			e.Revision = o.Revision
		}
		result.Entries = append(result.Entries, e)
		seen[w.ID] = true
	}
	ids := []string{}
	for id := range overrides {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := overrides[id]
		result.Entries = append(result.Entries, entry{Wish: o.Wish, Hidden: o.Hidden, Revision: o.Revision})
	}
	for _, e := range result.Entries {
		if err = catalog.Validate(snapshot.Root, e.Wish, true); err != nil {
			return profileCatalog{}, fmt.Errorf("invalid effective catalog: %w", err)
		}
	}
	return result, nil
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) {
		return
	}
	if s.config.FamilyDisabled {
		s.listProfile(w, r, "maksim")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	s.execute(w, r, "landing.html", profile.All())
}

func (s *Server) profileHome(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) {
		return
	}
	p := r.PathValue("profile")
	if !s.enabled(p) {
		http.NotFound(w, r)
		return
	}
	s.listProfile(w, r, p)
}

func (s *Server) canonicalProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) {
		return
	}
	p := r.PathValue("profile")
	if !s.enabled(p) {
		http.NotFound(w, r)
		return
	}
	target := s.profilePath(p)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

func (s *Server) listProfile(w http.ResponseWriter, r *http.Request, p string) {
	data, err := s.effective(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	reservations, err := s.database.List(r.Context(), p)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	status := r.URL.Query().Get("status")
	if status != "available" && status != "reserved" {
		status = "all"
	}
	category := r.URL.Query().Get("category")
	csrf := ensureCSRF(w, r)
	prof, _ := profile.Lookup(p)
	view := homeView{Profile: prof, ProfilePath: s.profilePath(p), ActiveStatus: status, ActiveCategory: category, CSRF: csrf}
	categories := map[string]bool{}
	for _, e := range data.Entries {
		if e.Hidden {
			continue
		}
		wish := e.Wish
		categories[wish.Category] = true
		_, reserved := reservations[wish.ID]
		if category != "" && wish.Category != category {
			continue
		}
		if status == "available" && reserved || status == "reserved" && !reserved {
			continue
		}
		view.Wishes = append(view.Wishes, wishView{Wish: wish, Reserved: reserved, CSRF: csrf})
	}
	for c := range categories {
		view.Categories = append(view.Categories, c)
	}
	sort.Strings(view.Categories)
	w.Header().Set("Cache-Control", "no-store")
	s.execute(w, r, "home.html", view)
}

func (s *Server) findWish(ctx context.Context, p, id string) (entry, bool, error) {
	data, err := s.effective(ctx, p)
	if err != nil {
		return entry{}, false, err
	}
	for _, e := range data.Entries {
		if e.Wish.ID == id {
			return e, true, nil
		}
	}
	return entry{}, false, nil
}

func (s *Server) validPost(w http.ResponseWriter, r *http.Request, host string, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme != "https" || origin.Host != host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		http.Error(w, "Недопустимый источник формы", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) {
		return
	}
	p := r.PathValue("profile")
	if p == "" {
		p = "maksim"
	}
	if !s.enabled(p) {
		http.NotFound(w, r)
		return
	}
	if !s.validPost(w, r, s.config.PublicHost, 8*1024) {
		return
	}
	if !s.limiter.Allow(clientIP(r)) {
		http.Error(w, "Слишком много попыток. Попробуйте позже.", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Некорректная форма", http.StatusBadRequest)
		return
	}
	if !validCSRF(r) {
		http.Error(w, "Форма устарела. Обновите страницу.", http.StatusForbidden)
		return
	}
	if r.Form.Get("website") != "" {
		http.Error(w, "Некорректная форма", http.StatusBadRequest)
		return
	}
	name, note := strings.TrimSpace(r.Form.Get("name")), strings.TrimSpace(r.Form.Get("note"))
	if len([]rune(name)) > 80 || len([]rune(note)) > 400 {
		http.Error(w, "Слишком длинное имя или комментарий", http.StatusBadRequest)
		return
	}
	e, found, err := s.findWish(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !found || e.Hidden {
		http.NotFound(w, r)
		return
	}
	token, err := s.database.Reserve(r.Context(), p, e.Wish.ID, e.Wish.Name, name, note)
	if errors.Is(err, store.ErrReserved) {
		s.renderWish(w, r, wishView{Wish: e.Wish, Reserved: true, CSRF: r.Form.Get("csrf"), Error: "Этот подарок уже зарезервирован."})
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.reserved.Add(1)
	slog.Info("wish reserved", "profile", p, "wish_id", e.Wish.ID, "client_ip", clientIP(r))
	if r.Header.Get("HX-Request") == "true" {
		s.renderWish(w, r, wishView{Wish: e.Wish, Reserved: true, CSRF: r.Form.Get("csrf"), CancelToken: token})
		return
	}
	http.Redirect(w, r, "/reservation/"+token, http.StatusSeeOther)
}

func (s *Server) lookupReservation(w http.ResponseWriter, r *http.Request) (store.Reservation, bool) {
	token := r.PathValue("token")
	if !validRandomToken(token) {
		http.NotFound(w, r)
		return store.Reservation{}, false
	}
	reservation, err := s.database.FindByToken(r.Context(), token)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return store.Reservation{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.Reservation{}, false
	}
	return reservation, true
}

func (s *Server) reservation(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) {
		return
	}
	reservation, ok := s.lookupReservation(w, r)
	if !ok {
		return
	}
	wish := catalog.Wish{Profile: reservation.Profile, ID: reservation.WishID, Name: reservation.WishName}
	e, found, err := s.findWish(r.Context(), reservation.Profile, reservation.WishID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if found {
		wish = e.Wish
	}
	if wish.Name == "" {
		wish.Name = "Подарок " + wish.ID
	}
	w.Header().Set("Cache-Control", "no-store")
	s.execute(w, r, "reservation.html", reservationView{Wish: wish, Token: r.PathValue("token"), CSRF: ensureCSRF(w, r), ProfilePath: s.profilePath(reservation.Profile)})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireHost(w, r, s.config.PublicHost) || !s.validPost(w, r, s.config.PublicHost, 8*1024) {
		return
	}
	if err := r.ParseForm(); err != nil || !validCSRF(r) {
		http.Error(w, "Форма устарела. Обновите страницу.", http.StatusForbidden)
		return
	}
	reservation, ok := s.lookupReservation(w, r)
	if !ok {
		return
	}
	removed, err := s.database.Cancel(r.Context(), r.PathValue("token"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !removed {
		http.NotFound(w, r)
		return
	}
	s.cancelled.Add(1)
	slog.Info("reservation cancelled", "profile", reservation.Profile, "client_ip", clientIP(r))
	target := s.profilePath(reservation.Profile)
	if !s.enabled(reservation.Profile) {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !localHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := contextWithTimeout(r, 2*time.Second)
	defer cancel()
	if err := s.database.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := catalog.Load(s.config.ContentRoot); err != nil {
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !localHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	count, err := s.database.Count(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "wishlist_http_requests_total %d\nwishlist_reservations_created_total %d\nwishlist_reservations_cancelled_total %d\nwishlist_http_errors_total %d\nwishlist_reservations_active %d\n", s.requests.Load(), s.reserved.Load(), s.cancelled.Load(), s.errors.Load(), count)
}

func (s *Server) styles(w http.ResponseWriter, r *http.Request) {
	if hostOnly(r.Host) != s.config.PublicHost && hostOnly(r.Host) != s.config.AdminHost {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, s.config.ContentRoot+"/site/styles.css")
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	if hostOnly(r.Host) != s.config.PublicHost && hostOnly(r.Host) != s.config.AdminHost {
		http.NotFound(w, r)
		return
	}
	if r.PathValue("path") == "placeholder.svg" {
		body, err := webFiles.ReadFile("static/placeholder.svg")
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
		return
	}
	root, err := filepath.EvalSymlinks(s.config.ContentRoot)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base, err := filepath.Abs(filepath.Join(root, "site", "assets"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := filepath.EvalSymlinks(filepath.Join(base, r.PathValue("path")))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, target)
}

func (s *Server) renderWish(w http.ResponseWriter, r *http.Request, view wishView) {
	w.Header().Set("Cache-Control", "no-store")
	s.execute(w, r, "wish", view)
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render failed", "template", name, "error", err, "path", safeLogPath(r))
	}
}
