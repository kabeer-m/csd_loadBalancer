# Patch: add `/health` to the messaging app backend

The load balancer's health checker does `GET <backend>/health` every second
and expects a 2xx response. Your messaging app's `server.py` (from the
`Chat-Application-` repo) doesn't have that route yet, so the LB will mark
it **UNHEALTHY** and refuse to route traffic to it until you add one.

This is a 4-line change to `backend/server.py`. No new dependencies needed.

## Before

```python
def create_app():
    app = web.Application()
    conn = database.get_connection()
    database.init_db(conn)
    app["db_conn"] = conn

    encryption_key = crypto_utils.load_or_create_encryption_key()
    app["fernet"] = Fernet(encryption_key)

    app.router.add_get("/", index)
    app.router.add_get("/ws", websocket_handler)
    app.router.add_static("/assets/", FRONTEND_DIR / "dist" / "assets")

    return app
```

## After

```python
async def health(request):
    return web.Response(text="ok")


def create_app():
    app = web.Application()
    conn = database.get_connection()
    database.init_db(conn)
    app["db_conn"] = conn

    encryption_key = crypto_utils.load_or_create_encryption_key()
    app["fernet"] = Fernet(encryption_key)

    app.router.add_get("/", index)
    app.router.add_get("/health", health)
    app.router.add_get("/ws", websocket_handler)
    app.router.add_static("/assets/", FRONTEND_DIR / "dist" / "assets")

    return app
```

Only two lines actually changed: the new `health()` function, and the
`app.router.add_get("/health", health)` line.

## Apply it

On each of Sys2, Sys3, Sys4 (after cloning/pulling the messaging app repo):

```bash
cd Chat-Application-/backend
```

Open `server.py` in an editor (nano, vim, VS Code — whatever's available)
and make the change above, or apply it in one shot with:

```bash
python3 - << 'EOF'
import re
path = "server.py"
src = open(path).read()

health_fn = '''async def health(request):
    return web.Response(text="ok")


'''
src = src.replace("def create_app():", health_fn + "def create_app():", 1)
src = src.replace(
    'app.router.add_get("/", index)',
    'app.router.add_get("/", index)\n    app.router.add_get("/health", health)',
    1,
)
open(path, "w").write(src)
print("patched", path)
EOF
```

Then verify:

```bash
python3 server.py &
sleep 1
curl http://localhost:3210/health
# should print: ok
```

That's it — the messaging app now satisfies the load balancer's health
check contract while everything else (WebSocket chat, encryption, DB)
is untouched.
