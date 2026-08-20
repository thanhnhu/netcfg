// Package httpapi serves the operator UI and the JSON API. It runs without any
// privilege and reaches the system only through the agent RPC client.
package httpapi

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"netcfg/internal/platform/auth"
	"netcfg/internal/rpc"
)

//go:embed assets/templates/*.html assets/static/*
var assets embed.FS

const (
	sessionCookie = "netcfg_session"
	// An insecure origin may neither overwrite nor delete a Secure cookie of the
	// same name, and a Secure cookie is never sent over plain HTTP. A host that
	// once served HTTPS would therefore trap the -no-tls build in a sign-in loop:
	insecureSessionCookie = "netcfg_session_http"
)

// Options configures the web tier.
type Options struct {
	Credentials    *auth.Manager
	Agent          *rpc.Client
	SessionTTL     time.Duration
	SessionMaxLife time.Duration
	SessionPath    string
	SecureCookie   bool
	TrustedProxy   []string
	PortalURL      string
	Log            *slog.Logger
}

// Server is the HTTP handler tree.
type Server struct {
	opts     Options
	tpl      *template.Template
	version  string
	sessions *auth.SessionStore
	throttle *auth.Throttle
	hub      *Hub
	mux      *http.ServeMux
	log      *slog.Logger
}

// New builds the server and registers routes.
func New(opts Options, hub *Hub) (*Server, error) {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 30 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	tpl, err := template.ParseFS(assets, "assets/templates/*.html")
	if err != nil {
		return nil, err
	}

	s := &Server{
		opts:     opts,
		tpl:      tpl,
		version:  assetVersion(),
		sessions: auth.NewSessionStore(opts.SessionTTL, opts.SessionMaxLife, opts.SessionPath, opts.Log),
		throttle: auth.NewThrottle(5, 5*time.Minute),
		hub:      hub,
		mux:      http.NewServeMux(),
		log:      opts.Log,
	}
	if err := s.sessions.Load(); err != nil {
		opts.Log.Warn("cannot restore saved sessions", "err", err)
	}
	s.routes()
	return s, nil
}

// Sessions exposes the store so the process can run its maintenance loop.
func (s *Server) Sessions() *auth.SessionStore { return s.sessions }

func (s *Server) cookieName() string {
	if s.opts.SecureCookie {
		return sessionCookie
	}
	return insecureSessionCookie
}

func (s *Server) routes() {
	static, err := fs.Sub(assets, "assets/static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServerFS(static))))

	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /i18n.json", s.handleCatalog)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.HandleFunc("GET /{$}", s.requireAuth(s.handleIndex))
	s.mux.HandleFunc("GET /account/password", s.requireAuth(s.handlePasswordPage))

	get := map[string]http.HandlerFunc{
		"GET /api/v1/state":    s.handleState,
		"GET /api/v1/pending":  s.handlePending,
		"GET /api/v1/hotspot":  s.handleHotspotStatus,
		"GET /api/v1/events":   s.handleEvents,
		"GET /api/v1/system":   s.handleSystemStats,
		"GET /api/v1/failover": s.handleFailoverStatus,
		"GET /api/v1/ssh":      s.handleSSHStatus,
	}
	post := map[string]http.HandlerFunc{
		"POST /api/v1/scan":                          s.handleScan,
		"POST /api/v1/plans":                         s.handlePlan,
		"POST /api/v1/ip":                            s.handleApplyIP,
		"POST /api/v1/wifi":                          s.handleApplyWiFi,
		"POST /api/v1/pending/{generation}/confirm":  s.handleConfirm,
		"POST /api/v1/pending/{generation}/rollback": s.handleRollback,
		"POST /api/v1/profiles/select":               s.handleSelectProfile,
		"POST /api/v1/profiles/secret":               s.handleProfileSecret,
		"POST /api/v1/profiles/remove":               s.handleRemoveProfile,
		"POST /api/v1/disconnect":                    s.handleDisconnect,
		"POST /api/v1/reconnect":                     s.handleReconnect,
		"POST /api/v1/hotspot/start":                 s.handleHotspotStart,
		"POST /api/v1/hotspot/stop":                  s.handleHotspotStop,
		"POST /api/v1/password":                      s.handleChangePassword,
		"POST /api/v1/ssh/enable":                    s.handleSSHEnable,
		"POST /api/v1/ssh/disable":                   s.handleSSHDisable,
	}
	for pattern, handler := range get {
		s.mux.HandleFunc(pattern, s.requireAuth(handler))
	}
	for pattern, handler := range post {
		s.mux.HandleFunc(pattern, s.requireAuth(handler))
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	// no-referrer would make Chrome send "Origin: null" on same-origin form posts.
	h.Set("Referrer-Policy", "same-origin")
	if s.opts.SecureCookie {
		h.Set("Strict-Transport-Security", "max-age=63072000")
	}
	if !strings.HasPrefix(r.URL.Path, "/static/") {
		h.Set("Cache-Control", "no-store")
	}
	h.Add("Vary", "Accept-Language")
	s.mux.ServeHTTP(w, r)
}

// requireAuth enforces authentication, and CSRF protection on every state
// changing verb.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withLang(r.Context(), s.resolveLanguage(w, r)))

		cookie, err := r.Cookie(s.cookieName())
		if err != nil {
			s.denyUnauthenticated(w, r)
			return
		}
		sess, ok := s.sessions.Get(cookie.Value)
		if !ok {
			s.clearSessionCookie(w)
			s.denyUnauthenticated(w, r)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) {
				s.writeStatusProblem(w, r, http.StatusForbidden, "Invalid Origin header")
				return
			}
			if !constantTimeEqual(r.Header.Get("X-CSRF-Token"), sess.CSRF) {
				s.writeStatusProblem(w, r, http.StatusForbidden, "Invalid CSRF token")
				return
			}
		}
		next(w, r.WithContext(withSession(r.Context(), sess)))
	}
}

func (s *Server) denyUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeStatusProblem(w, r, http.StatusUnauthorized, "Your session has expired")
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.opts.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.opts.SessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.opts.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// sameOrigin blocks cross-site posts that would otherwise ride on the cookie.
// Fetch Metadata decides first because a privacy-preserving Referrer-Policy can
// strip the Origin header down to "null" even on a same-origin form post.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// clientKey identifies a client for rate limiting. X-Forwarded-For is honoured
// only when the immediate peer is a configured trusted proxy.
func (s *Server) clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	for _, trusted := range s.opts.TrustedProxy {
		if trusted == host {
			if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
				return strings.TrimSpace(strings.Split(forwarded, ",")[0])
			}
		}
	}
	return host
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

// assetVersion fingerprints the embedded assets. Templates append it to every
// script and stylesheet URL, so a new binary can never serve fresh HTML against
// a browser still holding the previous script.
func assetVersion() string {
	sum := sha256.New()
	err := fs.WalkDir(assets, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		sum.Write([]byte(path))
		sum.Write(data)
		return nil
	})
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}
