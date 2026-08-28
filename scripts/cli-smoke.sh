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
    last_authorization = ""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(sdk), **kwargs)

    def do_POST(self):
        if self.path != "/api/deploy":
            self.send_error(404)
            return
        length = int(self.headers.get("content-length", "0"))
        self.rfile.read(length)
        Handler.deploys += 1
        Handler.last_authorization = self.headers.get("Authorization", "")
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
        assert "valid:" in run(["./cli/spot", "show", "validate", str(show)]).stdout
        runtime = (out / "index.html").read_text()
        for marker in ["__spotShow", "MutationObserver", "visual-dialog", "trace-list", "diff-split-pane", "cardIDs", "@highlightjs/cdn-assets@11.12.0", "mermaid@11.16.1"]:
            assert marker in runtime, marker
        assert "allow-same-origin" not in runtime
        meta = (out / "_spot.json").read_text()
        assert '"title":"My Spot Show"' in meta, meta
        assets = pathlib.Path(tmp) / "assets"
        assets.mkdir()
        image = assets / "result.png"
        image.write_bytes(b"png")
        asset_show = pathlib.Path(tmp) / "asset-show.json"
        asset_show.write_text(json.dumps({
            "title": "Assets and additions",
            "cards": [{
                "id": "evidence",
                "blocks": [
                    {"kind": "image", "src": "assets/result.png", "alt": "Result"},
                    {"kind": "code", "body": "package main", "language": "go", "line_start": 80},
                    {"kind": "diff", "body": "-old\n+new", "layout": "split"},
                    {"kind": "terminal", "body": "\\u001b[32mok\\u001b[0m", "cols": 100},
                    {"kind": "trace", "steps": [{"label": "Build", "status": "done"}]},
                ],
            }],
        }))
        asset_out = pathlib.Path(tmp) / "asset-out"
        run(["./cli/spot", "show", "build", str(asset_show), str(asset_out)])
        assert (asset_out / "assets" / "result.png").read_bytes() == b"png"
        duplicate = pathlib.Path(tmp) / "duplicate.json"
        duplicate.write_text('{"cards":[{"id":"same","blocks":[]},{"id":"same","blocks":[]}]}')
        duplicate_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(duplicate)], cwd=root, env=env, text=True, capture_output=True
        )
        assert duplicate_result.returncode != 0
        assert "duplicate card id" in duplicate_result.stderr
        invalid_kind = pathlib.Path(tmp) / "invalid-kind.json"
        invalid_kind.write_text('{"cards":[{"blocks":[{"kind":"video"}]}]}')
        kind_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(invalid_kind)], cwd=root, env=env, text=True, capture_output=True
        )
        assert kind_result.returncode != 0
        assert "unsupported block kind" in kind_result.stderr
        invalid_layout = pathlib.Path(tmp) / "invalid-layout.json"
        invalid_layout.write_text('{"cards":[{"blocks":[{"kind":"diff","layout":[]}]}]}')
        layout_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(invalid_layout)], cwd=root, env=env, text=True, capture_output=True
        )
        assert layout_result.returncode != 0
        assert "cards[0].blocks[0].layout" in layout_result.stderr
        assert "Traceback" not in layout_result.stderr
        invalid_status = pathlib.Path(tmp) / "invalid-status.json"
        invalid_status.write_text('{"cards":[{"blocks":[{"kind":"trace","steps":[{"label":"Run","status":{}}]}]}]}')
        status_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(invalid_status)], cwd=root, env=env, text=True, capture_output=True
        )
        assert status_result.returncode != 0
        assert "cards[0].blocks[0].steps[0].status" in status_result.stderr
        assert "Traceback" not in status_result.stderr
        future = pathlib.Path(tmp) / "future.json"
        future.write_text('{"cards":[],"future_option":true}')
        future_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(future)], cwd=root, env=env, text=True, capture_output=True
        )
        assert future_result.returncode == 0
        assert "warning:" in future_result.stderr
        preserved = pathlib.Path(tmp) / "preserved"
        preserved.mkdir()
        (preserved / "sentinel").write_text("keep")
        missing = pathlib.Path(tmp) / "missing.json"
        missing.write_text('{"cards":[{"blocks":[{"kind":"image","src":"missing.png"}]}]}')
        missing_result = subprocess.run(
            ["./cli/spot", "show", "build", str(missing), str(preserved)], cwd=root, env=env, text=True, capture_output=True
        )
        assert missing_result.returncode != 0
        assert "local file not found" in missing_result.stderr
        assert (preserved / "sentinel").read_text() == "keep"
        outside = pathlib.Path(tmp).parent / (pathlib.Path(tmp).name + "-outside.png")
        outside.write_bytes(b"outside")
        traversal = pathlib.Path(tmp) / "traversal.json"
        traversal.write_text(json.dumps({"cards": [{"blocks": [{"kind": "image", "src": "../" + outside.name}]}]}))
        traversal_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(traversal)], cwd=root, env=env, text=True, capture_output=True
        )
        assert traversal_result.returncode != 0
        assert "escapes the Show directory" in traversal_result.stderr
        linked = pathlib.Path(tmp) / "linked.png"
        linked.symlink_to(outside)
        symlink_show = pathlib.Path(tmp) / "symlink.json"
        symlink_show.write_text('{"cards":[{"blocks":[{"kind":"image","src":"linked.png"}]}]}')
        symlink_result = subprocess.run(
            ["./cli/spot", "show", "validate", str(symlink_show)], cwd=root, env=env, text=True, capture_output=True
        )
        assert symlink_result.returncode != 0
        assert "escapes the Show directory" in symlink_result.stderr
        outside.unlink()
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
        run(["./cli/spot", "show", "deploy", "--no-screenshot", "demo", str(show)], extra_env={"TMPDIR": tmp})
        leaked = list(pathlib.Path(tmp).glob("spot-show.*"))
        assert not leaked, leaked
        publishing_key = "test_publishing_key_redacted_fixture"
        curl_home = pathlib.Path(tmp) / "curl-home"
        curl_home.mkdir()
        curl_trace = pathlib.Path(tmp) / "curl-trace"
        (curl_home / ".curlrc").write_text(f'trace-ascii = "{curl_trace}"\n')
        keyed = run(
            ["./cli/spot", "deploy", "keydemo", str(out)],
            extra_env={"SPOT_PUBLISH_KEY": publishing_key, "CURL_HOME": str(curl_home)},
        )
        assert Handler.last_authorization == "Bearer " + publishing_key, Handler.last_authorization
        assert publishing_key not in keyed.stdout and publishing_key not in keyed.stderr
        assert not curl_trace.exists(), "secret-bearing curl loaded the default .curlrc"
        fake_bin = pathlib.Path(tmp) / "fake-bin"
        fake_bin.mkdir()
        chromium = fake_bin / "chromium"
        chromium.write_text("""#!/bin/sh
out=
for arg in "$@"; do
  case "$arg" in
    --screenshot=*) out=${arg#--screenshot=} ;;
  esac
done
[ -n "$out" ] || exit 2
printf '\\211\\120\\116\\107\\015\\012\\032\\012\\000\\000\\000\\015\\111\\110\\104\\122\\000\\000\\000\\001\\000\\000\\000\\001\\010\\004\\000\\000\\000\\265\\034\\014\\002\\000\\000\\000\\013\\111\\104\\101\\124\\170\\332\\143\\144\\370\\017\\000\\001\\005\\001\\001\\047\\030\\343\\146\\000\\000\\000\\000\\111\\105\\116\\104\\256\\102\\140\\202' > "$out"
""")
        chromium.chmod(0o755)
        start_deploys = Handler.deploys
        keyed_show = run(
            ["./cli/spot", "show", "deploy", "shotdemo", str(show)],
            extra_env={
                "TMPDIR": tmp,
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "SPOT_PUBLISH_KEY": publishing_key,
            },
        )
        assert Handler.deploys == start_deploys + 1, Handler.deploys
        assert "screenshot: skipped for publishing-key deploy" in keyed_show.stdout, keyed_show.stdout
        assert Handler.last_authorization == "Bearer " + publishing_key, Handler.last_authorization
        start_deploys = Handler.deploys
        run(
            ["./cli/spot", "show", "deploy", "--screenshot", "shotdemo-explicit", str(show)],
            extra_env={
                "TMPDIR": tmp,
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "SPOT_PUBLISH_KEY": publishing_key,
            },
        )
        assert Handler.deploys == start_deploys + 2, Handler.deploys
        assert Handler.last_authorization == "Bearer " + publishing_key, Handler.last_authorization

        chromium.write_text("""#!/bin/sh
out=
for arg in "$@"; do
  case "$arg" in
    --screenshot=*) out=${arg#--screenshot=} ;;
  esac
done
printf 'not-a-png' > "$out"
exit 7
""")
        failed_capture = subprocess.run(
            ["./cli/spot", "show", "deploy", "--screenshot", "shotdemo-failed", str(show)],
            cwd=root,
            env={
                **env,
                "TMPDIR": tmp,
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
            },
            text=True,
            capture_output=True,
        )
        assert failed_capture.returncode != 0, failed_capture.stdout
        assert "screenshot capture failed" in failed_capture.stderr, failed_capture.stderr

        chromium.write_text("""#!/bin/sh
out=
for arg in "$@"; do
  case "$arg" in
    --screenshot=*) out=${arg#--screenshot=} ;;
  esac
done
printf 'not-a-png' > "$out"
""")
        invalid_capture = subprocess.run(
            ["./cli/spot", "show", "deploy", "--screenshot", "shotdemo-invalid", str(show)],
            cwd=root,
            env={
                **env,
                "TMPDIR": tmp,
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
            },
            text=True,
            capture_output=True,
        )
        assert invalid_capture.returncode != 0, invalid_capture.stdout
        assert "invalid or incomplete PNG" in invalid_capture.stderr, invalid_capture.stderr

        chromium.write_text("""#!/bin/sh
out=
for arg in "$@"; do
  case "$arg" in
    --screenshot=*) out=${arg#--screenshot=} ;;
  esac
done
printf '\\211\\120\\116\\107\\015\\012\\032\\012\\000\\000\\000\\015\\111\\110\\104\\122\\000\\000\\000\\001\\000\\000\\000\\001\\010\\004\\000\\000\\000\\265\\034\\014\\002\\000\\000\\000\\013\\111\\104\\101\\124\\170\\332\\143\\144\\370\\017\\000\\001\\005\\001\\001\\047\\030\\343\\146\\000\\000\\000\\000\\111\\105\\116\\104\\256\\102\\140\\202' > "$out"
trap '' TERM
exec sleep 30
""")
        resistant_capture = subprocess.run(
            ["./cli/spot", "show", "deploy", "--screenshot", "shotdemo-resistant", str(show)],
            cwd=root,
            env={
                **env,
                "TMPDIR": tmp,
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "SPOT_SCREENSHOT_TIMEOUT": "6",
            },
            text=True,
            capture_output=True,
            timeout=8,
        )
        assert resistant_capture.returncode == 0, resistant_capture.stderr
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
            ["./cli/spot", "show", "watch", "--open", "--no-screenshot", "--interval", "1", "watch", str(watch_show)],
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
