import contextlib
import http.server
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
from unittest import mock

from collect import compare_billing

from soak import Evidence, MODEL, Monitor, StreamResult, failure_category, model_request, parse_docker_line, scrub, utc


class SoakTests(unittest.TestCase):
    def config(self, **kwargs):
        value = {"run_id":"soak-test", "role":"secondary", "model":MODEL,
                 "start_at":utc(time.time()-1), "end_at":utc(time.time()+90),
                 "api_key_id":710, "secondary_url":"http://127.0.0.1:8080"}
        return {**value, **kwargs}

    def test_stream_requires_successful_terminal_marker_and_eof(self):
        for terminal, status, eof, success in [("response.completed","completed",True,True),
                ("response.completed","completed",False,False),("response.failed","failed",True,False),
                ("response.incomplete","incomplete",True,False),(None,None,True,False)]:
            with self.subTest(terminal=terminal,eof=eof):
                s = StreamResult("OK")
                wire = b'data: {"type":"response.output_text.delta","delta":"OK"}\r\n\r\n'
                if terminal:
                    wire += ("data: " + json.dumps({"type":terminal,"response":{"status":status}})+"\r\n\r\n").encode()
                for byte in wire:
                    s.feed(bytes([byte]))
                self.assertEqual(s.finish(eof)["success"], success)

    def test_error_before_completed_is_not_hidden(self):
        s = StreamResult("OK")
        for item in [{"type":"error","message":"upstream failure"},
                     {"type":"response.output_text.delta","delta":"OK"},
                     {"type":"response.completed","response":{"status":"completed"}}]:
            s.feed(("data: "+json.dumps(item)+"\n\n").encode())
        self.assertFalse(s.finish(True)["success"])

    def test_invalid_and_oversized_sse(self):
        s = StreamResult("OK")
        with self.assertRaises(ValueError):
            s.feed(b"data: broken\n\n")
        with self.assertRaises(ValueError):
            StreamResult("OK").feed(b"x"*(2*1024*1024+1))

    def test_log_parsers_and_redaction(self):
        for line in ['2026-11-18T14:00:00Z {"level":"INFO","msg":"site synchronization completed","duration_ms":3}',
                     '2026-11-18T14:00:00Z 2026-11-18T22:00:00+0800\tINFO\tsite synchronization completed\t{"duration_ms":3}']:
            self.assertEqual(parse_docker_line(line)["record"]["msg"], "site synchronization completed")
        self.assertIsNone(parse_docker_line("unparseable"))
        self.assertNotIn("sk-secret123", scrub("key sk-secret123 Bearer anothersecret password=p"))
        self.assertNotIn("anothersecret", scrub("Bearer anothersecret"))

    def test_sync_gap_dedup_and_threshold_recovery(self):
        with tempfile.TemporaryDirectory() as directory:
            m = Monitor(self.config(), directory)
            record = {"msg":"site synchronization completed","duration_ms":100}
            m.sync_event(1000, record, "a")
            m.sync_event(1000, record, "a")
            m.sync_event(2025, record, "b")
            m.sync_event(1001, record, "historical-replay")
            self.assertEqual(m.e.get("max_sync_gap_s"),1025)
            self.assertEqual(m.e.get("last_sync")["at"],2025)
            self.assertEqual(len(m.e.rows("sync_gap")),1)
            m.alert("gap",True,{})
            m.alert("gap",True,{})
            m.alert("gap",False,{})
            self.assertEqual(len(m.e.rows("alert")),2)

    def test_persistence_and_original_deadline_survive_restart(self):
        with tempfile.TemporaryDirectory() as directory:
            first = Evidence(directory)
            first.put("request_sequence",8)
            first.add("request",{"success":True},unique="same")
            second = Evidence(directory)
            self.assertEqual(second.get("request_sequence"),8)
            self.assertFalse(second.add("request",{"success":True},unique="same"))
            config = self.config()
            self.assertEqual(Monitor(config,directory).end,Monitor(config,directory).end)

    def test_nginx_rotation_preserves_unread_old_lines_and_filters_other_traffic(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory)/"access.log"
            log.write_text("")
            m = Monitor(self.config(role="primary",nginx_access_log=str(log)),Path(directory)/"evidence")
            m.nginx()
            line = json.dumps({"uri":"/internal/sites/sync","status":200,"request_id":"a","authorization":"secret"})+"\n"
            log.write_text(line+json.dumps({"uri":"/v1/responses","status":200})+"\n")
            log.rename(log.with_suffix(".log.1"))
            log.write_text(line.replace('"a"','"b"'))
            m.nginx()
            m.nginx()
            events = m.e.rows("primary_access")
            self.assertEqual(len(events),2)
            self.assertFalse(m.e.rows("coverage_gap"))
            self.assertTrue(all("authorization" not in row for _,row in events))

    @contextlib.contextmanager
    def server(self, behavior):
        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self,*args):
                pass
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(200)
                self.send_header("Content-Type","text/event-stream")
                self.end_headers()
                behavior(self)
        server = http.server.ThreadingHTTPServer(("127.0.0.1",0),Handler)
        thread = threading.Thread(target=server.serve_forever,daemon=True)
        thread.start()
        try:
            yield "http://127.0.0.1:"+str(server.server_port)
        finally:
            server.shutdown()
            server.server_close()

    def test_200_stream_failure_is_failure(self):
        def fail(handler):
            handler.wfile.write(b'data: {"type":"response.failed","response":{"status":"failed","error":{"code":"upstream_error"}}}\n\n')
        with self.server(fail) as url:
            result=model_request(self.config(secondary_url=url),"sk-dummy",1,timeout=2)
            self.assertEqual(result["http_status"],200)
            self.assertEqual(result["terminal"],"response.failed")
            self.assertFalse(result["success"])

    def test_hard_deadline_ends_stream_without_newline(self):
        def blackhole(handler):
            handler.wfile.write(b"data: ")
            handler.wfile.flush()
            time.sleep(2)
        with self.server(blackhole) as url:
            start=time.monotonic()
            result=model_request(self.config(secondary_url=url),"sk-dummy",1,timeout=.25)
            self.assertLess(time.monotonic()-start,1.5)
            self.assertFalse(result["success"])

    def test_only_authorized_model_and_loopback(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(AssertionError):
                Monitor(self.config(model="gpt-any"),directory)
            with self.assertRaises(AssertionError):
                model_request(self.config(secondary_url="https://example.invalid"),"sk-dummy",1)

    def test_healthy_endpoint_does_not_hide_17_minute_sync_gap(self):
        with tempfile.TemporaryDirectory() as directory:
            m = Monitor(self.config(),directory)
            m.e.put("last_sync",{"at":time.time()-1025,"duration_ms":100})
            response=mock.MagicMock()
            response.__enter__.return_value.status=200
            with mock.patch("urllib.request.build_opener") as opener:
                opener.return_value.open.return_value=response
                m.health()
                self.assertTrue(m.e.get("alert:no_successful_sync_1020s"))
                self.assertFalse(m.e.get("alert:not_ready",False))
                self.assertFalse(m.e.get("alert:no_successful_sync_1200s",False))
                m.e.put("last_sync",{"at":time.time(),"duration_ms":100})
                m.health()
                self.assertFalse(m.e.get("alert:no_successful_sync_1020s"))

    def test_billing_import_missing_and_cost_mismatch(self):
        import hashlib
        row={"request_id":"client:example","model":MODEL,"input_tokens":10,"output_tokens":20,"actual_cost":.25}
        imported={**row,"request_id":"site:"+hashlib.sha256(b"office\0client:example").hexdigest()[:56]}
        self.assertTrue(compare_billing([imported],[row],"office")["consistent"])
        self.assertFalse(compare_billing([],[row],"office")["consistent"])
        self.assertEqual(compare_billing([{**imported,"actual_cost":.3}],[row],"office")["mismatches"][0]["fields"],["actual_cost"])

    def test_never_synced_is_not_reported_as_zero_gap(self):
        with tempfile.TemporaryDirectory() as directory:
            m=Monitor(self.config(start_at=utc(time.time()-1100)),directory)
            m.e.put("sync_observation_started",time.time()-1100)
            value=m.write_status()
            self.assertGreater(value["max_observed_sync_gap_s"],1020)
            self.assertIsNone(value["estimated_lease_remaining_s"])

    def test_budget_exhaustion_is_distinct_from_network_failure(self):
        self.assertEqual(failure_category({'success':False,'terminal':'response.incomplete','incomplete_reason':'length'}),'output_budget_exhausted')
        self.assertEqual(failure_category({'success':False,'terminal':'response.incomplete','errors':[{'error':"{'reason': 'length'}"}]}),'output_budget_exhausted')
        self.assertEqual(failure_category({'success':False,'errors':[{'exception':'TimeoutError'}]}),'transport_or_parse_error')

    def test_finished_window_does_not_grow_a_false_gap_on_later_boot(self):
        with tempfile.TemporaryDirectory() as directory:
            end=time.time()-86400
            m=Monitor(self.config(start_at=utc(end-3600),end_at=utc(end)),directory)
            m.e.put('last_sync',{'at':m.observation_end-5,'duration_ms':100})
            status=m.write_status(final=True)
            self.assertAlmostEqual(status['sync_age_s'],5,places=4)
            before=len(m.e.rows('monitor_finished'))
            m.run()
            self.assertEqual(len(m.e.rows('monitor_finished')),before)

    def test_different_census_times_do_not_invent_missing_bills(self):
        row={'request_id':'client:new','model':MODEL,'input_tokens':10,'output_tokens':20,'actual_cost':.2,'created_at':utc(2000)}
        result=compare_billing([],[row],'office',cutoff=1990)
        self.assertTrue(result['consistent'])
        self.assertEqual(result['excluded_recent_secondary_rows'],1)


if __name__ == "__main__":
    unittest.main()
