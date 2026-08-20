package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"netcfg/internal/app"
	"netcfg/internal/domain"
)

// Subscriber hands out event streams; the in-process event bus implements it.
type Subscriber interface {
	Subscribe() (<-chan domain.Event, func())
}

// ServerConfig configures the privileged listener.
type ServerConfig struct {
	SocketPath string
	// AllowedUIDs is the allowlist of client user ids. Empty means root only.
	AllowedUIDs []uint32
	// GID, when >= 0, is applied to the socket together with mode 0660 so that a
	// dedicated group can reach it without world access.
	GID int
}

// Server exposes the agent over a Unix domain socket.
type Server struct {
	cfg   ServerConfig
	agent *app.Agent
	bus   Subscriber
	log   *slog.Logger
}

// NewServer wires the listener.
func NewServer(cfg ServerConfig, agent *app.Agent, bus Subscriber, log *slog.Logger) *Server {
	if len(cfg.AllowedUIDs) == 0 {
		cfg.AllowedUIDs = []uint32{0}
	}
	return &Server{cfg: cfg, agent: agent, bus: bus, log: log}
}

// prepareDir makes the directory holding the socket reachable by the socket
// group. The socket's own 0660 is worthless if the group cannot traverse the
// directory it sits in, and systemd's RuntimeDirectory arrives owned by the
// unit's group, which need not be ours.
func (s *Server) prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if s.cfg.GID < 0 {
		return nil
	}

	if err := os.Chown(dir, 0, s.cfg.GID); err != nil {
		s.log.Warn("cannot set the socket directory group", "dir", dir, "err", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	// Group search only; nothing here is listed for anyone else.
	if want := info.Mode().Perm() | 0o050; want != info.Mode().Perm() {
		if err := os.Chmod(dir, want); err != nil {
			s.log.Warn("cannot make the socket directory searchable", "dir", dir, "err", err)
		}
	}
	return nil
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.prepareDir(filepath.Dir(s.cfg.SocketPath)); err != nil {
		return err
	}
	// A stale socket from an unclean shutdown would block Listen.
	if err := os.Remove(s.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	listener, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(s.cfg.SocketPath)

	if err := os.Chmod(s.cfg.SocketPath, 0o660); err != nil {
		return err
	}
	if s.cfg.GID >= 0 {
		if err := os.Chown(s.cfg.SocketPath, 0, s.cfg.GID); err != nil {
			s.log.Warn("cannot set the socket group", "err", err)
		}
	}
	if !peerCredSupported() {
		s.log.Warn("this platform has no SO_PEERCRED: skipping the client UID check (development builds only)")
	}
	s.log.Info("agent listening", "socket", s.cfg.SocketPath, "allowedUIDs", s.cfg.AllowedUIDs)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn.(*net.UnixConn))
	}
}

