package control

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type StreamSupervisorClient interface {
	Connect(context.Context) error
	Disconnect()
	Heartbeat([]string, bool) error
}

type StreamSettings struct {
	Labels            []string
	HeartbeatInterval time.Duration
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
}

type StreamSupervisor struct {
	Client            StreamSupervisorClient
	Labels            []string
	HeartbeatInterval time.Duration
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	Jitter            func(time.Duration) time.Duration
	Sleep             func(context.Context, time.Duration) error
	Busy              func() bool
	OnConnected       func() error
	OnHeartbeat       func() error
	Settings          func() StreamSettings
}

func (s StreamSupervisor) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	jitter := s.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	settings := s.operationalSettings()
	backoff := settings.ReconnectMin
	for {
		settings = s.operationalSettings()
		if backoff < settings.ReconnectMin {
			backoff = settings.ReconnectMin
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Client.Connect(ctx); err != nil {
			if !isTransientTransportError(err) {
				return fmt.Errorf("connect runner control stream: %w", err)
			}
			if err := sleep(ctx, jitterDelay(jitter, backoff, settings.ReconnectMax)); err != nil {
				return err
			}
			backoff = nextBackoff(backoff, settings.ReconnectMax)
			continue
		}
		backoff = settings.ReconnectMin
		if s.OnConnected != nil {
			if err := s.OnConnected(); err != nil {
				s.Client.Disconnect()
				if !isTransientTransportError(err) {
					return fmt.Errorf("resume runner control stream: %w", err)
				}
				if err := sleep(ctx, jitterDelay(jitter, backoff, settings.ReconnectMax)); err != nil {
					return err
				}
				backoff = nextBackoff(backoff, settings.ReconnectMax)
				continue
			}
		}
		if err := s.heartbeat(); err != nil {
			s.Client.Disconnect()
			if !isTransientTransportError(err) {
				return fmt.Errorf("send runner heartbeat: %w", err)
			}
			if err := sleep(ctx, jitterDelay(jitter, backoff, settings.ReconnectMax)); err != nil {
				return err
			}
			backoff = nextBackoff(backoff, settings.ReconnectMax)
			continue
		}
		ticker := time.NewTicker(settings.HeartbeatInterval)
		failed := false
		for !failed {
			select {
			case <-ctx.Done():
				ticker.Stop()
				s.Client.Disconnect()
				return ctx.Err()
			case <-ticker.C:
				if err := s.heartbeat(); err != nil {
					if !isTransientTransportError(err) {
						ticker.Stop()
						s.Client.Disconnect()
						return fmt.Errorf("send runner heartbeat: %w", err)
					}
					failed = true
				}
			}
		}
		ticker.Stop()
		s.Client.Disconnect()
		if err := sleep(ctx, jitterDelay(jitter, backoff, settings.ReconnectMax)); err != nil {
			return err
		}
		backoff = nextBackoff(backoff, settings.ReconnectMax)
	}
}

func (s StreamSupervisor) busy() bool {
	return s.Busy != nil && s.Busy()
}

func (s StreamSupervisor) heartbeat() error {
	if s.OnHeartbeat != nil {
		if err := s.OnHeartbeat(); err != nil {
			return err
		}
	}
	return s.Client.Heartbeat(s.settings().Labels, s.busy())
}

func (s StreamSupervisor) settings() StreamSettings {
	if s.Settings != nil {
		return s.Settings()
	}
	return StreamSettings{Labels: append([]string(nil), s.Labels...), HeartbeatInterval: s.HeartbeatInterval, ReconnectMin: s.ReconnectMin, ReconnectMax: s.ReconnectMax}
}

func (s StreamSupervisor) operationalSettings() StreamSettings {
	settings := s.settings()
	if settings.HeartbeatInterval <= 0 {
		settings.HeartbeatInterval = s.HeartbeatInterval
	}
	if settings.ReconnectMin <= 0 {
		settings.ReconnectMin = s.ReconnectMin
	}
	if settings.ReconnectMax < settings.ReconnectMin {
		settings.ReconnectMax = s.ReconnectMax
	}
	return settings
}

func (s StreamSupervisor) validate() error {
	if s.Client == nil {
		return errors.New("runner stream supervisor client is required")
	}
	if s.HeartbeatInterval <= 0 || s.ReconnectMin <= 0 || s.ReconnectMax < s.ReconnectMin {
		return errors.New("runner stream supervisor timing is invalid")
	}
	return nil
}

func isTransientTransportError(err error) bool {
	switch status.Code(err) {
	case codes.OK, codes.Canceled, codes.Unknown, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted, codes.Internal, codes.Unavailable, codes.DataLoss:
		return true
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied, codes.FailedPrecondition, codes.OutOfRange, codes.Unimplemented, codes.Unauthenticated:
		return false
	default:
		return true
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func jitterDelay(jitter func(time.Duration) time.Duration, base, maximum time.Duration) time.Duration {
	delay := jitter(base)
	if delay <= 0 {
		return base
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func defaultJitter(base time.Duration) time.Duration {
	if base <= 1 {
		return base
	}
	delta, err := rand.Int(rand.Reader, big.NewInt(int64(base/4)+1))
	if err != nil {
		return base
	}
	return base + time.Duration(delta.Int64())
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
