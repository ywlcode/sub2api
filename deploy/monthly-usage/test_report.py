import json
from email import message_from_bytes, policy
import re
import smtplib
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch

import report
from workbook import COLUMNS


class Reports(unittest.TestCase):
    def test_due_in_beijing(self):
        t = datetime(2027, 1, 31, 16, tzinfo=timezone.utc)
        self.assertEqual(list(report.months("2026-12", t)), ["2026-12", "2027-01"])
        self.assertEqual(list(report.months("2027-02", t)), [])
        for bad in ("2026-1", "2026-00", "2026-11\n"):
            with self.assertRaises(ValueError):
                report.period(bad)

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.state = Path(tmp.name)
        self.site, self.month = "boscserver", "2026-10"
        self.path = report.archive(self.state, self.site, self.month)
        self.marker = self.path.with_suffix(".delivery.json")
        self.render_record = self.path.with_suffix(".render.json")

        def query(sql, month=None, output=None):
            if sql == report.SMTP_CONFIG:
                return json.dumps(dict(smtp_host="smtp.example.invalid", smtp_port="465",
                                       smtp_username="user", smtp_password="pass",
                                       smtp_from="from@example.invalid")).encode()
            self.assertEqual(month, self.month)
            output.write(b"{}\n" + (",".join(COLUMNS) + "\n").encode())

        db = patch("report.db", side_effect=query)
        self.db = db.start()
        self.addCleanup(db.stop)
        self.smtp = MagicMock()
        self.server = self.smtp.return_value
        self.server.esmtp_features = {}
        self.server.mail.return_value = self.server.rcpt.return_value = (250, b"OK")
        self.server.docmd.return_value = (354, b"go ahead")
        self.server.getreply.return_value = (250, b"accepted")

    def send(self, **kwargs):
        report.deliver(self.state, self.site, self.month, "to@example.invalid", self.smtp, **kwargs)

    def message(self):
        chunks = [bytes(call.args[0]) for call in self.server.send.call_args_list]
        self.assertEqual(chunks.pop(), b".\r\n")
        wire = b"".join(chunks)
        self.assertNotIn(b"\r\n.\r\n", wire)
        return message_from_bytes(re.sub(br"(?m)^\.\.", b".", wire), policy=policy.default)

    def test_accepted_once(self):
        self.send()
        self.send()
        self.assertEqual(json.loads(self.marker.read_text())["status"], "accepted")
        self.server.docmd.assert_called_once_with("DATA")
        self.server.rcpt.assert_called_once_with("to@example.invalid")
        attachment, = self.message().iter_attachments()
        self.assertEqual(attachment.get_filename(), self.path.name)
        self.assertEqual(attachment.get_payload(decode=True), self.path.read_bytes())

    def test_manual_revision_has_independent_receipt_and_attachment(self):
        self.send()
        original = self.marker.read_bytes()
        self.server.reset_mock()
        revised = report.archive(self.state, self.site, self.month, 2)
        for _ in range(2):
            self.send(revision=2, note="历史差额未计入。\n.\n..")
        self.send()
        self.server.getreply.assert_called_once()
        message = self.message()
        self.assertIn("修订版 2", message["Subject"])
        self.assertIn("历史差额未计入。\r\n.\r\n..", message.get_body().get_content())
        attachment, = message.iter_attachments()
        self.assertEqual(attachment.get_filename(), revised.name)
        self.assertEqual(attachment.get_payload(decode=True), revised.read_bytes())
        self.assertEqual(self.marker.read_bytes(), original)

    def test_first_send_checks_archive_provenance(self):
        for kind in ("no record", "old version", "altered file", "broken record", "current"):
            with self.subTest(kind=kind):
                report.render(self.state, self.site, self.month)
                if kind == "no record":
                    self.render_record.unlink()
                elif kind == "old version":
                    record = json.loads(self.render_record.read_text())
                    record["version"] = "three-sheets-admin-filter-v1"
                    self.render_record.write_text(json.dumps(record))
                elif kind == "altered file":
                    self.path.write_bytes(b"stale data")
                elif kind == "broken record":
                    self.render_record.write_text("{")
                with patch("report.render", wraps=report.render) as render:
                    self.send()
                    self.assertEqual(render.call_count, int(kind != "current"))
                attachment, = self.message().iter_attachments()
                self.assertEqual(attachment.get_payload(decode=True), self.path.read_bytes())
                self.assertTrue(self.path.read_bytes().startswith(b"PK"))
                self.marker.unlink()
                self.server.reset_mock()

    def test_failed_rebuild_never_sends_legacy_data(self):
        self.path.write_bytes(b"old archive with administrator usage")
        with patch("report.render", side_effect=RuntimeError("snapshot unavailable")) as render:
            with self.assertRaisesRegex(RuntimeError, "snapshot unavailable"):
                self.send()
        render.assert_called_once_with(self.state, self.site, self.month, None)
        self.assertEqual(self.path.read_bytes(), b"old archive with administrator usage")
        self.smtp.assert_not_called()
        self.assertFalse(self.marker.exists())

    def test_receipts_protect_legacy_archives(self):
        self.path.write_bytes(b"archived")
        for status in ("accepted", "sending"):
            with self.subTest(status=status):
                self.marker.write_text(json.dumps({"status": status}))
                if status == "accepted":
                    self.send()
                else:
                    with self.assertRaises(report.Uncertain):
                        self.send()
                self.assertEqual(self.path.read_bytes(), b"archived")
                self.assertFalse(self.render_record.exists())
        self.db.assert_not_called()
        self.smtp.assert_not_called()

    def test_initial_reply_failure_is_retryable(self):
        for code in (250, 251, 451, 550, -1):
            with self.subTest(code=code):
                self.server.docmd.return_value = (code, b"unexpected")
                with self.assertRaisesRegex(RuntimeError, "expected 354"):
                    self.send()
                self.server.send.assert_not_called()
                self.assertFalse(self.marker.exists())
        self.server.docmd.side_effect = smtplib.SMTPServerDisconnected()
        with self.assertRaises(smtplib.SMTPServerDisconnected):
            self.send()
        self.assertFalse(self.marker.exists())

    def test_uncertain_after_data_is_never_retried(self):
        for result in (251, 354, 600, -1, smtplib.SMTPServerDisconnected()):
            with self.subTest(result=result):
                self.server.getreply.side_effect = result if isinstance(result, Exception) else None
                self.server.getreply.return_value = (result, b"unexpected")
                for _ in range(2):
                    with self.assertRaises(report.Uncertain):
                        self.send()
                self.server.docmd.assert_called_once_with("DATA")
                self.assertEqual(json.loads(self.marker.read_text())["status"], "sending")
                self.marker.unlink()
                self.server.reset_mock()

    def test_body_write_failure_keeps_sending_marker(self):
        self.server.send.side_effect = smtplib.SMTPServerDisconnected()
        for _ in range(2):
            with self.assertRaises(report.Uncertain):
                self.send()
        self.server.send.assert_called_once()
        self.assertEqual(json.loads(self.marker.read_text())["status"], "sending")

    def test_smtp_framing_across_chunks(self):
        prefix = b"x\r\n" * 10922
        # Split the first escaped dot pair across the 32 KiB write boundary.
        wire = b"x" + prefix + b".\r\n.."
        with patch.object(report.EmailMessage, "as_bytes", return_value=wire):
            self.send()
        chunks = [bytes(call.args[0]) for call in self.server.send.call_args_list]
        self.assertGreater(len(chunks), 2)
        self.assertEqual(b"".join(chunks), b"x" + prefix + b"..\r\n...\r\n.\r\n")

    def test_rejection_retry(self):
        for code in (451, 554):
            self.server.getreply.return_value = (code, b"rejected")
            with self.assertRaisesRegex(RuntimeError, "DATA rejected"):
                self.send()
            self.assertFalse(self.marker.exists())
        self.server.getreply.return_value = (250, b"OK")
        self.send()

    def test_failure_before_data(self):
        self.server.rcpt.return_value = (550, b"no")
        with self.assertRaises(RuntimeError):
            self.send()
        self.server.docmd.assert_not_called()
        self.assertFalse(self.marker.exists())

    def test_single_recipient(self):
        for addr in ("a@b.c,other@b.c", "a@b.c\r\nBcc: b@c.d", "no-domain"):
            with self.assertRaises(ValueError):
                report.mailbox(addr)


if __name__ == "__main__":
    unittest.main()
