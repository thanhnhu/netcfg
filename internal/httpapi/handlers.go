package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"netcfg/internal/app"
	"netcfg/internal/domain"
	"netcfg/internal/platform/auth"
	"netcfg/internal/platform/i18n"
	"netcfg/internal/rpc"
)

const maxBodyBytes = 16 << 10

type ctxKey struct{}

func withSession(ctx context.Context, s auth.Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

func sessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(auth.Session)
	return s, ok
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type pageData struct {
	User      string
	CSRF      string
	Version   string
	Lang      i18n.Lang
	Supported []i18n.Lang
	T         func(string, ...any) string
}

type loginPageData struct {
	Error     string
	Version   string
	Lang      i18n.Lang
	Supported []i18n.Lang
	T         func(string, ...any) string
}

func (s *Server) page(lang i18n.Lang, user, csrf string) pageData {
	return pageData{
		User: user, CSRF: csrf, Version: s.version, Lang: lang, Supported: i18n.Supported(),
		T: func(source string, args ...any) string { return i18n.T(lang, source, args...) },
	}
}

// ---------- pages ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status := map[string]any{"web": "ok"}
	code := http.StatusOK
	if err := s.opts.Agent.Ping(ctx); err != nil {
		status["agent"] = err.Error()
		code = http.StatusServiceUnavailable
	} else {
		status["agent"] = "ok"
	}
	writeJSON(w, code, status)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName()); err == nil {
		if _, ok := s.sessions.Get(cookie.Value); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.log.Warn("rejected sign-in: origin mismatch",
			"origin", r.Header.Get("Origin"), "host", r.Host, "site", r.Header.Get("Sec-Fetch-Site"))
		s.renderLogin(w, r, http.StatusForbidden, "Invalid request.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, "Malformed form data.")
		return
	}

	key := s.clientKey(r)
	if blocked, remaining := s.throttle.Blocked(key); blocked {
		s.renderLogin(w, r, http.StatusTooManyRequests,
			i18n.T(s.resolveLanguage(w, r), "Too many failed sign-in attempts. Try again in %d seconds.", int(remaining.Seconds())))
		return
	}

	username := r.PostFormValue("username")
	if !s.opts.Credentials.Verify(username, r.PostFormValue("password")) {
		s.throttle.Fail(key)
		s.log.Warn("failed sign-in", "client", key)
		s.renderLogin(w, r, http.StatusUnauthorized, "Wrong username or password.")
		return
	}

	// A fresh token on every login defeats session fixation.
	sess, err := s.sessions.Create(username)
	if err != nil {
		s.renderLogin(w, r, http.StatusInternalServerError, "Could not create a session.")
		return
	}
	s.throttle.Reset(key)
	s.setSessionCookie(w, sess.Token)
	s.log.Info("successful sign-in", "user", username, "client", key)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName()); err == nil {
		s.sessions.Drop(cookie.Value)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type changePasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
	Confirm string `json:"confirm"`
}

// handleChangePassword updates the credential of the signed-in operator. It
// shares the login throttle, so guessing the current password here is rate
// limited exactly like guessing it at the sign-in form.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if !s.decode(w, r, &req) {
		return
	}

	key := s.clientKey(r)
	if blocked, _ := s.throttle.Blocked(key); blocked {
		s.writeStatusProblem(w, r, http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	if req.New != req.Confirm {
		s.writeProblem(w, r, domain.Invalid("the two new passwords do not match"))
		return
	}

	sess, _ := sessionFrom(r.Context())
	if err := s.opts.Credentials.ChangePassword(sess.User, req.Current, req.New); err != nil {
		if domain.CodeOf(err) == domain.CodeInvalid {
			s.throttle.Fail(key)
		}
		s.writeProblem(w, r, err)
		return
	}
	s.throttle.Reset(key)

	// The current session survives so the operator is not thrown out mid-change.
	revoked := s.sessions.DropOthers(sess.Token)
	s.audit(r, "password.change", "revokedSessions", revoked)
	writeJSON(w, http.StatusOK, map[string]int{"revokedSessions": revoked})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "index.html", s.page(langOf(r.Context()), sess.User, sess.CSRF)); err != nil {
		s.log.Error("render index", "err", err)
	}
}

