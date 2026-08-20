package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"netcfg/internal/rpc"
)

func (s *Server) handleHotspotStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	status, err := s.opts.Agent.HotspotStatus(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rpc.HotspotResult{Status: status})
}

func (s *Server) handleHotspotStart(w http.ResponseWriter, r *http.Request) {
	var req rpc.LinkParams
	if !s.decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	status, err := s.opts.Agent.StartHotspot(ctx, req.Link)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "hotspot_start", "link", req.Link)
	writeJSON(w, http.StatusOK, rpc.HotspotResult{Status: status})
}

func (s *Server) handleHotspotStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	status, err := s.opts.Agent.StopHotspot(ctx)
	if err != nil {
		s.writeProblem(w, r, err)
		return
	}
	s.audit(r, "hotspot_stop")
	writeJSON(w, http.StatusOK, rpc.HotspotResult{Status: status})
}

// captivePortal answers the connectivity checks each operating system performs
// after joining a Wi-Fi network, which is what makes the sign-in page pop up.
type captivePortal struct {
	inner     http.Handler
	portalURL string
}

// probePaths are the well-known URLs used by Android, Apple, Windows and
// GNOME/Ubuntu to decide whether a network is "captive".
var probePaths = map[string]bool{
	"/generate_204":                  true,
	"/gen_204":                       true,
	"/mobile/status.php":             true,
	"/hotspot-detect.html":           true,
	"/library/test/success.html":     true,
	"/success.txt":                   true,
	"/ncsi.txt":                      true,
	"/connecttest.txt":               true,
	"/redirect":                      true,
	"/canonical.html":                true,
	"/check_network_status.txt":      true,
	"/kindle-wifi/wifiredirect.html": true,
}

// NewCaptivePortal wraps the UI so that unknown probe URLs redirect into it.
func NewCaptivePortal(inner http.Handler, portalURL string) http.Handler {
	return &captivePortal{inner: inner, portalURL: portalURL}
}

func (c *captivePortal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if probePaths[strings.ToLower(r.URL.Path)] {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, c.portalURL, http.StatusFound)
		return
	}
	c.inner.ServeHTTP(w, r)
}

// PortalURL builds the address advertised to clients of the fallback AP.
func PortalURL(address string) string {
	host, _, found := strings.Cut(address, "/")
	if !found {
		host = address
	}
	return fmt.Sprintf("http://%s/", host)
}
