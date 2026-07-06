import asyncio
import json
import logging
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from aiohttp import web as aioweb

from ha import HA
from reconciler import Reconciler
from web import make_app

logging.basicConfig(level=logging.INFO,
                    format="%(asctime)s %(levelname)s %(name)s: %(message)s")
log = logging.getLogger("ha-git-ops")


async def main():
    opts = json.loads(Path("/data/options.json").read_text())
    ha = HA()
    rec = Reconciler(opts, ha)
    rec.ensure_keys()

    runner = aioweb.AppRunner(make_app(rec))
    await runner.setup()
    await aioweb.TCPSite(runner, "0.0.0.0", 8099).start()
    log.info("ingress UI listening on :8099")

    while True:
        try:
            await rec.tick()
        except Exception as e:
            log.exception("reconcile tick failed")
            rec.status["error"] = str(e)
        await asyncio.sleep(opts.get("poll_interval", 120))


if __name__ == "__main__":
    asyncio.run(main())
