# solustrum

my website

## Running locally

```sh
go run .
# http://localhost:8080
```

`PORT` and `HOST` control the listen address. `HOST` is empty (all interfaces) by
default, so container platforms work out of the box; set `HOST=127.0.0.1` when a
reverse proxy fronts the process.

The site in `sites/root` is embedded in the binary with `go:embed`, so the built
artifact needs no files beside it.

Endpoints: `/` (site), `/hello?name=`, `/healthz` (health check).

## Deployment (Lightsail)

`solustrum.com` -> Cloudflare Tunnel -> Apache :80 -> Go binary on 127.0.0.1:8080.

- Pushing to **dev** triggers the GitHub webhook at `/deploy/solustrum`.
- `solustrum-deploy.sh` fetches the branch, builds it, restarts `solustrum.service`,
  and rolls back if `/healthz` does not come up.
- The webhook only deploys allowlisted refs (`refs/heads/dev`); `stable` still has
  the old static layout and is intentionally ignored.

Server layout:

| Path | Purpose |
| --- | --- |
| `/srv/solustrum` | git checkout of the deployed branch |
| `/srv/bin/solustrum` | running binary (`solustrum.service`) |
| `/usr/local/bin/solustrum-deploy.sh` | build + restart + health check |
| `/usr/local/bin/solustrum-webhook.py` | GitHub webhook receiver (`solustrum-webhook.service`) |
| `/etc/httpd/conf.d/solustrum.conf` | Apache: webhook + reverse proxy |
| `/var/www/html/solustrum` | previously served static site, now unused |
