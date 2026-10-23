package sms

import (
	"context"
	"log/slog"
)

// Sender delivers an SMS. Real providers (MSG91) implement this in M6.
type Sender interface {
	Send(ctx context.Context, phone, message string) error
}

// ConsoleSender logs messages instead of sending them (dev/test).
type ConsoleSender struct{ Log *slog.Logger }

func (c ConsoleSender) Send(_ context.Context, phone, message string) error {
	c.Log.Info("sms (console)", "phone", phone, "message", message)
	return nil
}

// Recorder keeps sent messages in memory for tests.
type Recorder struct{ Messages []Message }

type Message struct{ Phone, Body string }

func (r *Recorder) Send(_ context.Context, phone, message string) error {
	r.Messages = append(r.Messages, Message{phone, message})
	return nil
}
