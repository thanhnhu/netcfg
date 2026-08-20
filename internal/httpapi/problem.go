package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"netcfg/internal/domain"
	"netcfg/internal/platform/i18n"
)

// Problem is an RFC 9457 error document. MessageFormat and MessageArgs are a
// local extension: they let the browser re-translate the text on the fly.
type Problem struct {
	Type          string      `json:"type"`
	Title         string      `json:"title"`
	Status        int         `json:"status"`
	Detail        string      `json:"detail,omitempty"`
	Instance      string      `json:"instance,omitempty"`
	Code          domain.Code `json:"code,omitempty"`
	MessageFormat string      `json:"messageFormat,omitempty"`
	MessageArgs   []string    `json:"messageArgs,omitempty"`
}

var titles = map[domain.Code]string{
	domain.CodeInvalid:     "Invalid request",
	domain.CodeNotFound:    "Not found",
	domain.CodeConflict:    "State conflict",
	domain.CodeUnavailable: "System service unavailable",
	domain.CodeInternal:    "Internal error",
}

func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeInvalid:
		return http.StatusBadRequest
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict:
		return http.StatusConflict
	case domain.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// writeProblem renders a domain error as problem+json in the operator's
// language. Internal errors are logged in full but reported without detail.
func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	code := domain.CodeOf(err)
	status := statusFor(code)
	lang := langOf(r.Context())
	message := domain.MessageOf(err)

	if code == domain.CodeInternal {
		s.log.Error("request failed", "path", r.URL.Path, "err", err)
		message = domain.Msg("An internal error occurred. See the system logs for details.")
	} else {
		s.log.Warn("request rejected", "path", r.URL.Path, "code", code, "err", err)
	}

	problem := Problem{
		Type:          "https://netcfg.local/errors/" + string(code),
		Title:         i18n.T(lang, titles[code]),
		Status:        status,
		Detail:        i18n.M(lang, message),
		Instance:      r.URL.Path,
		Code:          code,
		MessageFormat: message.Format,
		MessageArgs:   message.Args,
	}

	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(problem); err != nil {
		slog.Default().Error("cannot write problem+json", "err", err)
	}
}

// writeStatusProblem reports a transport level failure that has no domain code.
func (s *Server) writeStatusProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	lang := langOf(r.Context())
	problem := Problem{
		Type:          "about:blank",
		Title:         http.StatusText(status),
		Status:        status,
		Detail:        i18n.T(lang, detail),
		Instance:      r.URL.Path,
		MessageFormat: detail,
	}

	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
