#!/bin/sh
set -eu

cd "$(dirname "$0")/.."

diff -q cli/spot sdk/spot >/dev/null || {
    echo "sdk/spot has drifted from cli/spot — copy cli/spot over it" >&2
    exit 1
}
sh -n cli/spot sdk/spot

python3 - <<'PY'
import functools
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading
import time

root = pathlib.Path.cwd()
sdk = root / "sdk"

class Handler(http.server.SimpleHTTPRequestHandler):
    deploys = 0

    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(sdk), **kwargs)

    def do_POST(self):
        if self.path != "/api/deploy":
            self.send_error(404)
            return
        length = int(self.headers.get("content-length", "0"))
        self.rfile.read(length)
        Handler.deploys += 1
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

handler = functools.partial(Handler)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
base = f"http://127.0.0.1:{server.server_port}"
env = {**os.environ, "SPOT_URL": base}

def run(args, extra_env=None, **kwargs):
    proc_env = env if extra_env is None else {**env, **extra_env}
    return subprocess.run(args, cwd=root, env=proc_env, check=True, text=True, capture_output=True, **kwargs)

def wait_for_deploys(want):
    for _ in range(40):
        if Handler.deploys >= want:
            return
        time.sleep(0.1)
    raise AssertionError(f"deploy count = {Handler.deploys}, want at least {want}")

try:
    howto = run(["./cli/spot", "agent-howto"]).stdout
    assert "# Spot — agent how-to" in howto, howto[:200]
    schema = run(["./cli/spot", "show-schema"]).stdout
    assert "# Spot Show schema" in schema, schema[:200]
    with tempfile.TemporaryDirectory() as tmp:
        show = pathlib.Path(tmp) / "show.json"
        out = pathlib.Path(tmp) / "out"
        init = run(["./cli/spot", "show", "init", str(show)]).stdout
        assert show.exists(), init
        built = run(["./cli/spot", "show", "build", str(show), str(out)]).stdout
        assert (out / "index.html").exists(), built
        assert (out / "show.json").exists(), built
        meta = (out / "_spot.json").read_text()
        assert '"title":"My Spot Show"' in meta, meta
        compact = pathlib.Path(tmp) / "compact.json"
        compact.write_text('{"title":"Auth \\"happy path\\"","description":"Compact JSON","cards":[]}')
        compact_out = pathlib.Path(tmp) / "compact-out"
        run(["./cli/spot", "show", "build", str(compact), str(compact_out)])
        compact_meta = json.loads((compact_out / "_spot.json").read_text())
        assert compact_meta["title"] == 'Auth "happy path"', compact_meta
        assert compact_meta["description"] == "Compact JSON", compact_meta
        nested = pathlib.Path(tmp) / "nested.json"
        nested.write_text('{"description":"Top-level description","cards":[{"title":"Nested title"}]}')
        nested_out = pathlib.Path(tmp) / "nested-out"
        run(["./cli/spot", "show", "build", str(nested), str(nested_out)])
        nested_meta = json.loads((nested_out / "_spot.json").read_text())
        assert nested_meta["title"] == "Spot Show", nested_meta
        assert nested_meta["description"] == "Top-level description", nested_meta
        run(["./cli/spot", "show", "deploy", "demo", str(show)], extra_env={"TMPDIR": tmp})
        leaked = list(pathlib.Path(tmp).glob("spot-show.*"))
        assert not leaked, leaked
        zero = subprocess.run(
            ["./cli/spot", "show", "watch", "--interval", "0", "demo", str(show)],
            cwd=root,
            env=env,
            text=True,
            capture_output=True,
        )
        assert zero.returncode != 0, zero.stdout
        assert "greater than zero" in zero.stderr, zero.stderr
        invalid_site = subprocess.run(
            ["./cli/spot", "show", "watch", "Bad_Name", str(show)],
            cwd=root,
            env=env,
            text=True,
            capture_output=True,
        )
        assert invalid_site.returncode != 0, invalid_site.stdout
        assert "site name 'Bad_Name'" in invalid_site.stderr, invalid_site.stderr
        watch_show = pathlib.Path(tmp) / "watch.json"
        watch_show.write_text("{")
        open_log = pathlib.Path(tmp) / "open.log"
        open_bin = pathlib.Path(tmp) / "open-bin"
        open_bin.mkdir()
        opener = open_bin / "xdg-open"
        opener.write_text('#!/bin/sh\necho "$1" >> "$SPOT_OPEN_LOG"\n')
        opener.chmod(0o755)
        start_deploys = Handler.deploys
        watch = subprocess.Popen(
            ["./cli/spot", "show", "watch", "--open", "--interval", "1", "watch", str(watch_show)],
            cwd=root,
            env={
                **env,
                "TMPDIR": tmp,
                "PATH": str(open_bin) + os.pathsep + os.environ["PATH"],
                "SPOT_OPEN_LOG": str(open_log),
            },
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        try:
            time.sleep(1.4)
            assert watch.poll() is None, watch.communicate(timeout=1)
            assert Handler.deploys == start_deploys, Handler.deploys
            valid_watch = pathlib.Path(tmp) / "watch-valid.json"
            valid_watch.write_text('{"title":"Watch one","cards":[]}')
            valid_watch.replace(watch_show)
            wait_for_deploys(start_deploys + 1)
            invalid_watch = pathlib.Path(tmp) / "watch-invalid.json"
            invalid_watch.write_text("{")
            invalid_watch.replace(watch_show)
            time.sleep(1.4)
            assert watch.poll() is None, watch.communicate(timeout=1)
            watch_show.unlink()
            time.sleep(1.4)
            assert watch.poll() is None, watch.communicate(timeout=1)
            valid_watch = pathlib.Path(tmp) / "watch-valid-again.json"
            valid_watch.write_text('{"title":"Watch two","cards":[]}')
            valid_watch.replace(watch_show)
            wait_for_deploys(start_deploys + 2)
            assert watch.poll() is None, watch.communicate(timeout=1)
        finally:
            if watch.poll() is None:
                watch.terminate()
            try:
                out, err = watch.communicate(timeout=3)
            except subprocess.TimeoutExpired:
                watch.kill()
                out, err = watch.communicate(timeout=3)
        assert "could not parse" in err, (out, err)
        assert open_log.read_text().count("\n") == 1, open_log.read_text()
finally:
    server.shutdown()
PY
