package service

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

type smtpMessage struct {
	envelopeFrom string
	envelopeTo   string
	data         []byte
}

func buildSMTPMessage(config *SMTPConfig, to, subject, body string) (smtpMessage, error) {
	message, err := buildSMTPHeaders(config, to, subject)
	if err != nil {
		return smtpMessage{}, err
	}
	data := bytes.NewBuffer(message.data)
	data.WriteString("Content-Type: text/html; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	if err := writeSMTPBody(data, body); err != nil {
		return smtpMessage{}, err
	}
	message.data = data.Bytes()
	return message, nil
}

func writeSMTPBody(w io.Writer, body string) error {
	bodyWriter := quotedprintable.NewWriter(w)
	if _, err := bodyWriter.Write([]byte(body)); err != nil {
		return fmt.Errorf("encode email body: %w", err)
	}
	if err := bodyWriter.Close(); err != nil {
		return fmt.Errorf("close email body encoder: %w", err)
	}
	return nil
}

func buildSMTPHeaders(config *SMTPConfig, to, subject string) (smtpMessage, error) {
	if config == nil {
		return smtpMessage{}, errors.New("missing SMTP configuration")
	}

	fromAddress, err := parseSMTPAddress(config.From, "from")
	if err != nil {
		return smtpMessage{}, err
	}
	recipientAddress, err := parseSMTPAddress(to, "recipient")
	if err != nil {
		return smtpMessage{}, err
	}
	messageID, err := generateEmailMessageID(fromAddress.Address, config.Host)
	if err != nil {
		return smtpMessage{}, fmt.Errorf("generate message ID: %w", err)
	}

	fromName := sanitizeEmailHeader(config.FromName)
	if strings.TrimSpace(fromName) == "" {
		fromName = fromAddress.Name
	}
	fromHeader := (&mail.Address{
		Name:    fromName,
		Address: fromAddress.Address,
	}).String()
	toHeader := (&mail.Address{
		Name:    recipientAddress.Name,
		Address: recipientAddress.Address,
	}).String()
	subjectHeader := mime.QEncoding.Encode("UTF-8", sanitizeEmailHeader(subject))

	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\n", fromHeader)
	fmt.Fprintf(&message, "To: %s\r\n", toHeader)
	fmt.Fprintf(&message, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&message, "Message-ID: %s\r\n", messageID)
	fmt.Fprintf(&message, "Subject: %s\r\n", subjectHeader)
	message.WriteString("MIME-Version: 1.0\r\n")

	return smtpMessage{
		envelopeFrom: fromAddress.Address,
		envelopeTo:   recipientAddress.Address,
		data:         message.Bytes(),
	}, nil
}

func parseSMTPAddress(value, field string) (*mail.Address, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("invalid SMTP %s address: contains a line break", field)
	}

	cleaned := strings.TrimSpace(value)
	address, err := mail.ParseAddress(cleaned)
	if err != nil || strings.TrimSpace(address.Address) == "" {
		if err == nil {
			err = fmt.Errorf("address is empty")
		}
		return nil, fmt.Errorf("invalid SMTP %s address: %w", field, err)
	}
	return address, nil
}

func generateEmailMessageID(fromAddress, smtpHost string) (string, error) {
	randomID := make([]byte, 16)
	if _, err := rand.Read(randomID); err != nil {
		return "", err
	}

	domain := strings.TrimSpace(sanitizeEmailHeader(smtpHost))
	if at := strings.LastIndexByte(fromAddress, '@'); at >= 0 && at < len(fromAddress)-1 {
		domain = fromAddress[at+1:]
	}
	domain = strings.Trim(domain, "[]<>")
	if domain == "" {
		domain = "localhost"
	}

	return fmt.Sprintf("<%s@%s>", hex.EncodeToString(randomID), domain), nil
}
