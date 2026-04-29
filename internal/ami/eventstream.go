package ami

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// EventHandler receives every async AMI event off a long-lived connection.
type EventHandler interface {
	Handle(ctx context.Context, ev Message)
}

// EventHandlerFunc is a function adapter for EventHandler.
type EventHandlerFunc func(ctx context.Context, ev Message)

// Handle implements EventHandler.
func (f EventHandlerFunc) Handle(ctx context.Context, ev Message) { f(ctx, ev) }

// EventStreamHooks lets the caller observe lifecycle transitions of the
// stream (e.g. drive Prometheus gauges/counters).
type EventStreamHooks struct {
	OnUp        func(up bool)
	OnReconnect func()
}

// EventStream maintains a persistent AMI connection that subscribes to async
// events and dispatches each event to the handler. On disconnect it
// reconnects with bounded exponential backoff.
type EventStream struct {
	cfg     Config
	dialer  Dialer
	handler EventHandler
	hooks   EventStreamHooks
	logger  *slog.Logger

	// Backoff bounds; overridable in tests.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// NewEventStream constructs an EventStream. dialer may be nil to use the
// default TCP dialer. cfg.EventClasses must be non-empty (e.g. "call,reporting")
// or the underlying Login will request no events.
func NewEventStream(cfg Config, handler EventHandler, hooks EventStreamHooks, logger *slog.Logger, dialer Dialer) *EventStream {
	if logger == nil {
		logger = slog.Default()
	}
	if dialer == nil {
		dialer = DefaultDialer{}
	}
	if cfg.EventClasses == "" {
		cfg.EventClasses = "call,reporting"
	}
	return &EventStream{
		cfg:         cfg,
		dialer:      dialer,
		handler:     handler,
		hooks:       hooks,
		logger:      logger,
		BaseBackoff: 1 * time.Second,
		MaxBackoff:  30 * time.Second,
	}
}

// Run blocks until ctx is canceled. It loops connect → consume → backoff,
// invoking hooks for observability. Returns ctx.Err() on graceful shutdown.
func (e *EventStream) Run(ctx context.Context) error {
	backoff := e.BaseBackoff
	first := true

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !first && e.hooks.OnReconnect != nil {
			e.hooks.OnReconnect()
		}
		first = false

		err := e.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			e.logger.Warn("ami event stream broke", "err", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > e.MaxBackoff {
			backoff = e.MaxBackoff
		}
	}
}

func (e *EventStream) runOnce(ctx context.Context) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, max(e.cfg.Timeout, 10*time.Second))
	conn, err := e.dialer.Dial(dialCtx, e.cfg)
	dialCancel()
	if err != nil {
		return err
	}

	// Cancel-on-ctx watchdog: closing the connection unblocks any pending
	// read and lets runOnce return cleanly.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	defer closeConn()
	go func() {
		<-ctx.Done()
		closeConn()
	}()

	loginCtx, loginCancel := context.WithTimeout(ctx, max(e.cfg.Timeout, 10*time.Second))
	if err := conn.Login(loginCtx); err != nil {
		loginCancel()
		return err
	}
	loginCancel()

	if e.hooks.OnUp != nil {
		e.hooks.OnUp(true)
		defer e.hooks.OnUp(false)
	}
	e.logger.Info("ami event stream connected", "address", e.cfg.Address, "events", e.cfg.EventClasses)

	for {
		msg, err := conn.NextEvent(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		// Filter responses (e.g. the Login response) — handler only wants events.
		if msg.Get("Event") == "" {
			continue
		}
		e.handler.Handle(ctx, msg)
	}
}

// ErrClosed is returned by NextEvent after Close has been invoked. Exposed
// so tests can assert on shutdown semantics.
var ErrClosed = errors.New("ami: connection closed")
