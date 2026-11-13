"""Local-only helpers shared by the reproducible site fault tests."""
import gzip
import json
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path


class Lab:
    def __init__(self, directory):
        self.root = Path(directory).resolve()
        self.compose = ["docker", "compose", "-f", str(self.root / "compose.json")]
        self.creds = json.loads((self.root / "credentials.json").read_text())
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.urls = {}

    def command(self, *args, input=None, check=True):
        return subprocess.run(self.compose + list(args), input=input, text=True, capture_output=True, check=check)

    def url(self, service):
        if service in self.urls:
            return self.urls[service]
        cid = self.command("ps", "-q", service).stdout.strip()
        info = json.loads(subprocess.check_output(["docker", "inspect", cid]))[0]
        ip = next(iter(info["NetworkSettings"]["Networks"].values()))["IPAddress"]
        port = {"upstream": 8090, "link": 8091}.get(service, 8080)
        self.urls[service] = f"http://{ip}:{port}"
        return self.urls[service]

    def sql(self, site, query):
        return self.command("exec", "-T", site + "-db", "psql", "-U", "sub2api", "-d", "sub2api", "-XAt", "-v", "ON_ERROR_STOP=1", input=query).stdout.strip()

    def query(self, site, query):
        return json.loads(self.sql(site, "SELECT coalesce(json_agg(t),'[]'::json) FROM (" + query + ") t;"))

    def http(self, site, path, body=None, headers=None, timeout=35, compress=False):
        data = json.dumps(body).encode() if body is not None else None
        hdr = {"Content-Type": "application/json", **(headers or {})}
        if compress:
            data = gzip.compress(data)
            hdr["Content-Encoding"] = "gzip"
        req = urllib.request.Request(self.url(site) + path, data=data, headers=hdr)
        started = time.monotonic()
        try:
            resp = self.opener.open(req, timeout=timeout)
        except urllib.error.HTTPError as error:
            resp = error
        with resp:
            raw = resp.read(1)
            first_byte = time.monotonic() - started
            raw += resp.read()
            if resp.headers.get("Content-Encoding") == "gzip":
                raw = gzip.decompress(raw)
            return {"status": resp.status, "body": raw.decode(), "seconds": round(time.monotonic() - started, 4), "first_byte_seconds": round(first_byte, 4), "headers": dict(resp.headers)}

    def request(self, protocol="responses", model="deepseek-chat", stream=False, text="hello", site="secondary", key=None, extra=None):
        body = {"model": model, "stream": stream}
        if protocol == "responses":
            body.update(input=text, max_output_tokens=200)
        else:
            body.update(messages=[{"role": "user", "content": text}], max_tokens=200)
        body.update(extra or {})
        return self.http(site, "/v1/" + protocol, body, {"Authorization": "Bearer " + (key or self.creds["key"])})

    def control(self, **settings):
        script = "import json,urllib.request; r=urllib.request.Request('http://127.0.0.1:8091/control',data=" + repr(json.dumps(settings).encode()) + ",headers={'Content-Type':'application/json'}); print(urllib.request.urlopen(r).read().decode())"
        self.command("exec", "-T", "link", "python3", "-c", script)

    def records(self):
        script = "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8091/control').read().decode())"
        return json.loads(self.command("exec", "-T", "link", "python3", "-c", script).stdout)["records"]

    def netem(self, *args):
        return self.command("exec", "-T", "link", "tc", "qdisc", "replace", "dev", "eth0", "root", "netem", *args).stdout

    def clear_netem(self):
        self.command("exec", "-T", "link", "tc", "qdisc", "del", "dev", "eth0", "root", check=False)

    def eventually(self, condition, timeout=30, label="condition"):
        end = time.monotonic() + timeout
        last = None
        while time.monotonic() < end:
            try:
                last = condition()
                if last:
                    return last
            except (OSError, subprocess.CalledProcessError, urllib.error.URLError):
                pass
            time.sleep(0.3)
        raise AssertionError(f"timeout waiting for {label}: {last}")

    def ready(self):
        return self.http("secondary", "/readyz")["status"] == 200

    def settled(self):
        return int(self.sql("secondary", "SELECT count(*) FROM site_outbox;")) == 0


def seed(lab):
    key = lab.creds["key"]
    domestic = {"api_key": "domestic-secret", "base_url": "http://upstream:8090/domestic", "deepseek_volcengine_compatibility": True,
                "api_protocol": "responses", "model_mapping": {"deepseek-chat": "deepseek-chat"},
                "api_base_urls": {p: "http://upstream:8090/domestic" for p in ("responses", "chat_completions", "anthropic")}}
    foreign = {"api_key": "foreign-secret", "base_url": "http://upstream:8090/foreign", "api_protocol": "responses", "model_mapping": {"gpt-test": "gpt-5.1", "gpt-5.1": "gpt-5.1"}}
    assert not lab.sql("primary", "SELECT id FROM users WHERE id=101;"), "seed only on a fresh lab"
    lab.sql("primary", f"""
    BEGIN;
    INSERT INTO users(id,email,password_hash,balance,concurrency) VALUES(101,'client@site-lab.invalid','disabled',100,200);
    INSERT INTO groups(id,name,platform,subscription_type) VALUES(101,'lab-composite','composite','standard');
    INSERT INTO accounts(id,name,platform,type,credentials,concurrency) VALUES
      (101,'domestic','deepseek','apikey','{json.dumps(domestic)}',200),
      (102,'foreign','openai','apikey','{json.dumps(foreign)}',200);
    UPDATE accounts SET extra='{{"site_secondary_enabled":true}}' WHERE id=101;
    INSERT INTO account_groups(account_id,group_id) VALUES(101,101),(102,101);
    INSERT INTO api_keys(id,user_id,key,name,group_id,quota) VALUES(101,101,'{key}','lab-client',101,100);
    INSERT INTO composite_model_routes(group_id,public_model,target_platform,upstream_model) VALUES
      (101,'deepseek-chat','deepseek','deepseek-chat'),(101,'gpt-test','openai','gpt-5.1');
    COMMIT;
    """)
    lab.command("restart", "primary")
    lab.eventually(lambda: lab.sql("secondary", "SELECT count(*) FROM accounts WHERE id=101 AND deleted_at IS NULL;") == "1", timeout=40, label="seeded account synchronized")
    # The empty bootstrap lease may predate seeding. Start the actual scenarios
    # only after a fresh process has installed the seeded policy and router.
    lab.command("restart", "secondary")
    lab.eventually(lab.ready, timeout=40, label="first seeded replica")


if __name__ == "__main__":
    import sys
    lab = Lab(sys.argv[1])
    if len(sys.argv) > 2 and sys.argv[2] == "seed":
        seed(lab)
    print(json.dumps({name: lab.url(name) for name in ("primary", "secondary", "link", "upstream")}))
