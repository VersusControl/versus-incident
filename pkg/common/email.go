package common

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html"
	"html/template"
	"net/smtp"
	"path/filepath"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

type EmailProvider struct {
	smtpHost     string
	smtpPort     string
	username     string
	password     string
	to           string
	subject      string
	templatePath string
}

// loginAuth implements smtp.Auth for Office365's LOGIN authentication
type loginAuth struct {
	username, password string
}

func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username, password}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		switch string(fromServer) {
		case "Username:":
			return []byte(a.username), nil
		case "Password:":
			return []byte(a.password), nil
		default:
			return nil, fmt.Errorf("unexpected server challenge: %s", fromServer)
		}
	}
	return nil, nil
}

func NewEmailProvider(cfg config.EmailConfig) *EmailProvider {
	return &EmailProvider{
		smtpHost:     cfg.SMTPHost,
		smtpPort:     cfg.SMTPPort,
		username:     cfg.Username,
		password:     cfg.Password,
		to:           cfg.To,
		subject:      cfg.Subject,
		templatePath: cfg.TemplatePath,
	}
}

func (e *EmailProvider) getAuth() smtp.Auth {
	// Use LOGIN auth for Office365/Outlook
	if strings.Contains(e.smtpHost, "office365.com") ||
		strings.Contains(e.smtpHost, "outlook.com") ||
		strings.Contains(e.smtpHost, "microsoft.com") {
		return LoginAuth(e.username, e.password)
	}
	// Use PLAIN auth for Gmail and others
	return smtp.PlainAuth("", e.username, e.password, e.smtpHost)
}

// Name implements core.AlertProvider.
func (e *EmailProvider) Name() string { return "email" }

func (e *EmailProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := e.templatePath
	if i.Content != nil && utils.IsAgentIncident(*i.Content) {
		tplPath = utils.AgentEmailTemplatePath
	}

	// Parse template
	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var body bytes.Buffer
	if err := tmpl.Execute(&body, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Parse recipients (support multiple comma-separated email addresses)
	recipients := parseRecipients(e.to)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid email recipients found")
	}

	// Set email headers
	headers := make(map[string]string)
	headers["From"] = e.username
	headers["To"] = e.to
	headers["Subject"] = e.subject
	headers["MIME-Version"] = "1.0"
	headers["Content-Type"] = "text/html; charset=UTF-8"

	// Construct message
	var message bytes.Buffer
	for key, value := range headers {
		message.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
	}
	message.WriteString("\r\n")
	message.Write(body.Bytes())

	return e.sendMessage(message.Bytes(), recipients)
}

// sendMessage opens an authenticated SMTP connection and writes the raw
// message to the recipients. Shared by SendAlert (template body) and
// SendAttachment (MIME report).
func (e *EmailProvider) sendMessage(message []byte, recipients []string) error {
	// Get appropriate auth based on SMTP host
	auth := e.getAuth()

	// Create a custom SMTP client with TLS
	addr := fmt.Sprintf("%s:%s", e.smtpHost, e.smtpPort)
	conn, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("failed to dial SMTP server: %w", err)
	}
	defer conn.Close()

	// Only use STARTTLS if not using port 465 (SSL)
	if e.smtpPort != "465" {
		tlsConfig := &tls.Config{
			ServerName: e.smtpHost,
		}
		if err := conn.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("failed to start TLS: %w", err)
		}
	}

	// Authenticate
	if err := conn.Auth(auth); err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// Send the email
	if err := conn.Mail(e.username); err != nil {
		return fmt.Errorf("failed to set sender: %w", err)
	}

	// Add all recipients to the email
	for _, recipient := range recipients {
		if err := conn.Rcpt(recipient); err != nil {
			return fmt.Errorf("failed to set recipient %s: %w", recipient, err)
		}
	}

	w, err := conn.Data()
	if err != nil {
		return fmt.Errorf("failed to open data connection: %w", err)
	}

	if _, err = w.Write(message); err != nil {
		return fmt.Errorf("failed to write message: %w", err)
	}

	if err = w.Close(); err != nil {
		return fmt.Errorf("failed to close data connection: %w", err)
	}

	// Quit the connection
	if err := conn.Quit(); err != nil {
		return fmt.Errorf("failed to quit connection: %w", err)
	}

	return nil
}

// SendAttachment implements core.AttachmentSender: it emails the report as
// a multipart/related MIME message with the PNG inline (referenced by a
// Content-ID) and the redacted caption as the text body.
func (e *EmailProvider) SendAttachment(i *m.Incident, att core.Attachment) error {
	if len(att.Data) == 0 {
		return fmt.Errorf("email: empty attachment")
	}
	recipients := parseRecipients(e.to)
	if len(recipients) == 0 {
		return fmt.Errorf("no valid email recipients found")
	}
	subject := e.subject
	if subject == "" {
		subject = "Incident report"
	}
	msg := buildReportMIME(e.username, e.to, subject, att)
	return e.sendMessage(msg, recipients)
}

// buildReportMIME assembles a multipart/related message with the report
// image inline (cid:report) and the caption as the HTML body. Pure and
// side-effect free so it is unit-testable without an SMTP server.
func buildReportMIME(from, to, subject string, att core.Attachment) []byte {
	const boundary = "versus-report-boundary-8f2a1c"
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/related; boundary=%q\r\n\r\n", boundary)

	// HTML body part (caption).
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	caption := strings.ReplaceAll(html.EscapeString(att.Caption), "\n", "<br/>")
	fmt.Fprintf(&b, "<p>%s</p>\r\n<img src=\"cid:report\" alt=\"incident report\"/>\r\n\r\n", caption)

	// Image part (base64, inline via Content-ID).
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Type: %s\r\n", att.MIME)
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("Content-ID: <report>\r\n")
	fmt.Fprintf(&b, "Content-Disposition: inline; filename=%q\r\n\r\n", att.Filename)
	enc := base64.StdEncoding.EncodeToString(att.Data)
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return b.Bytes()
}

// parseRecipients splits a comma-separated list of email addresses
// and returns a slice of trimmed email addresses
func parseRecipients(to string) []string {
	if to == "" {
		return nil
	}

	// Split the string by commas
	emails := strings.Split(to, ",")

	// Trim whitespace from each email address
	var validEmails []string
	for _, email := range emails {
		trimmedEmail := strings.TrimSpace(email)
		if trimmedEmail != "" {
			validEmails = append(validEmails, trimmedEmail)
		}
	}

	return validEmails
}
