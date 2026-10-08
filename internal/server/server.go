package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m-lushpaev/wishlist/internal/catalog"
	"github.com/m-lushpaev/wishlist/internal/store"
)

//go:embed templates/*.html static/*
var webFiles embed.FS

type Config struct {
	ContentRoot string
	PublicHost  string
	AdminHost   string
}

type Server struct {
	config    Config
	database  *store.DB
	templates *template.Template
	static    fs.FS
	limiter   *rateLimiter
	requests  atomic.Uint64
	reserved  atomic.Uint64
	cancelled atomic.Uint64
	errors    atomic.Uint64
}

type wishView struct {
	Wish        catalog.Wish
	Reserved    bool
	CSRF        string
	CancelToken string
	Error       string
}

type homeView struct {
	Wishes         []wishView
	Categories     []string
	ActiveCategory string
	ActiveStatus   string
	CSRF           string
}

type reservationView struct {
	Wish  catalog.Wish
	Token string
	CSRF  string
}

type adminReservation struct {
	WishName  string
	WishID    string
	CreatedAt time.Time
}

type adminView struct {
	User         string
	TotalWishes  int
	Available    int
	Reservations []adminReservation
}

func New(config Config, database *store.DB) (*Server, error) {
	templates, err := template.New("").Funcs(template.FuncMap{
		"date": func(value time.Time) string { return value.Local().Format("02.01.2006 15:04") },
	}).ParseFS(webFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(webFiles, "static")
	if err != nil {
		return nil, err
	}
	if _, err := catalog.Load(config.ContentRoot); err != nil {
		return nil, err
	}
	return &Server{
		config: config, database: database, templates: templates, static: static,
		limiter: newRateLimiter(8, 10*time.Minute),
	}, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.home)
	mux.HandleFunc("POST /reserve/{id}", server.reserve)
	mux.HandleFunc("GET /reservation/{token}", server.reservation)
	mux.HandleFunc("POST /reservation/{token}/cancel", server.cancel)
	mux.HandleFunc("GET /admin", server.admin)
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /metrics", server.metrics)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(server.static)))))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", cacheStatic(http.FileServer(http.Dir(server.config.ContentRoot+"/site/assets")))))
	mux.HandleFunc("GET /styles.css", server.styles)
	return server.logRequests(mux)
}

func (server *Server) home(response http.ResponseWriter, request *http.Request) {
	if !server.requireHost(response, request, server.config.PublicHost) {
		return
	}
	if request.URL.Path != "/" {
		http.NotFound(response, request)
		return
	}
	wishes, err := catalog.Load(server.config.ContentRoot)
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	reservations, err := server.database.List(request.Context())
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	status := request.URL.Query().Get("status")
	if status != "available" && status != "reserved" {
		status = "all"
	}
	category := request.URL.Query().Get("category")
	csrf := ensureCSRF(response, request)
	view := homeView{ActiveCategory: category, ActiveStatus: status, CSRF: csrf}
	categorySet := map[string]bool{}
	for _, wish := range wishes {
		categorySet[wish.Category] = true
		_, isReserved := reservations[wish.ID]
		if category != "" && wish.Category != category {
			continue
		}
		if status == "available" && isReserved || status == "reserved" && !isReserved {
			continue
		}
		view.Wishes = append(view.Wishes, wishView{Wish: wish, Reserved: isReserved, CSRF: csrf})
	}
	for category := range categorySet {
		view.Categories = append(view.Categories, category)
	}
	sort.Strings(view.Categories)
	response.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodHead {
		response.WriteHeader(http.StatusOK)
		return
	}
	server.execute(response, request, "home.html", view)
}

