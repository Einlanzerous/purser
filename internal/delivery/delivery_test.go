package delivery

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{Port: 587, From: "a@b.c"}); err == nil {
		t.Error("expected error for missing host")
	}
	if _, err := New(Config{Host: "h", Port: 587}); err == nil {
		t.Error("expected error for missing From")
	}
	s, err := New(Config{Host: "h", Port: 587, From: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.TLS != TLSStartTLS {
		t.Errorf("TLS should default to starttls, got %q", s.cfg.TLS)
	}
}

// TestSend drives a real SMTP exchange against an in-process mock server and
// asserts the envelope + message reach it intact.
func TestSend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var (
		mu       sync.Mutex
		captured strings.Builder
		mailFrom string
		rcptTo   string
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		w("220 purser-test ESMTP")
		inData := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					w("250 OK queued")
					continue
				}
				mu.Lock()
				captured.WriteString(line + "\n")
				mu.Unlock()
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				w("250-localhost")
				w("250 OK")
			case strings.HasPrefix(line, "MAIL FROM:"):
				mu.Lock()
				mailFrom = line
				mu.Unlock()
				w("250 OK")
			case strings.HasPrefix(line, "RCPT TO:"):
				mu.Lock()
				rcptTo = line
				mu.Unlock()
				w("250 OK")
			case line == "DATA":
				inData = true
				w("354 End data with <CR><LF>.<CR><LF>")
			case line == "QUIT":
				w("221 Bye")
				return
			default:
				w("250 OK")
			}
		}
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	s, err := New(Config{Host: "127.0.0.1", Port: port, From: "purser@construct.local", TLS: TLSNone})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Send(context.Background(), "grace@example.com", "Your access", "Hi Grace — token: sw_ABC\n")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-done

	mu.Lock()
	defer mu.Unlock()
	body := captured.String()
	if !strings.Contains(mailFrom, "purser@construct.local") {
		t.Errorf("MAIL FROM wrong: %q", mailFrom)
	}
	if !strings.Contains(rcptTo, "grace@example.com") {
		t.Errorf("RCPT TO wrong: %q", rcptTo)
	}
	if !strings.Contains(body, "Subject: Your access") {
		t.Errorf("subject missing from message:\n%s", body)
	}
	if !strings.Contains(body, "token: sw_ABC") {
		t.Errorf("body missing from message:\n%s", body)
	}
}
