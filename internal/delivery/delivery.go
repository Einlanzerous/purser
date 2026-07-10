// Package delivery sends Purser credential blocks to invited people over SMTP.
// It mirrors the construct-server house SMTP pattern (see lyceum/internal/
// delivery) but ships a plain-text message body rather than an attachment.
//
// Copy-paste "delivery" needs no code here — the orchestrator simply returns the
// rendered block for the operator to paste. This package is only used when the
// operator chooses email delivery.
package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// TLS modes for the SMTP connection.
const (
	TLSStartTLS = "starttls" // plaintext connect, upgrade via STARTTLS (587)
	TLSImplicit = "implicit" // TLS from the first byte (465)
	TLSNone     = "none"     // plaintext (local capture servers / tests)
)

// Config describes the upstream SMTP relay. From is the envelope sender and the
// message From header; it must be an address the relay may send as.
type Config struct {
	Host     string
	Port     int
	Username string // optional; empty => no AUTH
	Password string
	From     string
	TLS      string // one of the TLS* constants; defaults to STARTTLS
}

func (c Config) addr() string { return net.JoinHostPort(c.Host, fmt.Sprintf("%d", c.Port)) }

// Sender ships credential blocks over SMTP.
type Sender struct {
	cfg  Config
	dial func(network, addr string) (net.Conn, error) // overridable in tests
}

// New builds a Sender, validating the essential relay fields up front.
func New(cfg Config) (*Sender, error) {
	if cfg.Host == "" || cfg.Port == 0 {
		return nil, errors.New("delivery: SMTP host and port are required")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("delivery: From address is required")
	}
	if cfg.TLS == "" {
		cfg.TLS = TLSStartTLS
	}
	return &Sender{cfg: cfg}, nil
}

// Send delivers a plain-text message to toAddr. It satisfies invite.Emailer.
func (s *Sender) Send(ctx context.Context, toAddr, subject, body string) error {
	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("delivery: invalid From %q: %w", s.cfg.From, err)
	}
	to, err := mail.ParseAddress(toAddr)
	if err != nil {
		return fmt.Errorf("delivery: invalid recipient %q: %w", toAddr, err)
	}

	msg := buildMessage(s.cfg.From, to.Address, subject, body)

	client, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("delivery: auth: %w", err)
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return fmt.Errorf("delivery: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("delivery: RCPT TO: %w", err)
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("delivery: DATA: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("delivery: write message: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("delivery: close data: %w", err)
	}
	return client.Quit()
}

// connect dials the relay and returns a ready smtp.Client, handling the three
// TLS modes.
func (s *Sender) connect(ctx context.Context) (*smtp.Client, error) {
	dialer := s.dial
	if dialer == nil {
		nd := &net.Dialer{Timeout: 15 * time.Second}
		dialer = func(network, addr string) (net.Conn, error) {
			return nd.DialContext(ctx, network, addr)
		}
	}

	conn, err := dialer("tcp", s.cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("delivery: dial %s: %w", s.cfg.addr(), err)
	}

	if s.cfg.TLS == TLSImplicit {
		conn = tls.Client(conn, &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12})
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("delivery: smtp client: %w", err)
	}

	if s.cfg.TLS == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("delivery: starttls: %w", err)
			}
		}
	}
	return client, nil
}

// buildMessage renders a minimal RFC 5322 plain-text message.
func buildMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	// Normalize to CRLF line endings for SMTP.
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}
