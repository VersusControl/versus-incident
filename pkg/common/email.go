package common

import (
	"bytes"
	"fmt"
	"html/template"
	"net/smtp"
	"path/filepath"
	"strconv"
	m "versus-incident/pkg/models"
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

func NewEmailProvider(cfg EmailConfig) *EmailProvider {
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

func (e *EmailProvider) SendAlert(i *m.Incident) error {
	// Parse template
	tmpl, err := template.New(filepath.Base(e.templatePath)).ParseFiles(e.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var body bytes.Buffer
	if err := tmpl.Execute(&body, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Set email headers
	headers := make(map[string]string)
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

	// Authentication
	auth := smtp.PlainAuth("", e.username, e.password, e.smtpHost)

	// Convert port from string to int
	port, err := strconv.Atoi(e.smtpPort)
	if err != nil {
		return fmt.Errorf("invalid SMTP port number: %w", err)
	}

	// Send email without from address
	addr := fmt.Sprintf("%s:%d", e.smtpHost, port)
	if err := smtp.SendMail(addr, auth, e.username, []string{e.to}, message.Bytes()); err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	return nil
}