func (s *Server) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()

	if uid, err := peerUID(conn); err == nil {
		if !slices.Contains(s.cfg.AllowedUIDs, uid) {
			s.log.Warn("rejected an unauthorised client", "uid", uid)
			return
		}
	} else if peerCredSupported() {
		s.log.Warn("cannot read client credentials", "err", err)
		return
	}

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req Request
		if err := decoder.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.log.Debug("agent connection ended", "err", err)
			}
			return
		}

		if req.Method == MethodSubscribe {
			_ = encoder.Encode(Response{ID: req.ID, Result: json.RawMessage(`{"ok":true}`)})
			s.stream(ctx, conn, encoder)
			return
		}

		result, err := s.dispatch(ctx, req)
		resp := Response{ID: req.ID}
		if err != nil {
			resp.Error = &Error{Code: domain.CodeOf(err), Msg: domain.MessageOf(err)}
			s.log.Warn("agent command failed", "req", req, "code", resp.Error.Code, "err", err)
		} else if result != nil {
			payload, mErr := json.Marshal(result)
			if mErr != nil {
				resp.Error = &Error{Code: domain.CodeInternal, Msg: domain.Msg(mErr.Error())}
			} else {
				resp.Result = payload
			}
		}
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// stream pushes agent events until the peer disconnects.
func (s *Server) stream(ctx context.Context, conn *net.UnixConn, encoder *json.Encoder) {
	events, cancel := s.bus.Subscribe()
	defer cancel()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _ = io.Copy(io.Discard, conn) // unblocks when the peer goes away
	}()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-closed:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			if err := encoder.Encode(evt); err != nil {
				return
			}
		case <-ticker.C:
			if err := encoder.Encode(domain.NewEvent(domain.EventLog, "", domain.Message{}, nil)); err != nil {
				return
			}
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) (any, error) {
	decode := func(dst any) error {
		if len(req.Params) == 0 {
			return nil
		}
		if err := json.Unmarshal(req.Params, dst); err != nil {
			return domain.Invalid("invalid parameters for %s", req.Method)
		}
		return nil
	}

	switch req.Method {
	case MethodLinks:
		links, err := s.agent.Links(ctx)
		return LinksResult{Links: links}, err

	case MethodSystemStats:
		stats, err := s.agent.SystemStats(ctx)
		return SystemStatsResult{Stats: stats}, err

	case MethodFailover:
		return FailoverResult{Status: s.agent.FailoverStatus(ctx)}, nil

	case MethodSSHStatus:
		status, err := s.agent.SSHStatus(ctx)
		return SSHResult{Status: status}, err

	case MethodSSHEnable:
		var p SSHParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		status, err := s.agent.SSHEnable(ctx, p.Window)
		return SSHResult{Status: status}, err

	case MethodSSHDisable:
		status, err := s.agent.SSHDisable(ctx)
		return SSHResult{Status: status}, err

	case MethodSnapshot:
		var p LinkParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.agent.Snapshot(ctx, p.Link)

	case MethodScan:
		var p LinkParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		networks, err := s.agent.Scan(ctx, p.Link)
		return ScanResult{Networks: networks}, err

	case MethodPlanIP:
		var p ApplyIPParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return s.agent.PlanIP(ctx, p.Plan)

	case MethodApplyIP:
		var p ApplyIPParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		pending, err := s.agent.ApplyIP(ctx, p.Plan, app.ApplyOptions{
			ConfirmWindow: p.ConfirmWindow,
			NoRollback:    p.NoRollback,
		})
		return ApplyResult{Pending: pending}, err

	case MethodApplyWiFi:
		var p ApplyWiFiParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		pending, warning, err := s.agent.ApplyWiFi(ctx, domain.WiFiRequest{
			Link:       p.Link,
			SSID:       p.SSID,
			Security:   p.Security,
			Hidden:     p.Hidden,
			Passphrase: domain.NewSecret(p.Passphrase),
		}, app.ApplyOptions{ConfirmWindow: p.ConfirmWindow, NoRollback: p.NoRollback})
		return ApplyResult{Pending: pending, Warning: warning}, err

	case MethodConfirm:
		var p GenerationParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.Confirm(ctx, p.Generation)

	case MethodRollback:
		var p GenerationParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.Rollback(ctx, p.Generation)

	case MethodPending:
		return PendingResult{Pending: s.agent.Pending()}, nil

	case MethodSelectProfile:
		var p ProfileParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.SelectProfile(ctx, p.Link, p.ID)

	case MethodRemoveProfile:
		var p ProfileParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.RemoveProfile(ctx, p.Link, p.ID)

	case MethodProfileSecret:
		var p ProfileParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		secret, err := s.agent.ProfileSecret(ctx, p.Link, p.ID)
		if err != nil {
			return nil, err
		}
		// The only place a stored credential leaves the agent.
		return SecretResult{SSID: secret.SSID, Value: secret.Value.Reveal(), Hashed: secret.Hashed}, nil
	case MethodDisconnect:
		var p LinkParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.Disconnect(ctx, p.Link)

	case MethodReconnect:
		var p LinkParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, s.agent.Reconnect(ctx, p.Link)

	case MethodHotspotStatus:
		return HotspotResult{Status: s.agent.HotspotStatus(ctx)}, nil

	case MethodHotspotStart:
		var p LinkParams
		if err := decode(&p); err != nil {
			return nil, err
		}
		if err := s.agent.StartHotspot(ctx, p.Link); err != nil {
			return nil, err
		}
		return HotspotResult{Status: s.agent.HotspotStatus(ctx)}, nil

	case MethodHotspotStop:
		if err := s.agent.StopHotspot(ctx); err != nil {
			return nil, err
		}
		return HotspotResult{Status: s.agent.HotspotStatus(ctx)}, nil
	}
	return nil, domain.NotFound("unknown method: %q", req.Method)
}
