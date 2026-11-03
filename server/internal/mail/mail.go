// Package mail sends email through the SMTP server configured in the portal.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/rhisijmuiden/remote-support-tool/server/internal/secret"
	"github.com/rhisijmuiden/remote-support-tool/server/internal/settings"
)

const SettingsKey = "mail"

// Settings is stored in the database. Password is sealed with the server secret.
type Settings struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"` // "starttls" (587), "tls" (465, implicit), "none"
	Auth        string `json:"auth"`     // "auto", "plain", "login", "cram-md5", "scram-sha-256", "xoauth2", "none"
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
	ReplyTo     string `json:"reply_to"`
	HELO        string `json:"helo"`
	// MinTLS is "1.2" or "1.3".
	MinTLS string `json:"min_tls"`
}

func Defaults() Settings {
	return Settings{Port: 587, Security: "starttls", Auth: "auto", MinTLS: "1.2", FromName: "RHIS Remote"}
}

type Service struct {
	Store *settings.Store
	Box   *secret.Box
}

func (s *Service) Load(ctx context.Context) (Settings, error) {
	st := Defaults()
	_, err := s.Store.Get(ctx, SettingsKey, &st)
	return st, err
}

// Save stores settings. An empty password keeps the existing one.
func (s *Service) Save(ctx context.Context, in Settings) error {
	if err := in.validate(); err != nil {
		return err
	}
	old, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if in.Password == "" {
		in.Password = old.Password
	} else if in.Password, err = s.Box.Seal(in.Password); err != nil {
		return err
	}
	return s.Store.Put(ctx, SettingsKey, in)
}

func (st Settings) validate() error {
	if !st.Enabled {
		return nil
	}
	if st.Host == "" || st.Port <= 0 || st.Port > 65535 {
		return errors.New("mail_host_port")
	}
	if !strings.Contains(st.FromAddress, "@") {
		return errors.New("mail_from")
	}
	switch st.Security {
	case "starttls", "tls", "none":
	default:
		return errors.New("mail_security")
	}
	return nil
}

func authType(a string) gomail.SMTPAuthType {
	switch a {
	case "plain":
		return gomail.SMTPAuthPlain
	case "login":
		return gomail.SMTPAuthLogin
	case "cram-md5":
		return gomail.SMTPAuthCramMD5
	case "scram-sha-256":
		return gomail.SMTPAuthSCRAMSHA256
	case "xoauth2":
		return gomail.SMTPAuthXOAUTH2
	case "none":
		return gomail.SMTPAuthNoAuth
	default:
		return gomail.SMTPAuthAutoDiscover
	}
}

func (s *Service) client(st Settings) (*gomail.Client, error) {
	pw, err := s.Box.Open(st.Password)
	if err != nil {
		return nil, err
	}
	minTLS := uint16(tls.VersionTLS12)
	if st.MinTLS == "1.3" {
		minTLS = tls.VersionTLS13
	}
	opts := []gomail.Option{
		gomail.WithPort(st.Port),
		gomail.WithTimeout(20 * time.Second),
		gomail.WithTLSConfig(&tls.Config{ServerName: st.Host, MinVersion: minTLS}),
	}
	switch st.Security {
	case "tls":
		opts = append(opts, gomail.WithSSL())
	case "starttls":
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	case "none":
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	}
	if st.Auth != "none" && st.Username != "" {
		opts = append(opts, gomail.WithSMTPAuth(authType(st.Auth)), gomail.WithUsername(st.Username), gomail.WithPassword(pw))
	}
	if st.HELO != "" {
		opts = append(opts, gomail.WithHELO(st.HELO))
	}
	return gomail.NewClient(st.Host, opts...)
}

// Message is a branded email with a plain-text and an HTML part.
type Message struct {
	To      []string
	Subject string
	Heading string
	Body    []string // paragraphs
}

func (s *Service) Send(ctx context.Context, msg Message) error {
	st, err := s.Load(ctx)
	if err != nil {
		return err
	}
	return s.SendWith(ctx, st, msg)
}

// SendWith sends using st, which lets the portal test settings before saving them.
func (s *Service) SendWith(ctx context.Context, st Settings, msg Message) error {
	if !st.Enabled {
		return errors.New("mail_disabled")
	}
	if err := st.validate(); err != nil {
		return err
	}
	c, err := s.client(st)
	if err != nil {
		return err
	}
	m := gomail.NewMsg()
	if err := m.FromFormat(st.FromName, st.FromAddress); err != nil {
		return err
	}
	if err := m.To(msg.To...); err != nil {
		return err
	}
	if st.ReplyTo != "" {
		m.ReplyTo(st.ReplyTo)
	}
	m.Subject(msg.Subject)
	m.SetDate()
	m.SetMessageID()
	m.SetBodyString(gomail.TypeTextPlain, msg.Heading+"\n\n"+strings.Join(msg.Body, "\n\n"))
	m.AddAlternativeString(gomail.TypeTextHTML, renderHTML(st.FromName, msg))
	if err := c.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

// renderHTML builds a simple, client-safe email in the RHIS house style (inline styles only).
func renderHTML(brand string, msg Message) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><body style="margin:0;background:#F6F7FB;font-family:Archivo,Segoe UI,Arial,sans-serif;color:#14213D">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center" style="padding:32px 16px">`)
	b.WriteString(`<table role="presentation" width="560" cellpadding="0" cellspacing="0" style="max-width:560px;width:100%;background:#FFFFFF;border-radius:12px;overflow:hidden">`)
	b.WriteString(`<tr><td style="background:#14213D;padding:20px 28px;color:#FFFFFF;font-weight:800;font-size:18px;letter-spacing:.02em">` + html.EscapeString(brand) + `<span style="color:#FF6B57"> &#9679;</span></td></tr>`)
	b.WriteString(`<tr><td style="padding:28px"><h1 style="margin:0 0 16px;font-size:22px;line-height:1.25">` + html.EscapeString(msg.Heading) + `</h1>`)
	for _, p := range msg.Body {
		b.WriteString(`<p style="margin:0 0 14px;font-size:15px;line-height:1.6;color:#4F5B73">` + html.EscapeString(p) + `</p>`)
	}
	b.WriteString(`</td></tr></table></td></tr></table></body></html>`)
	return b.String()
}