func (server *Server) reserve(response http.ResponseWriter, request *http.Request) {
	if !server.requireHost(response, request, server.config.PublicHost) || !server.validPost(response, request) {
		return
	}
	if !server.limiter.Allow(clientIP(request)) {
		http.Error(response, "Слишком много попыток. Попробуйте позже.", http.StatusTooManyRequests)
		return
	}
	if err := request.ParseForm(); err != nil {
		http.Error(response, "Некорректная форма", http.StatusBadRequest)
		return
	}
	if !validCSRF(request) {
		http.Error(response, "Форма устарела. Обновите страницу.", http.StatusForbidden)
		return
	}
	if request.Form.Get("website") != "" {
		http.Error(response, "Некорректная форма", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(request.Form.Get("name"))
	note := strings.TrimSpace(request.Form.Get("note"))
	if len([]rune(name)) > 80 || len([]rune(note)) > 400 {
		http.Error(response, "Слишком длинное имя или комментарий", http.StatusBadRequest)
		return
	}
	wish, found, err := server.findWish(request.PathValue("id"))
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	if !found {
		http.NotFound(response, request)
		return
	}
	token, err := server.database.Reserve(request.Context(), wish.ID, name, note)
	if errors.Is(err, store.ErrReserved) {
		server.renderWish(response, request, wishView{Wish: wish, Reserved: true, CSRF: request.Form.Get("csrf"), Error: "Этот подарок уже зарезервирован."})
		return
	}
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	server.reserved.Add(1)
	slog.Info("wish reserved", "wish_id", wish.ID, "client_ip", clientIP(request))
	if request.Header.Get("HX-Request") == "true" {
		server.renderWish(response, request, wishView{Wish: wish, Reserved: true, CSRF: request.Form.Get("csrf"), CancelToken: token})
		return
	}
	http.Redirect(response, request, "/reservation/"+token, http.StatusSeeOther)
}

func (server *Server) reservation(response http.ResponseWriter, request *http.Request) {
	if !server.requireHost(response, request, server.config.PublicHost) {
		return
	}
	token := request.PathValue("token")
	reservation, err := server.database.FindByToken(request.Context(), token)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	wish, found, err := server.findWish(reservation.WishID)
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	if !found {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	server.execute(response, request, "reservation.html", reservationView{Wish: wish, Token: token, CSRF: ensureCSRF(response, request)})
}

func (server *Server) cancel(response http.ResponseWriter, request *http.Request) {
	if !server.requireHost(response, request, server.config.PublicHost) || !server.validPost(response, request) {
		return
	}
	if err := request.ParseForm(); err != nil || !validCSRF(request) {
		http.Error(response, "Форма устарела. Обновите страницу.", http.StatusForbidden)
		return
	}
	removed, err := server.database.Cancel(request.Context(), request.PathValue("token"))
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	if !removed {
		http.NotFound(response, request)
		return
	}
	server.cancelled.Add(1)
	slog.Info("reservation cancelled", "client_ip", clientIP(request))
	http.Redirect(response, request, "/", http.StatusSeeOther)
}

func (server *Server) admin(response http.ResponseWriter, request *http.Request) {
	if !server.requireHost(response, request, server.config.AdminHost) {
		return
	}
	if !hasGroup(request.Header.Get("Remote-Groups"), "admins") {
		http.Error(response, "Forbidden", http.StatusForbidden)
		return
	}
	wishes, err := catalog.Load(server.config.ContentRoot)
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	reservations, err := server.database.List(request.Context())
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	names := map[string]string{}
	for _, wish := range wishes {
		names[wish.ID] = wish.Name
	}
	view := adminView{User: request.Header.Get("Remote-User"), TotalWishes: len(wishes), Available: len(wishes) - len(reservations)}
	for _, reservation := range reservations {
		view.Reservations = append(view.Reservations, adminReservation{WishName: names[reservation.WishID], WishID: reservation.WishID, CreatedAt: reservation.CreatedAt})
	}
	response.Header().Set("Cache-Control", "no-store")
	server.execute(response, request, "admin.html", view)
}

func (server *Server) health(response http.ResponseWriter, request *http.Request) {
	if !localHost(request.Host) {
		http.NotFound(response, request)
		return
	}
	ctx, cancel := contextWithTimeout(request, 2*time.Second)
	defer cancel()
	if err := server.database.Ping(ctx); err != nil {
		http.Error(response, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := catalog.Load(server.config.ContentRoot); err != nil {
		http.Error(response, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	json.NewEncoder(response).Encode(map[string]string{"status": "ok"})
}

func (server *Server) metrics(response http.ResponseWriter, request *http.Request) {
	if !localHost(request.Host) {
		http.NotFound(response, request)
		return
	}
	reservations, err := server.database.List(request.Context())
	if err != nil {
		server.internalError(response, request, err)
		return
	}
	response.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(response, "wishlist_http_requests_total %d\n", server.requests.Load())
	fmt.Fprintf(response, "wishlist_reservations_created_total %d\n", server.reserved.Load())
	fmt.Fprintf(response, "wishlist_reservations_cancelled_total %d\n", server.cancelled.Load())
	fmt.Fprintf(response, "wishlist_http_errors_total %d\n", server.errors.Load())
	fmt.Fprintf(response, "wishlist_reservations_active %d\n", len(reservations))
}

func (server *Server) styles(response http.ResponseWriter, request *http.Request) {
	host := hostOnly(request.Host)
	if host != server.config.PublicHost && host != server.config.AdminHost {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", "text/css; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(response, request, server.config.ContentRoot+"/site/styles.css")
}

func (server *Server) findWish(id string) (catalog.Wish, bool, error) {
	wishes, err := catalog.Load(server.config.ContentRoot)
	if err != nil {
		return catalog.Wish{}, false, err
	}
	for _, wish := range wishes {
		if wish.ID == id {
			return wish, true, nil
		}
	}
	return catalog.Wish{}, false, nil
}

func (server *Server) renderWish(response http.ResponseWriter, request *http.Request, view wishView) {
	response.Header().Set("Cache-Control", "no-store")
	server.execute(response, request, "wish", view)
}

func (server *Server) execute(response http.ResponseWriter, request *http.Request, name string, data any) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := server.templates.ExecuteTemplate(response, name, data); err != nil {
		slog.Error("render failed", "template", name, "error", err, "path", request.URL.Path)
	}
}

func (server *Server) validPost(response http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(response, request.Body, 8*1024)
	origin := request.Header.Get("Origin")
	if origin == "" {
		http.Error(response, "Missing Origin", http.StatusForbidden)
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != server.config.PublicHost {
		http.Error(response, "Invalid Origin", http.StatusForbidden)
		return false
	}
	return true
}

func (server *Server) requireHost(response http.ResponseWriter, request *http.Request, expected string) bool {
	if hostOnly(request.Host) != expected {
		http.NotFound(response, request)
		return false
	}
	return true
}

func (server *Server) internalError(response http.ResponseWriter, request *http.Request, err error) {
	server.errors.Add(1)
	slog.Error("request failed", "method", request.Method, "path", request.URL.Path, "error", err)
	http.Error(response, "Временная ошибка сервера", http.StatusInternalServerError)
}

func (server *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		server.requests.Add(1)
		wrapped := &statusWriter{ResponseWriter: response, status: http.StatusOK}
		next.ServeHTTP(wrapped, request)
		slog.Info("http request", "method", request.Method, "path", request.URL.Path, "status", wrapped.status,
			"duration_ms", time.Since(started).Milliseconds(), "client_ip", clientIP(request))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(response, request)
	})
}

func ensureCSRF(response http.ResponseWriter, request *http.Request) string {
	if cookie, err := request.Cookie("wishlist_csrf"); err == nil && validRandomToken(cookie.Value) {
		return cookie.Value
	}
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	http.SetCookie(response, &http.Cookie{Name: "wishlist_csrf", Value: token, Path: "/", MaxAge: 86400,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	return token
}

func validCSRF(request *http.Request) bool {
	cookie, err := request.Cookie("wishlist_csrf")
	if err != nil || !validRandomToken(cookie.Value) {
		return false
	}
	form := request.Form.Get("csrf")
	return len(cookie.Value) == len(form) && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(form)) == 1
}

func validRandomToken(token string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) >= 24 && len(decoded) <= 64
}

func clientIP(request *http.Request) string {
	if ip := net.ParseIP(strings.TrimSpace(request.Header.Get("CF-Connecting-IP"))); ip != nil {
		return ip.String()
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}

func hostOnly(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return strings.TrimSuffix(host, ".")
}

func localHost(host string) bool {
	host = hostOnly(host)
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func hasGroup(groups, expected string) bool {
	for _, group := range strings.FieldsFunc(groups, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if group == expected {
			return true
		}
	}
	return false
}

func contextWithTimeout(request *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), timeout)
}

type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string][]time.Time{}}
}

func (limiter *rateLimiter) Allow(key string) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-limiter.window)
	times := limiter.entries[key][:0]
	for _, timestamp := range limiter.entries[key] {
		if timestamp.After(cutoff) {
			times = append(times, timestamp)
		}
	}
	if len(times) >= limiter.limit {
		limiter.entries[key] = times
		return false
	}
	limiter.entries[key] = append(times, now)
	if len(limiter.entries) > 4096 {
		for entry, timestamps := range limiter.entries {
			if len(timestamps) == 0 || timestamps[len(timestamps)-1].Before(cutoff) {
				delete(limiter.entries, entry)
			}
		}
	}
	return true
}
