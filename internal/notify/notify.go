// Package notify sends push notifications to registered devices.
package notify

import (
	"context"
	"errors"
	"log/slog"
)

// Message is deliberately small: payloads never carry visitor details, the app fetches them after opening.
type Message struct {
	Title   string
	Body    string
	Data    map[string]string
	Channel string // Android channel id; critical alerts use a loud one
}

// ErrUnregistered means the token is dead and must be dropped.
var ErrUnregistered = errors.New("push token unregistered")

type Sender interface {
	Send(ctx context.Context, token string, m Message) error
}

// LogSender is used when no FCM credentials are configured (dev, tests).
type LogSender struct{ Log *slog.Logger }

func (l LogSender) Send(_ context.Context, token string, m Message) error {
	if l.Log != nil {
		l.Log.Info("push (log sender)", "token_suffix", suffix(token), "title", m.Title, "data", m.Data)
	}
	return nil
}

func suffix(t string) string {
	if len(t) <= 6 {
		return t
	}
	return t[len(t)-6:]
}
