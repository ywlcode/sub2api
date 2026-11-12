"""Prepare an isolated two-site Docker lab; never reads production credentials."""
import argparse
import json
import secrets
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("directory", type=Path)
parser.add_argument("--project", default="sub2api-site-lab")
parser.add_argument("--image", default="sub2api-site-lab:workspace")
parser.add_argument("--base-image", default="sub2api-ywlcode:4e0339570")
args = parser.parse_args()
root = args.directory.resolve()
root.mkdir(parents=True, exist_ok=True)
for name in ("config", "evidence", "build"):
    (root / name).mkdir(exist_ok=True)
source = Path(__file__).resolve().parent
credentials = root / "credentials.json"
if credentials.exists():
    creds = json.loads(credentials.read_text())
else:
    creds = {k: secrets.token_hex(32) for k in ("token", "jwt", "db", "admin")}
    creds["key"] = "sk-site-lab-" + secrets.token_hex(24)
    credentials.write_text(json.dumps(creds))
    credentials.chmod(0o600)
prices = {name: {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000002, "litellm_provider": provider, "mode": "chat"}
          for name, provider in (("deepseek-chat", "deepseek"), ("gpt-5.1", "openai"), ("gpt-test", "openai"))}
(root / "config/prices.json").write_text(json.dumps(prices))
services = {}
volumes = {}
for name in ("primary", "secondary"):
    site = {"mode": name, "id": "lab-" + name}
    if name == "primary":
        site["peers"] = [{"id": "lab-secondary", "token": creds["token"]}]
    else:
        site.update(primary_url="http://link:8091", token=creds["token"], allow_http=True)
    config = {"site": site, "runtime": {"role": "full"},
              "security": {"url_allowlist": {"enabled": False, "allow_private_hosts": True, "allow_insecure_http": True}},
              "pricing": {"remote_url": "", "hash_url": "", "fallback_file": "/lab/prices.json", "data_dir": "/app/data"},
              "server": {"host": "0.0.0.0", "port": 8080, "mode": "release"},
              "default": {"rate_multiplier": 1}}
    (root / f"config/{name}.json").write_text(json.dumps(config, indent=2))
    for suffix in ("db", "redis", "data"):
        volumes[f"{name}-{suffix}"] = {}
    services[name + "-db"] = {"image": "postgres:18.1-alpine3.23", "environment": {"POSTGRES_USER": "sub2api", "POSTGRES_PASSWORD": creds["db"], "POSTGRES_DB": "sub2api"},
                              "volumes": [f"{name}-db:/var/lib/postgresql"],
                              "healthcheck": {"test": ["CMD-SHELL", "pg_isready -U sub2api"], "interval": "2s", "timeout": "2s", "retries": 30}}
    services[name + "-redis"] = {"image": "redis:8.4-alpine", "command": ["redis-server", "--appendonly", "yes"], "volumes": [f"{name}-redis:/data"],
                                 "healthcheck": {"test": ["CMD", "redis-cli", "ping"], "interval": "2s", "timeout": "2s", "retries": 30}}
    env = {"AUTO_SETUP": "true", "CONFIG_FILE": f"/lab/{name}.json", "TZ": "Asia/Shanghai", "JWT_SECRET": creds["jwt"],
           "HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "http_proxy": "", "https_proxy": "", "all_proxy": "", "NO_PROXY": "*", "no_proxy": "*",
           "DATABASE_HOST": name + "-db", "DATABASE_USER": "sub2api", "DATABASE_PASSWORD": creds["db"], "DATABASE_DBNAME": "sub2api", "DATABASE_SSLMODE": "disable",
           "REDIS_HOST": name + "-redis", "ADMIN_EMAIL": name + "@site-lab.invalid", "ADMIN_PASSWORD": creds["admin"]}
    services[name] = {"image": args.image, "environment": env, "ports": ["127.0.0.1::8080"],
                      "volumes": [f"{name}-data:/app/data", f"{root}/config:/lab:ro"],
                      "depends_on": {name + "-db": {"condition": "service_healthy"}, name + "-redis": {"condition": "service_healthy"}},
                      "stop_grace_period": "30s"}
for name, script, port in (("upstream", "upstream.py", 8090), ("link", "link.py", 8091)):
    services[name] = {"image": "sub2api-oauth-lab-tun:0.1", "entrypoint": ["python3", "/lab/" + script],
                      "volumes": [f"{source}:/lab:ro", f"{root}/evidence:/evidence"], "ports": [f"127.0.0.1::{port}"]}
services["link"]["cap_add"] = ["NET_ADMIN"]
compose = {"name": args.project, "services": services, "volumes": volumes, "networks": {"default": {"internal": True}}}
(root / "compose.json").write_text(json.dumps(compose, indent=2))
(root / "compose.json").chmod(0o600)
(root / "build/Dockerfile").write_text(f"FROM {args.base_image}\nCOPY --chmod=0755 sub2api /app/sub2api\nLABEL purpose=sub2api-site-lab\n")
print(json.dumps({"directory": str(root), "compose": str(root / "compose.json")}))
