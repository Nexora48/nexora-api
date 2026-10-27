package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a tiny in-process SMTP server that records what it receives.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	rcpt []string
	data string
}

func startSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
	w("220 fake ESMTP")
	inData := false
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.data = data.String()
				s.mu.Unlock()
				w("250 queued")
				continue
			}
			data.WriteString(line + "\n")
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250 fake")
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line
			s.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.rcpt = append(s.rcpt, line)
			s.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			inData = true
			w("354 go")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func TestSMTPSendsOneMessageToOneRecipient(t *testing.T) {
	srv := startSMTP(t)
	host, port, _ := net.SplitHostPort(srv.ln.Addr().String())
	var p int
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	s := SMTP{Host: host, Port: p, From: "Nexora <no-reply@example.com>"}
	if err := s.Send(context.Background(), "nia@example.com", "Your invitation", "Line one\nLine two"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !strings.Contains(srv.from, "no-reply@example.com") || len(srv.rcpt) != 1 || !strings.Contains(srv.rcpt[0], "nia@example.com") {
		t.Fatalf("envelope: from=%q rcpt=%v", srv.from, srv.rcpt)
	}
	for _, want := range []string{"Subject: Your invitation", "To: nia@example.com", "Line one", "Line two"} {
		if !strings.Contains(srv.data, want) {
			t.Errorf("message lacks %q:\n%s", want, srv.data)
		}
	}
}

func TestHeaderInjectionIsRefused(t *testing.T) {
	srv := startSMTP(t)
	host, _, _ := net.SplitHostPort(srv.ln.Addr().String())
	s := SMTP{Host: host, Port: 1, From: "a@example.com"}
	for _, bad := range []string{"a@example.com\r\nBcc: victim@example.com", "a@example.com, b@example.com", "not an address", ""} {
		if err := s.Send(context.Background(), bad, "x", "y"); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	// a subject cannot smuggle headers either (newlines become spaces)
	srv2 := startSMTP(t)
	h2, port2, _ := net.SplitHostPort(srv2.ln.Addr().String())
	var p int
	for _, ch := range port2 {
		p = p*10 + int(ch-'0')
	}
	if err := (SMTP{Host: h2, Port: p, From: "a@example.com"}).Send(context.Background(), "b@example.com", "Hi\r\nBcc: victim@example.com", "body"); err != nil {
		t.Fatal(err)
	}
	srv2.mu.Lock()
	defer srv2.mu.Unlock()
	if strings.Contains(srv2.data, "\nBcc:") {
		t.Fatalf("the subject injected a header:\n%s", srv2.data)
	}
}

func TestNewPicksTheSender(t *testing.T) {
	if _, ok := New("smtp.example.com", 587, "u", "p", "a@b.c", false, nil).(SMTP); !ok {
		t.Error("a host selects SMTP")
	}
	if _, ok := New("", 0, "", "", "a@b.c", true, nil).(Console); !ok {
		t.Error("dev without a host selects the console")
	}
	if New("", 0, "", "", "a@b.c", false, nil) != nil {
		t.Error("production without a host sends nothing")
	}
}
