package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"unicode"
	"unicode/utf8"
)

const xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// SendMonthlyUsageExcel sends a UTF-8 plain text message and an XLSX attachment
// to one recipient. On error, uncertain indicates that SMTP may have accepted
// the message and retrying could deliver a duplicate.
func (s *EmailService) SendMonthlyUsageExcel(ctx context.Context, to, subject, body, filename string, attachment []byte) (uncertain bool, err error) {
	config, err := s.GetSMTPConfig(ctx)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(filename) == "" || !utf8.ValidString(filename) || strings.IndexFunc(filename, unicode.IsControl) >= 0 {
		return false, errors.New("invalid attachment filename")
	}
	message, err := buildSMTPHeaders(config, to, subject)
	if err != nil {
		return false, err
	}
	data := bytes.NewBuffer(message.data)
	mixed := multipart.NewWriter(data)
	fmt.Fprintf(data, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mixed.Boundary())
	text, err := mixed.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=UTF-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return false, err
	}
	if err := writeSMTPBody(text, body); err != nil {
		return false, err
	}
	file, err := mixed.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {xlsxMediaType},
		"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": filename})},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return false, err
	}
	if err := writeMIMEBase64(file, attachment); err != nil {
		return false, err
	}
	if err := mixed.Close(); err != nil {
		return false, err
	}
	message.data = data.Bytes()
	return s.sendSMTPMessage(ctx, config, message)
}

func writeMIMEBase64(w io.Writer, data []byte) error {
	// 57 bytes encode to one 76-character MIME line without padding.
	var line [76 + 2]byte
	for len(data) > 0 {
		n := min(len(data), 57)
		base64.StdEncoding.Encode(line[:], data[:n])
		size := base64.StdEncoding.EncodedLen(n)
		line[size], line[size+1] = '\r', '\n'
		if _, err := w.Write(line[:size+2]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
