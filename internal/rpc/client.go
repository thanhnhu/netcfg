package rpc

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"time"

	"netcfg/internal/app"
	"netcfg/internal/domain"
)

const dialTimeout = 5 * time.Second

// Client talks to the privileged agent. Each call uses its own short lived
// connection, so a slow scan never blocks an unrelated request.
type Client struct {
	socketPath string
	nextID     atomic.Uint64
}

// NewClient returns a client for the agent socket.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

func (c *Client) dial(ctx context.Context) (*net.UnixConn, error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, domain.Unavailable("cannot reach netcfgd at %s: %v", c.socketPath, err)
	}
	return conn.(*net.UnixConn), nil
}

// call performs one request/response round trip.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}

	req := Request{ID: c.nextID.Add(1), Method: method}
	if params != nil {
		payload, err := json.Marshal(params)
		if err != nil {
			return domain.Internal("encode parameters: %v", err)
		}
		req.Params = payload
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return domain.Unavailable("send command to netcfgd: %v", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return domain.Unavailable("read reply from netcfgd: %v", err)
	}
	if resp.Error != nil {
		return resp.Error.ToDomain()
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return domain.Internal("decode reply: %v", err)
		}
	}
	return nil
}

// Ping verifies the agent is reachable.
func (c *Client) Ping(ctx context.Context) error {
	var out LinksResult
	return c.call(ctx, MethodLinks, nil, &out)
}

func (c *Client) Links(ctx context.Context) ([]domain.Link, error) {
	var out LinksResult
	err := c.call(ctx, MethodLinks, nil, &out)
	return out.Links, err
}

func (c *Client) SystemStats(ctx context.Context) (domain.SystemStats, error) {
	var out SystemStatsResult
	err := c.call(ctx, MethodSystemStats, nil, &out)
	return out.Stats, err
}

func (c *Client) FailoverStatus(ctx context.Context) (domain.FailoverStatus, error) {
	var out FailoverResult
	err := c.call(ctx, MethodFailover, nil, &out)
	return out.Status, err
}

func (c *Client) SSHStatus(ctx context.Context) (domain.SSHStatus, error) {
	var out SSHResult
	err := c.call(ctx, MethodSSHStatus, nil, &out)
	return out.Status, err
}

func (c *Client) SSHEnable(ctx context.Context, window time.Duration) (domain.SSHStatus, error) {
	var out SSHResult
	err := c.call(ctx, MethodSSHEnable, SSHParams{Window: window}, &out)
	return out.Status, err
}

func (c *Client) SSHDisable(ctx context.Context) (domain.SSHStatus, error) {
	var out SSHResult
	err := c.call(ctx, MethodSSHDisable, nil, &out)
	return out.Status, err
}

func (c *Client) Snapshot(ctx context.Context, link string) (app.LinkView, error) {
	var out app.LinkView
	err := c.call(ctx, MethodSnapshot, LinkParams{Link: link}, &out)
	return out, err
}

func (c *Client) Scan(ctx context.Context, link string) ([]domain.AccessPoint, error) {
	var out ScanResult
	err := c.call(ctx, MethodScan, LinkParams{Link: link}, &out)
	return out.Networks, err
}

func (c *Client) PlanIP(ctx context.Context, plan domain.IPPlan) (domain.Diff, error) {
	var out domain.Diff
	err := c.call(ctx, MethodPlanIP, ApplyIPParams{Plan: plan}, &out)
	return out, err
}

func (c *Client) ApplyIP(ctx context.Context, params ApplyIPParams) (ApplyResult, error) {
	var out ApplyResult
	err := c.call(ctx, MethodApplyIP, params, &out)
	return out, err
}

func (c *Client) ApplyWiFi(ctx context.Context, params ApplyWiFiParams) (ApplyResult, error) {
	var out ApplyResult
	err := c.call(ctx, MethodApplyWiFi, params, &out)
	return out, err
}

func (c *Client) Confirm(ctx context.Context, generation domain.Generation) error {
	return c.call(ctx, MethodConfirm, GenerationParams{Generation: generation}, nil)
}

func (c *Client) Rollback(ctx context.Context, generation domain.Generation) error {
	return c.call(ctx, MethodRollback, GenerationParams{Generation: generation}, nil)
}

func (c *Client) Pending(ctx context.Context) (*domain.PendingApply, error) {
	var out PendingResult
	err := c.call(ctx, MethodPending, nil, &out)
	return out.Pending, err
}

func (c *Client) SelectProfile(ctx context.Context, link string, id int) error {
	return c.call(ctx, MethodSelectProfile, ProfileParams{Link: link, ID: id}, nil)
}

func (c *Client) RemoveProfile(ctx context.Context, link string, id int) error {
	return c.call(ctx, MethodRemoveProfile, ProfileParams{Link: link, ID: id}, nil)
}

func (c *Client) ProfileSecret(ctx context.Context, link string, id int) (SecretResult, error) {
	var out SecretResult
	err := c.call(ctx, MethodProfileSecret, ProfileParams{Link: link, ID: id}, &out)
	return out, err
}

func (c *Client) Disconnect(ctx context.Context, link string) error {
	return c.call(ctx, MethodDisconnect, LinkParams{Link: link}, nil)
}

func (c *Client) Reconnect(ctx context.Context, link string) error {
	return c.call(ctx, MethodReconnect, LinkParams{Link: link}, nil)
}

func (c *Client) HotspotStatus(ctx context.Context) (domain.HotspotStatus, error) {
	var out HotspotResult
	err := c.call(ctx, MethodHotspotStatus, nil, &out)
	return out.Status, err
}

func (c *Client) StartHotspot(ctx context.Context, link string) (domain.HotspotStatus, error) {
	var out HotspotResult
	err := c.call(ctx, MethodHotspotStart, LinkParams{Link: link}, &out)
	return out.Status, err
}

func (c *Client) StopHotspot(ctx context.Context) (domain.HotspotStatus, error) {
	var out HotspotResult
	err := c.call(ctx, MethodHotspotStop, nil, &out)
	return out.Status, err
}

// Subscribe streams agent events until ctx is cancelled or the agent goes away.
// The caller should reconnect on channel close.
func (c *Client) Subscribe(ctx context.Context) (<-chan domain.Event, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	if err := json.NewEncoder(conn).Encode(Request{ID: c.nextID.Add(1), Method: MethodSubscribe}); err != nil {
		conn.Close()
		return nil, domain.Unavailable("event subscription failed: %v", err)
	}

	decoder := json.NewDecoder(conn)
	var ack Response
	if err := decoder.Decode(&ack); err != nil {
		conn.Close()
		return nil, domain.Unavailable("event subscription failed: %v", err)
	}

	out := make(chan domain.Event, 32)
	go func() {
		defer close(out)
		defer conn.Close()

		go func() {
			<-ctx.Done()
			_ = conn.Close()
		}()

		for {
			var evt domain.Event
			if err := decoder.Decode(&evt); err != nil {
				return
			}
			if evt.Type == domain.EventLog && evt.Text.Empty() {
				continue // keepalive
			}
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
