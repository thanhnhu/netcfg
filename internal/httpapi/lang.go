package httpapi

import (
	"context"
	"net/http"
	"time"

	"netcfg/internal/platform/i18n"
)

type langKey struct{}

// withLang stores the resolved language for the rest of the request.
func withLang(ctx context.Context, lang i18n.Lang) context.Context {
	return context.WithValue(ctx, langKey{}, lang)
}

// langOf returns the language chosen for this request.
func langOf(ctx context.Context) i18n.Lang {
	if lang, ok := ctx.Value(langKey{}).(i18n.Lang); ok {
		return lang
	}
	return i18n.Default
}

// resolveLanguage applies ?lang= over the cookie over the browser preference,
// and remembers an explicit choice so it survives the next request.
func (s *Server) resolveLanguage(w http.ResponseWriter, r *http.Request) i18n.Lang {
	var cookieValue string
	if c, err := r.Cookie(i18n.Cookie); err == nil {
		cookieValue = c.Value
	}

	query := r.URL.Query().Get("lang")
	lang := i18n.Resolve(query, cookieValue, r.Header.Get("Accept-Language"))

	if chosen, ok := i18n.Parse(query); ok && string(chosen) != cookieValue {
		http.SetCookie(w, &http.Cookie{
			Name:     i18n.Cookie,
			Value:    string(chosen),
			Path:     "/",
			HttpOnly: false, // the browser bundle reads it to pick its catalog
			Secure:   s.opts.SecureCookie,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		})
	}
	return lang
}

// handleCatalog serves the translations the browser bundle needs. It is public
// because the sign-in page is rendered before a session exists.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	lang := s.resolveLanguage(w, r)

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"lang":      lang,
		"supported": i18n.Supported(),
		"messages":  i18n.Catalog(lang),
	})
}
