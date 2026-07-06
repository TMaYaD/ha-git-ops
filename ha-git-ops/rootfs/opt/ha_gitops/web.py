"""Ingress UI: status, per-file diffs, promote / revert / sync actions.

Served under HA ingress — all URLs are relative so the ingress path
prefix is transparent. Secrets diffs are masked to key names only.
"""

import difflib
import html
import logging

from aiohttp import web

from reconciler import SECRETS_PLAIN

log = logging.getLogger("web")

PAGE = """<!doctype html><html><head><meta charset="utf-8">
<title>HA GitOps</title><style>
 body {{ font-family: system-ui, sans-serif; margin: 1.5rem; max-width: 60rem; }}
 .ok {{ color: #1a7f37; }} .bad {{ color: #cf222e; }} .warn {{ color: #9a6700; }}
 pre {{ background: #f6f8fa; padding: .8rem; overflow-x: auto; font-size: .85rem; }}
 pre .add {{ color: #1a7f37; }} pre .del {{ color: #cf222e; }}
 .file {{ border: 1px solid #d0d7de; border-radius: 6px; margin: 1rem 0; }}
 .file > header {{ padding: .5rem .8rem; background: #f6f8fa;
   display: flex; justify-content: space-between; align-items: center; }}
 button {{ cursor: pointer; }} input[type=text] {{ width: 24rem; }}
</style></head><body>
<h2>HA GitOps</h2>
<p>{status}</p>
<form method="post" action="./sync" style="display:inline">
  <button>Sync now</button></form>
{restart}
{conflicts}
{files}
{promote_all}
</body></html>"""


def _diff_html(rel, want, live):
    if rel == SECRETS_PLAIN:
        return "<pre>(secrets diff masked — values changed)</pre>"
    want_l = (want or b"").decode(errors="replace").splitlines()
    live_l = (live or b"").decode(errors="replace").splitlines()
    lines = []
    for l in difflib.unified_diff(want_l, live_l, "git (desired)",
                                  "live", lineterm=""):
        cls = "add" if l.startswith("+") else "del" if l.startswith("-") else ""
        lines.append(f'<span class="{cls}">{html.escape(l)}</span>')
    return "<pre>" + "\n".join(lines) + "</pre>"


def make_app(rec):
    async def index(_):
        st = rec.status
        drift, conflicts = st["drift"], st["conflicts"]
        if st.get("error"):
            status = f'<span class="bad">✗ {html.escape(st["error"])}</span>'
        elif conflicts:
            status = f'<span class="bad">⚠ {len(conflicts)} conflict(s)</span>'
        elif drift:
            status = f'<span class="warn">● {len(drift)} file(s) drifted</span>'
        else:
            status = '<span class="ok">✓ in sync</span>'
        status += (f' — applied <code>{(rec.state["applied_sha"] or "")[:9]}'
                   f'</code>, last sync {st["last_sync"] or "never"}')

        restart = ""
        if st["restart_required"]:
            restart = ('<p class="warn">Core restart required to activate '
                       'applied changes.</p>')

        conflicts_html = ""
        if conflicts:
            items = "".join(f"<li><code>{html.escape(r)}</code> — "
                            f"{html.escape(why)}</li>"
                            for r, why in sorted(conflicts.items()))
            conflicts_html = (f"<h3>Conflicts (resolve by revert or promote, "
                              f"then sync)</h3><ul>{items}</ul>")

        blocks = []
        for rel in sorted(drift):
            want = rec._at_ref(rec.state["applied_sha"], rel)
            live = rec._live(rel)
            blocks.append(f"""<div class="file"><header>
  <code>{html.escape(rel)}</code> <span>{html.escape(drift[rel])}</span>
  <span>
   <form method="post" action="./revert" style="display:inline">
    <input type="hidden" name="rel" value="{html.escape(rel)}">
    <button>Revert to git</button></form>
   <form method="post" action="./promote" style="display:inline">
    <input type="hidden" name="rel" value="{html.escape(rel)}">
    <input type="text" name="message" placeholder="commit message">
    <button>Promote</button></form>
  </span></header>
  {_diff_html(rel, want, live)}</div>""")

        promote_all = ""
        if len(drift) > 1:
            promote_all = f"""<form method="post" action="./promote">
  {"".join(f'<input type="hidden" name="rel" value="{html.escape(r)}">'
           for r in sorted(drift))}
  <input type="text" name="message" placeholder="commit message">
  <button>Promote all {len(drift)} files</button></form>"""

        return web.Response(
            text=PAGE.format(status=status, restart=restart,
                             conflicts=conflicts_html,
                             files="".join(blocks), promote_all=promote_all),
            content_type="text/html")

    async def sync(_):
        await rec.tick()
        raise web.HTTPFound("./")

    async def revert(request):
        form = await request.post()
        await rec.revert(form["rel"])
        raise web.HTTPFound("./")

    async def promote(request):
        form = await request.post()
        rels = form.getall("rel")
        try:
            await rec.promote(rels, form.get("message", ""))
        except RuntimeError as e:
            return web.Response(text=str(e), status=409)
        raise web.HTTPFound("./")

    app = web.Application()
    app.router.add_get("/", index)
    app.router.add_post("/sync", sync)
    app.router.add_post("/revert", revert)
    app.router.add_post("/promote", promote)
    return app