func (s *Server) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "password.html", s.page(langOf(r.Context()), sess.User, sess.CSRF)); err != nil {
		s.log.Error("render password page", "err", err)
	}
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	lang := s.resolveLanguage(w, r)
	data := loginPageData{
		Version: s.version, Lang: lang, Supported: i18n.Supported(),
		T: func(source string, args ...any) string { return i18n.T(lang, source, args...) },
	}
	if message != "" {
		data.Error = i18n.T(lang, message)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tpl.ExecuteTemplate(w, "login.html", data); err != nil {
		s.log.Error("render login", "err", err)
	}
}

// ---------- api ----------

type stateResponse struct {
	Links      []domain.Link `json:"links"`
	View       app.LinkView  `json:"view"`
	ServerTime time.Time     `json:"serverTime"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	links, err := s.opts.Agent.Links(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	view, err := s.opts.Agent.Snapshot(ctx, r.URL.Query().Get("link"))
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{Links: links, View: view, ServerTime: time.Now()})
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pending, err := s.opts.Agent.Pending(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.PendingResult{Pending: pending})
}

func (s *Server) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stats, err := s.opts.Agent.SystemStats(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.SystemStatsResult{Stats: stats})
}

func (s *Server) handleFailoverStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	status, err := s.opts.Agent.FailoverStatus(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.FailoverResult{Status: status})
}

func (s *Server) handleSSHStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	status, err := s.opts.Agent.SSHStatus(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.SSHResult{Status: status})
}

type sshRequest struct {
	WindowMinutes int `json:"windowMinutes"`
}

func (s *Server) handleSSHEnable(w http.ResponseWriter, r *http.Request) {
	var req sshRequest
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	status, err := s.opts.Agent.SSHEnable(ctx, time.Duration(req.WindowMinutes)*time.Minute)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "ssh.enable", "windowMinutes", req.WindowMinutes)
	writeJSON(w, http.StatusOK, rpc.SSHResult{Status: status})
}

func (s *Server) handleSSHDisable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	status, err := s.opts.Agent.SSHDisable(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "ssh.disable")
	writeJSON(w, http.StatusOK, rpc.SSHResult{Status: status})
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req rpc.LinkParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	networks, err := s.opts.Agent.Scan(ctx, req.Link)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.ScanResult{Networks: networks})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req ipRequest
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	diff, err := s.opts.Agent.PlanIP(ctx, req.plan())
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

type ipRequest struct {
	Link           string   `json:"link"`
	Mode           string   `json:"mode"`
	Address        string   `json:"address"`
	Gateway        string   `json:"gateway"`
	Mode6          string   `json:"mode6"`
	Address6       string   `json:"address6"`
	Gateway6       string   `json:"gateway6"`
	Metric         uint32   `json:"metric"`
	NoDefaultRoute bool     `json:"noDefaultRoute"`
	DNS            []string `json:"dns"`
	ConfirmWindow  int      `json:"confirmWindowSeconds"`
	NoRollback     bool     `json:"noRollback"`
}

func (r ipRequest) plan() domain.IPPlan {
	return domain.IPPlan{
		Link:           r.Link,
		Mode:           domain.Mode(r.Mode),
		Address:        r.Address,
		Gateway:        r.Gateway,
		Mode6:          domain.Mode(r.Mode6),
		Address6:       r.Address6,
		Gateway6:       r.Gateway6,
		Metric:         r.Metric,
		NoDefaultRoute: r.NoDefaultRoute,
		DNS:            r.DNS,
	}
}

func (s *Server) handleApplyIP(w http.ResponseWriter, r *http.Request) {
	var req ipRequest
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	result, err := s.opts.Agent.ApplyIP(ctx, rpc.ApplyIPParams{
		Plan:          req.plan(),
		ConfirmWindow: time.Duration(req.ConfirmWindow) * time.Second,
		NoRollback:    req.NoRollback,
	})
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "apply_ip", "link", req.Link, "mode", req.Mode, "noRollback", req.NoRollback)
	writeJSON(w, http.StatusOK, result)
}

type wifiRequest struct {
	Link          string `json:"link"`
	SSID          string `json:"ssid"`
	Security      string `json:"security"`
	Hidden        bool   `json:"hidden"`
	Passphrase    string `json:"passphrase"`
	ConfirmWindow int    `json:"confirmWindowSeconds"`
	NoRollback    bool   `json:"noRollback"`
}

func (s *Server) handleApplyWiFi(w http.ResponseWriter, r *http.Request) {
	var req wifiRequest
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	result, err := s.opts.Agent.ApplyWiFi(ctx, rpc.ApplyWiFiParams{
		Link:          req.Link,
		SSID:          req.SSID,
		Security:      domain.Security(req.Security),
		Hidden:        req.Hidden,
		Passphrase:    req.Passphrase,
		ConfirmWindow: time.Duration(req.ConfirmWindow) * time.Second,
		NoRollback:    req.NoRollback,
	})
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "apply_wifi", "link", req.Link, "ssid", req.SSID, "noRollback", req.NoRollback)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	generation, ok := s.generation(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if err := s.opts.Agent.Confirm(ctx, generation); err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "confirm", "generation", generation)
	writeJSON(w, http.StatusOK, rpc.OKResult{OK: true})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	generation, ok := s.generation(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := s.opts.Agent.Rollback(ctx, generation); err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "rollback", "generation", generation)
	writeJSON(w, http.StatusOK, rpc.OKResult{OK: true})
}

func (s *Server) handleSelectProfile(w http.ResponseWriter, r *http.Request) {
	var req rpc.ProfileParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.opts.Agent.SelectProfile(ctx, req.Link, req.ID); err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "select_profile", "link", req.Link, "id", req.ID)
	writeJSON(w, http.StatusOK, rpc.OKResult{OK: true})
}

func (s *Server) handleProfileSecret(w http.ResponseWriter, r *http.Request) {
	var req rpc.ProfileParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	secret, err := s.opts.Agent.ProfileSecret(ctx, req.Link, req.ID)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "reveal_profile_secret", "link", req.Link, "id", req.ID)
	// Caching a credential in a proxy or on disk would outlive the session.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, secret)
}

func (s *Server) handleRemoveProfile(w http.ResponseWriter, r *http.Request) {
	var req rpc.ProfileParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.opts.Agent.RemoveProfile(ctx, req.Link, req.ID); err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "remove_profile", "link", req.Link, "id", req.ID)
	writeJSON(w, http.StatusOK, rpc.OKResult{OK: true})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	s.linkAction(w, r, "disconnect", s.opts.Agent.Disconnect)
}
func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	s.linkAction(w, r, "reconnect", s.opts.Agent.Reconnect)
}

func (s *Server) linkAction(w http.ResponseWriter, r *http.Request, name string, action func(context.Context, string) error) {
	var req rpc.LinkParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := action(ctx, req.Link); err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, name, "link", req.Link)
	writeJSON(w, http.StatusOK, rpc.OKResult{OK: true})
}

// ---------- helpers ----------

func (s *Server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		s.writeStatusProblem(w, r, http.StatusBadRequest, "Malformed request body")
		return false
	}
	return true
}

func (s *Server) generation(w http.ResponseWriter, r *http.Request) (domain.Generation, bool) {
	raw := r.PathValue("generation")
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		s.writeStatusProblem(w, r, http.StatusBadRequest, "invalid generation")
		return 0, false
	}
	return domain.Generation(value), true
}

// audit records who changed what; it deliberately never receives a passphrase.
func (s *Server) audit(r *http.Request, action string, kv ...any) {
	sess, _ := sessionFrom(r.Context())
	args := append([]any{"action", action, "user", sess.User, "client", s.clientKey(r)}, kv...)
	s.log.Info("audit", args...)
}
