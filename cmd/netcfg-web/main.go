// Command netcfg-web serves the operator UI. It runs without privileges and can
// only reach the system through the narrow RPC contract exposed by netcfgd.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"netcfg/internal/httpapi"
	"netcfg/internal/platform/auth"
	"netcfg/internal/platform/certs"
	"netcfg/internal/platform/logging"
	"netcfg/internal/rpc"
)

func main() {
	var (
		listen       = flag.String("listen", ":8090", "listen address; open to the whole LAN by default")
		agentSocket  = flag.String("agent-socket", "/run/netcfgd/netcfgd.sock", "netcfgd socket")
		configPath   = flag.String("config", "/etc/netcfg-web/config.json", "administrator credential file")
		tlsDir       = flag.String("tls-dir", "", "where to keep the self-signed certificate (defaults to <state-dir>/tls)")
		tlsCert      = flag.String("tls-cert", "", "existing TLS certificate (overrides -tls-dir)")
		tlsKey       = flag.String("tls-key", "", "existing TLS private key")
		tlsHosts     = flag.String("tls-hosts", "", "extra host names or IPs for the self-signed certificate")
		noTLS        = flag.Bool("no-tls", false, "serve plain HTTP; only behind a TLS terminating proxy")
		sessionTTL   = flag.Duration("session-ttl", 30*time.Minute, "idle time before a session expires")
		sessionMax   = flag.Duration("session-max", 12*time.Hour, "absolute lifetime of a session")
		stateDir     = flag.String("state-dir", "/var/lib/netcfg-web", "where sessions and certificates are stored")
		portalListen = flag.String("portal-listen", "", "HTTP address for the captive portal while the fallback AP runs, e.g. :80")
		portalURL    = flag.String("portal-url", "http://192.168.4.1/", "URL the captive portal redirects to")
		trustedProxy = flag.String("trusted-proxy", "", "reverse proxy IPs trusted for X-Forwarded-For")
		username     = flag.String("username", "admin", "user name used with -set-password")
		setPassword  = flag.Bool("set-password", false, "set the administrator password and exit")
		logFormat    = flag.String("log-format", "text", "log format: text or json")
		logLevel     = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

	log := logging.New(*logFormat, *logLevel, "netcfg-web")

	if *setPassword {
		if err := runSetPassword(*configPath, *username); err != nil {
			log.Error("cannot set the password", "err", err)
			os.Exit(1)
		}
		fmt.Printf("Saved account %q to %s\n", *username, *configPath)
		return
	}

	credentials, err := auth.Load(*configPath)
	if err != nil {
		log.Error("cannot read the administrator credentials", "err", err)
		os.Exit(1)
	}

	certFile, keyFile := *tlsCert, *tlsKey
	useTLS := !*noTLS
	if useTLS && certFile == "" {
		certFile, keyFile, err = ensureCert(orDefault(*tlsDir, filepath.Join(*stateDir, "tls")), *tlsHosts, log)
		if err != nil {
			log.Error("cannot create a self-signed certificate", "err", err)
			os.Exit(1)
		}
	}
	if *noTLS {
		log.Warn("TLS is disabled: only do this behind a TLS terminating reverse proxy")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := rpc.NewClient(*agentSocket)
	if err := client.Ping(ctx); err != nil {
		log.Warn("netcfgd is not reachable yet, retrying on demand", "socket", *agentSocket, "err", err)
	}

	hub := httpapi.NewHub(client, log)
	go hub.Run(ctx)

	handler, err := httpapi.New(httpapi.Options{
		Credentials:    auth.NewManager(*configPath, credentials),
		Agent:          client,
		SessionTTL:     *sessionTTL,
		SessionMaxLife: *sessionMax,
		SessionPath:    filepath.Join(*stateDir, "sessions.json"),
		SecureCookie:   useTLS,
		TrustedProxy:   splitList(*trustedProxy),
		PortalURL:      *portalURL,
		Log:            log,
	}, hub)
	if err != nil {
		log.Error("cannot start the web server", "err", err)
		os.Exit(1)
	}
	go handler.Sessions().Run(ctx)

	if *portalListen != "" {
		go servePortal(ctx, *portalListen, *portalURL, handler, log)
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Long enough for a Wi-Fi scan, but the SSE stream needs no write limit.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		TLSConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	announce(*listen, useTLS, log)

	if useTLS {
		err = server.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("web server stopped", "err", err)
		os.Exit(1)
	}
}

func ensureCert(dir, extraHosts string, log *slog.Logger) (string, string, error) {
	certFile, keyFile, fingerprint, err := certs.EnsureSelfSigned(dir, splitList(extraHosts))
	if err != nil {
		return "", "", err
	}
	log.Info("using a self-signed certificate", "cert", certFile, "fingerprint", fingerprint)
	fmt.Fprintf(os.Stderr, "\nCertificate fingerprint (compare it when the browser warns you):\n  %s\n\n", fingerprint)
	return certFile, keyFile, nil
}

// servePortal answers the fallback AP over plain HTTP. Captive portal detection
// refuses to follow a redirect into HTTPS, so this listener must not use TLS.
func servePortal(ctx context.Context, addr, portalURL string, inner http.Handler, log *slog.Logger) {
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.NewCaptivePortal(inner, portalURL),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info("captive portal listening", "addr", addr, "redirect", portalURL)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("captive portal stopped", "addr", addr, "err", err)
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// announce prints every URL the device can be reached at, which is what an
// operator on the LAN actually needs.
func announce(listen string, useTLS bool, log *slog.Logger) {
	scheme := "https"
	if !useTLS {
		scheme = "http"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		log.Info("listening", "addr", listen)
		return
	}

	if host != "" && host != "0.0.0.0" && host != "::" {
		log.Info("web interface ready", "url", fmt.Sprintf("%s://%s:%s/", scheme, host, port))
		return
	}

	urls := []string{fmt.Sprintf("%s://127.0.0.1:%s/", scheme, port)}
	if name, err := os.Hostname(); err == nil && name != "" {
		urls = append(urls, fmt.Sprintf("%s://%s.local:%s/", scheme, name, port))
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, ifi := range ifaces {
			if ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := ifi.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ipNet, ok := addr.(*net.IPNet)
				if !ok || ipNet.IP.IsLinkLocalUnicast() {
					continue
				}
				host := ipNet.IP.String()
				if ipNet.IP.To4() == nil {
					host = "[" + host + "]"
				}
				urls = append(urls, fmt.Sprintf("%s://%s:%s/", scheme, host, port))
			}
		}
	}

	log.Info("web interface ready", "addr", listen)
	fmt.Fprintln(os.Stderr, "Reach the interface at:")
	for _, url := range urls {
		fmt.Fprintln(os.Stderr, "  "+url)
	}
	fmt.Fprintln(os.Stderr)
}

func runSetPassword(configPath, username string) error {
	password, err := readSecret("New administrator password: ")
	if err != nil {
		return err
	}
	confirm, err := readSecret("Repeat password: ")
	if err != nil {
		return err
	}
	if password != confirm {
		return errors.New("the two entries do not match")
	}
	return auth.Save(configPath, username, password)
}

// readSecret prefers NETCFG_PASSWORD, then a terminal prompt with echo
// disabled, then a piped stdin line.
func readSecret(prompt string) (string, error) {
	if v, ok := os.LookupEnv("NETCFG_PASSWORD"); ok {
		return v, nil
	}
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, prompt)
		restore := disableEcho()
		defer func() {
			restore()
			fmt.Fprintln(os.Stderr)
		}()
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func disableEcho() func() {
	if runtime.GOOS != "linux" {
		return func() {}
	}
	if err := stty("-echo"); err != nil {
		return func() {}
	}
	return func() { _ = stty("echo") }
}

func stty(mode string) error {
	cmd := exec.Command("stty", mode)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
