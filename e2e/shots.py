import json
import os
import sys

from playwright.sync_api import sync_playwright

BASE = "http://127.0.0.1:18080/media"
HERE = os.path.dirname(os.path.abspath(__file__))
SEED = json.load(open(os.path.join(HERE, "state", "seed.json")))
IDS = SEED["ids"]

PAGES = {
    "incidents": "/",
    "incident": f"/incidents/{IDS['incident']}",
    "incident-blocked": f"/incidents/{IDS['blocked']}",
    "transcript": f"/incidents/{IDS['incident']}/transcript",
    "selftest": "/selftest",
    "maintenance": "/maintenance",
    "monitoring": "/babysit",
    "monitoring-show": "/babysit/3",
}
SCHEMES = ("light", "dark")
VIEWPORTS = (("desk", 1280), ("phone", 390))


def main(out):
    os.makedirs(out, exist_ok=True)
    with sync_playwright() as pw:
        br = pw.chromium.launch()
        for scheme in SCHEMES:
            for tag, width in VIEWPORTS:
                pg = br.new_page(color_scheme=scheme, viewport={"width": width, "height": 900})
                for name, path in PAGES.items():
                    pg.goto(BASE + path, wait_until="load")
                    pg.wait_for_timeout(800)
                    pg.screenshot(path=f"{out}/{name}-{scheme}-{tag}.png", full_page=True)
                pg.close()
        br.close()


if __name__ == "__main__":
    main(sys.argv[1])
