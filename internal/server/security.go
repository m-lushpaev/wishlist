package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (server *Server) requireHost(response http.ResponseWriter, request *http.Request, expected string) bool {
	if hostOnly(request.Host) != expected {
		http.NotFound(response, request)
		return false
	}
	return true
}

func (server *Server) internalError(response http.ResponseWriter, request *http.Request, err error) {
	server.errors.Add(1)
	slog.Error("request failed", "method", request.Method, "path", safeLogPath(request), "error", err)
	http.Error(response, "Временная ошибка сервера", http.StatusInternalServerError)
}

func (server *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		server.requests.Add(1)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		slog.Info("http request", "method", r.Method, "path", safeLogPath(r), "status", wrapped.status, "duration_ms", time.Since(started).Milliseconds(), "client_ip", clientIP(r))
	})
}

func safeLogPath(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/reservation/") {
		return "/reservation/[redacted]"
	}
	return r.URL.Path
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
	form := request.PostForm.Get("csrf")
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
