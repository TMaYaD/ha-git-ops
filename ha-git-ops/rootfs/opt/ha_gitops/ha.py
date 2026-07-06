"""Supervisor / Core API client. Runs with the add-on's SUPERVISOR_TOKEN."""

import logging
import os

import aiohttp

log = logging.getLogger("ha")
SUPERVISOR = "http://supervisor"


class HA:
    def __init__(self):
        self.headers = {
            "Authorization": f"Bearer {os.environ['SUPERVISOR_TOKEN']}"}

    async def _req(self, method, path, json=None):
        async with aiohttp.ClientSession(headers=self.headers) as s:
            async with s.request(method, f"{SUPERVISOR}{path}",
                                 json=json) as r:
                body = await r.json(content_type=None)
                return r.status, body

    async def core_check(self):
        status, body = await self._req("POST", "/core/check")
        ok = status == 200 and body.get("result") == "ok"
        if not ok:
            log.error("core config check failed: %s", body)
        return ok

    async def core_restart(self):
        await self._req("POST", "/core/restart")

    async def call_service(self, domain, service, data=None):
        status, body = await self._req(
            "POST", f"/core/api/services/{domain}/{service}", json=data or {})
        if status >= 400:
            log.error("service %s.%s failed: %s", domain, service, body)

    async def set_state(self, entity_id, state, attributes):
        await self._req("POST", f"/core/api/states/{entity_id}",
                        json={"state": state, "attributes": attributes})

    async def notify(self, title, message):
        await self.call_service(
            "persistent_notification", "create",
            {"title": title, "message": message,
             "notification_id": f"ha_gitops_{abs(hash(title))}"})
