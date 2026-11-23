//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func monthlyUsageTestEmailService(port int, useTLS bool) *EmailService {
	return NewEmailService(&notificationEmailMemorySettingRepo{values: map[string]string{
		SettingKeySMTPHost:     "127.0.0.1",
		SettingKeySMTPPort:     strconv.Itoa(port),
		SettingKeySMTPFrom:     "sender@example.org",
		SettingKeySMTPFromName: "报表服务",
		SettingKeySMTPUsername: "sender",
		SettingKeySMTPPassword: "secret",
		SettingKeySMTPUseTLS:   strconv.FormatBool(useTLS),
	}}, nil)
}

func TestSendMonthlyUsageExcel(t *testing.T) {
	const filename = "用量 11月; \"明细\".xlsx"
	const body = "十一月的用量如下。\n.\n详见附件。"
	attachment := bytes.Repeat([]byte{0, 1, 128, 255}, 235)

	for _, tc := range []struct {
		name, stopAt, reply string
		wantUncertain       bool
	}{
		{name: "accepted"},
		{name: "recipient refused", stopAt: "RCPT", reply: "550 invalid recipient"},
		{name: "DATA refused", stopAt: "DATA", reply: "451 try later"},
		{name: "premature success without body", stopAt: "DATA", reply: "250 queued"},
		{name: "malformed initial reply", stopAt: "DATA", reply: "bad reply"},
		{name: "unexpected positive final reply", stopAt: "final", reply: "251 forwarded", wantUncertain: true},
		{name: "unexpected intermediate final reply", stopAt: "final", reply: "354 send more", wantUncertain: true},
		{name: "disconnect before DATA starts", stopAt: "DATA"},
		{name: "temporary final rejection", stopAt: "final", reply: "452 mailbox full"},
		{name: "permanent final rejection", stopAt: "final", reply: "554 message rejected"},
		{name: "final reply lost", stopAt: "final", wantUncertain: true},
		{name: "malformed final reply", stopAt: "final", reply: "bad reply", wantUncertain: true},
		{name: "QUIT reply lost", stopAt: "QUIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.SetDeadline(time.Now().Add(5*time.Second)))
			svc := monthlyUsageTestEmailService(listener.Addr().(*net.TCPAddr).Port, false)
			type result struct {
				uncertain bool
				err       error
			}
			done := make(chan result, 1)
			go func() {
				uncertain, err := svc.SendMonthlyUsageExcel(context.Background(), "收件人 <reader@example.org>", "月报", body, filename, attachment)
				done <- result{uncertain, err}
			}()

			conn, err := listener.Accept()
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			session := textproto.NewConn(conn)
			require.NoError(t, session.PrintfLine("220 reports.test ready"))

			for _, step := range []struct{ stage, command, reply string }{
				{"EHLO", "EHLO localhost", "250-reports.test\r\n250 AUTH PLAIN"},
				{"AUTH", "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00sender\x00secret")), "235 authenticated"},
				{"MAIL", "MAIL FROM:<sender@example.org>", "250 ok"},
				{"RCPT", "RCPT TO:<reader@example.org>", "250 ok"},
				{"DATA", "DATA", "354 go ahead"},
				{"final", "", "250 queued"},
				{"QUIT", "QUIT", "221 bye"},
			} {
				if step.stage == "final" {
					var data bytes.Buffer
					for {
						line, err := session.R.ReadString('\n')
						require.NoError(t, err)
						require.True(t, strings.HasSuffix(line, "\r\n"))
						if line == ".\r\n" {
							break
						}
						data.WriteString(strings.TrimPrefix(line, "."))
					}
					if tc.name == "accepted" {
						checkMonthlyUsageMIME(t, data.Bytes(), body, filename, attachment)
					}
				} else {
					cmd, err := session.ReadLine()
					require.NoError(t, err)
					require.Equal(t, step.command, cmd)
				}
				if step.stage == tc.stopAt {
					if tc.reply != "" {
						require.NoError(t, session.PrintfLine("%s", tc.reply))
					}
					require.NoError(t, session.Close())
					break
				}
				require.NoError(t, session.PrintfLine("%s", step.reply))
			}
			got := <-done
			if tc.stopAt == "" || tc.stopAt == "QUIT" {
				require.NoError(t, got.err)
			} else {
				require.Error(t, got.err)
			}
			require.Equal(t, tc.wantUncertain, got.uncertain)
		})
	}
}

