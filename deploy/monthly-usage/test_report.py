import json
from email import message_from_bytes, policy
import smtplib
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch

import report


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
        self.path.write_bytes(b"mock workbook")
        self.marker = self.path.with_suffix(".delivery.json")
        db = patch("report.db", return_value=json.dumps(dict(smtp_host="smtp.example.invalid",
            smtp_port="465", smtp_username="user", smtp_password="pass",
            smtp_from="from@example.invalid")).encode())
        db.start()
        self.addCleanup(db.stop)
        self.smtp = MagicMock()
        self.smtp.return_value.esmtp_features = {}
        for method in ("mail", "rcpt", "data"):
            getattr(self.smtp.return_value, method).return_value = (250, b"OK")

    def send(self):
        report.deliver(self.state, self.site, self.month, "to@example.invalid", self.smtp)

    def test_accepted_once(self):
        self.send()
        self.send()
        self.assertEqual(json.loads(self.marker.read_text())["status"], "accepted")
        self.smtp.return_value.data.assert_called_once()
        self.smtp.return_value.rcpt.assert_called_once_with("to@example.invalid")
        self.assertIn(b"Sub2API-boscserver-2026-10.xlsx", self.smtp.return_value.data.call_args.args[0])

    def test_manual_revision_has_independent_receipt_and_attachment(self):
        self.send()
        original = self.marker.read_bytes()
        revised = report.archive(self.state, self.site, self.month, 2)
        revised.write_bytes(b"updated workbook")
        for _ in range(2):
            report.deliver(self.state, self.site, self.month, "to@example.invalid", self.smtp,
                           revision=2, note="历史差额未计入。")
        self.send()
        self.assertEqual(self.smtp.return_value.data.call_count, 2)
        message = message_from_bytes(self.smtp.return_value.data.call_args.args[0], policy=policy.default)
        self.assertIn("修订版 2", message["Subject"])
        self.assertIn("历史差额未计入。", message.get_body().get_content())
        attachment, = message.iter_attachments()
        self.assertEqual(attachment.get_filename(), revised.name)
        self.assertEqual(attachment.get_payload(decode=True), b"updated workbook")
        self.assertEqual(self.marker.read_bytes(), original)

    def test_socket_failure_during_data_is_never_retried(self):
        self.smtp.return_value.data.side_effect = smtplib.SMTPServerDisconnected()
        for _ in range(2):
            with self.assertRaises(report.Uncertain):
                self.send()
        self.smtp.return_value.data.assert_called_once()

    def test_rejection_retry(self):
        self.smtp.return_value.data.return_value = (451, b"busy")
        with self.assertRaises(RuntimeError):
            self.send()
        self.assertFalse(self.marker.exists())
        self.smtp.return_value.data.return_value = (250, b"OK")
        self.send()

    def test_failure_before_data(self):
        self.smtp.return_value.rcpt.return_value = (550, b"no")
        with self.assertRaises(RuntimeError):
            self.send()
        self.smtp.return_value.data.assert_not_called()
        self.assertFalse(self.marker.exists())

    def test_single_recipient(self):
        for addr in ("a@b.c,other@b.c", "a@b.c\r\nBcc: b@c.d", "no-domain"):
            with self.assertRaises(ValueError):
                report.mailbox(addr)


if __name__ == "__main__":
    unittest.main()
