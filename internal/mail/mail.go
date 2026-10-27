// Package mail sends plain-text e-mail (staff invitations). Nothing here is required to run the API:
// without SMTP_HOST the Console sender prints the message to the log in dev and drops it elsewhere.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// ErrBadAddress is returned for anything that is not one plain address (no header injection through the recipient).
var ErrBadAddress = errors.New("mail: invalid address")

func validAddr(a string) (string, error) {
	p, err := mail.ParseAddress(strings.TrimSpace(a))
	if err != nil || strings.ContainsAny(a, "\r\n") {
		return "", ErrBadAddress
	}
	return p.Address, nil
}

// Console logs the message (dev). The body is logged, so use it only where secrets in mail are acceptable.
type Console struct{ Log *slog.Logger }

func (c Console) Send(_ context.Context, to, subject, body string) error {
	if c.Log != nil {
		c.Log.Info("mail (console sender)", "to", to, "subject", subject, "body", body)
	}
	return nil
}

// Delivers reports that nothing leaves the machine: callers must not tell a person "e-mail sent".
func (Console) Delivers() bool { return false }

// SMTP sends through a relay with STARTTLS when offered and PLAIN auth when a user is set.
type SMTP struct {
	Host, User, Pass, From string
	Port                   int
	Timeout                time.Duration
}

func (s SMTP) Send(ctx context.Context, to, subject, body string) error {
	rcpt, err := validAddr(to)
	if err != nil {
		return err
	}
	from, err := validAddr(s.From)
	if err != nil {
		return fmt.Errorf("mail: MAIL_FROM: %w", err)
	}
	clean := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(v) }
	msg := "From: " + clean(s.From) + "\r\nTo: " + rcpt + "\r\nSubject: " + clean(subject) + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	var auth smtp.Auth
	if s.User != "" {
		auth = smtp.PlainAuth("", s.User, s.Pass, s.Host)
	}
	done := make(chan error, 1)
	go func() {
		done <- smtp.SendMail(fmt.Sprintf("%s:%d", s.Host, s.Port), auth, from, []string{rcpt}, []byte(msg))
	}()
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("mail: timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recorder keeps messages in memory (tests).
type Recorder struct {
	mu   sync.Mutex
	Sent []Message
}

type Message struct{ To, Subject, Body string }

func (r *Recorder) Send(_ context.Context, to, subject, body string) error {
	if _, err := validAddr(to); err != nil {
		return err
	}
	r.mu.Lock()
	r.Sent = append(r.Sent, Message{to, subject, body})
	r.mu.Unlock()
	return nil
}

// New picks the sender from configuration: SMTP when a host is set, otherwise the console in dev and nothing elsewhere.
func New(host string, port int, user, pass, from string, dev bool, log *slog.Logger) Sender {
	switch {
	case host != "":
		return SMTP{Host: host, Port: port, User: user, Pass: pass, From: from}
	case dev:
		return Console{Log: log}
	}
	return nil
}
