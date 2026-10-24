// Package notify sends push notifications to registered devices.
package notify

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is deliberately small: payloads never carry visitor details, the app fetches them after opening.
type Message struct {
	Title string
	Body  string
	Data  map[string]string
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

type Service struct {
	pool   *pgxpool.Pool
	sender Sender
	log    *slog.Logger
}

func NewService(pool *pgxpool.Pool, sender Sender, log *slog.Logger) *Service {
	if sender == nil {
		sender = LogSender{Log: log}
	}
	return &Service{pool: pool, sender: sender, log: log}
}

// ToUsers pushes to every registered device of the given users and drops dead tokens.
func (s *Service) ToUsers(ctx context.Context, userIDs []uuid.UUID, m Message) {
	if len(userIDs) == 0 {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT fcm_token FROM devices WHERE user_id = ANY($1) AND fcm_token IS NOT NULL AND fcm_token <> ''`, userIDs)
	if err != nil {
		return
	}
	var tokens []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tokens = append(tokens, t)
		}
	}
	rows.Close()
	for _, t := range tokens {
		err := s.sender.Send(ctx, t, m)
		if errors.Is(err, ErrUnregistered) {
			_, _ = s.pool.Exec(ctx, `UPDATE devices SET fcm_token=NULL WHERE fcm_token=$1`, t)
		} else if err != nil && s.log != nil {
			s.log.Warn("push failed", "err", err)
		}
	}
}