func checkMonthlyUsageMIME(t *testing.T, data []byte, body, filename string, attachment []byte) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	require.NoError(t, err)
	require.NotContains(t, msg.Header, "Bcc")
	require.NotContains(t, msg.Header, "Cc")
	to, err := msg.Header.AddressList("To")
	require.NoError(t, err)
	require.Len(t, to, 1)
	require.Equal(t, &mail.Address{Name: "收件人", Address: "reader@example.org"}, to[0])
	from, err := msg.Header.AddressList("From")
	require.NoError(t, err)
	require.Len(t, from, 1)
	require.Equal(t, "报表服务", from[0].Name)
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	require.NoError(t, err)
	require.Equal(t, "月报", subject)
	require.Equal(t, "1.0", msg.Header.Get("MIME-Version"))

	kind, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/mixed", kind)
	parts := multipart.NewReader(msg.Body, params["boundary"])
	part, err := parts.NextRawPart()
	require.NoError(t, err)
	kind, params, err = mime.ParseMediaType(part.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "text/plain", kind)
	require.Equal(t, "UTF-8", params["charset"])
	require.Equal(t, "quoted-printable", part.Header.Get("Content-Transfer-Encoding"))
	decoded, err := io.ReadAll(quotedprintable.NewReader(part))
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(body, "\n", "\r\n"), string(decoded))

	part, err = parts.NextRawPart()
	require.NoError(t, err)
	require.Equal(t, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", part.Header.Get("Content-Type"))
	kind, params, err = mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	require.NoError(t, err)
	require.Equal(t, "attachment", kind)
	require.Equal(t, filename, params["filename"])
	require.Equal(t, "base64", part.Header.Get("Content-Transfer-Encoding"))
	raw, err := io.ReadAll(part)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\r\n"), "\r\n")
	for _, line := range lines[:len(lines)-1] {
		require.Len(t, line, 76)
	}
	require.LessOrEqual(t, len(lines[len(lines)-1]), 76)
	decoded, err = base64.StdEncoding.DecodeString(string(raw))
	require.NoError(t, err)
	require.Equal(t, attachment, decoded)
	_, err = parts.NextPart()
	require.ErrorIs(t, err, io.EOF)
}

func TestSendMonthlyUsageExcelRejectsUnsafeHeadersBeforeConnect(t *testing.T) {
	srv, port := startFakeSMTPServer(t, false, false)
	svc := monthlyUsageTestEmailService(port, false)
	for _, tc := range []struct{ to, filename string }{
		{"one@example.org,two@example.org", "report.xlsx"},
		{"one@example.org\r\nBcc: two@example.org", "report.xlsx"},
		{"one@example.org", "report.xlsx\r\nBcc: two@example.org"},
	} {
		uncertain, err := svc.SendMonthlyUsageExcel(context.Background(), tc.to, "月报", "正文", tc.filename, nil)
		require.Error(t, err)
		require.False(t, uncertain)
	}
	require.Zero(t, srv.conns.Load())
}

func TestSendMonthlyUsageExcelRequiresConfiguredTLS(t *testing.T) {
	srv, port := startFakeSMTPServer(t, false, false)
	svc := monthlyUsageTestEmailService(port, true)
	uncertain, err := svc.SendMonthlyUsageExcel(context.Background(), "reader@example.org", "月报", "正文", "report.xlsx", nil)
	require.ErrorContains(t, err, "STARTTLS")
	require.False(t, uncertain)
	require.False(t, srv.sawCommand("AUTH"))
}

func TestSMTPDataDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attachment bool
		stall      bool
		wantErr    bool
	}{
		{"active attachment upload", true, false, false},
		{"attachment final reply times out", true, true, true},
		{"ordinary mail keeps total deadline", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out, in := net.Pipe()
				done := make(chan struct{})
				defer func() {
					_ = out.Close()
					<-done
				}()
				data := bytes.Repeat([]byte("x"), 8*4096)
				go func() {
					defer close(done)
					defer in.Close()
					server := textproto.NewConn(in)
					if server.PrintfLine("220 ready") != nil {
						return
					}
					cmd, err := server.ReadLine()
					if err != nil || cmd != "DATA" || server.PrintfLine("354 proceed") != nil {
						return
					}
					for i := 0; i < 8; i++ {
						time.Sleep(4 * time.Second)
						if _, err := io.CopyN(io.Discard, server.R, 4096); err != nil {
							return
						}
					}
					// The body above has no line break. DotWriter appends CRLF and the terminator.
					if _, err := io.ReadFull(server.R, make([]byte, len("\r\n.\r\n"))); err != nil {
						return
					}
					if tc.stall {
						time.Sleep(smtpIOTimeout + time.Second)
					}
					_ = server.PrintfLine("250 accepted")
				}()
				client, err := newSMTPClient(out, "localhost")
				require.NoError(t, err)
				start := time.Now()
				uncertain, err := client.sendData(data, tc.attachment)
				if tc.wantErr {
					var timeout net.Error
					require.ErrorAs(t, err, &timeout)
					require.True(t, timeout.Timeout())
				} else {
					require.NoError(t, err)
					require.Greater(t, time.Since(start), smtpIOTimeout)
				}
				require.Equal(t, tc.stall, uncertain)
			})
		})
	}
}
