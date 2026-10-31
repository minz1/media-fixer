import sys

from playwright.sync_api import sync_playwright

from shots import BASE, PAGES, SCHEMES, SEED, VIEWPORTS

EXPECT = {
    "incident": ["Link Click S01E02 will not play"],
    "monitoring": ["Samurai Champloo"],
    "maintenance": ["Pack sweep", "Stuck queue", "Incident timers"],
}


def check(cond, msg):
    if not cond:
        sys.exit(f"FAIL: {msg}")


def main():
    with sync_playwright() as pw:
        br = pw.chromium.launch()
        for scheme in SCHEMES:
            for tag, width in VIEWPORTS:
                pg = br.new_page(color_scheme=scheme, viewport={"width": width, "height": 900})
                errors = []
                pg.on("pageerror", lambda e: errors.append(str(e)))
                pg.on("console", lambda m: m.type == "error" and errors.append(m.text))
                for name, path in PAGES.items():
                    where = f"{name} {scheme} {tag}"
                    resp = pg.goto(BASE + path, wait_until="load")
                    pg.wait_for_timeout(800)
                    check(resp.status == 200, f"{where}: HTTP {resp.status}")
                    check(not errors, f"{where}: console errors {errors}")
                    text = pg.inner_text("body")
                    for want in EXPECT.get(name, []):
                        check(want in text, f"{where}: missing {want!r}")
                    if name == "incidents":
                        rows = pg.locator("a[href*='/incidents/']").count()
                        check(rows == 4, f"{where}: {rows} incident rows, want 4")
                pg.close()
        br.close()
    print("statuses:", sorted(SEED["statuses"].items()))
    print("walk ok")


if __name__ == "__main__":
    main()
