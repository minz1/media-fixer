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


def interactions(br):
    pg = br.new_page(viewport={"width": 1280, "height": 900})
    pg.on("dialog", lambda d: d.accept())
    pg.goto(BASE + "/maintenance", wait_until="load")
    held = []
    pg.route("**/stuck-queue/run", lambda r: held.append(r))
    btn = pg.locator("form[hx-post$='/stuck-queue/run'] button")
    btn.click()
    pg.wait_for_timeout(300)
    check(btn.is_disabled(), "stuck-queue Run button not disabled while running")
    check("htmx-request" in (pg.get_attribute("#progress", "class") or ""), "#progress lacks htmx-request")
    check(len(held) == 1, "request was not held")
    held[0].continue_()
    pg.wait_for_selector("#notice p.notice-ok")
    pg.unroute("**/stuck-queue/run")

    pg.route("**/stuck-queue/run", lambda r: r.abort())
    pg.locator("form[hx-post$='/stuck-queue/run'] button").click()
    pg.wait_for_selector("#notice p.notice-err")
    check("could not reach" in pg.inner_text("#notice"), "aborted request shows no 'could not reach'")
    pg.unroute("**/stuck-queue/run")

    pg.goto(BASE + "/maintenance", wait_until="load")
    sent = []
    pg.on("request", lambda r: sent.append(r.url) if "incident-timers" in r.url else None)
    pg.fill("#timer-stall_minutes", "99")
    pg.click("[data-reset=stall_minutes]")
    check(pg.input_value("#timer-stall_minutes") == "60", "timers Reset did not restore the default")
    check(not sent, f"timers Reset sent a request: {sent}")
    pg.locator("#incident-timers button[type=submit]").click()
    pg.wait_for_selector("#notice p.notice-ok")
    check("Saved" in pg.inner_text("#notice"), "timers Save notice does not say Saved")
    box = pg.locator("#notice").bounding_box()
    check(box and 0 <= box["y"] < 900, "timers Save notice outside the viewport")

    pg.click("#pause-toggle button")
    pg.wait_for_selector("#pause-toggle >> text=Resume")
    check(pg.url.endswith("/media/maintenance"), f"Pause navigated away: {pg.url}")
    pg.click("#pause-toggle button")
    pg.wait_for_selector("#pause-toggle >> text=Pause agent")
    check(pg.url.endswith("/media/maintenance"), f"Resume navigated away: {pg.url}")

    pg.goto(BASE + "/monitoring/3", wait_until="load")
    pg.route("**/explain", lambda r: r.fulfill(status=500, body="boom"))
    pg.locator("form[hx-post$='/explain'] button").click()
    pg.wait_for_selector("#notice p.notice-err")
    check("boom" in pg.inner_text("#notice"), "explain 500 text not shown in #notice")
    check(pg.locator("form[hx-post$='/explain']").count() == 1, "explain card was replaced by the error")
    pg.unroute("**/explain")

    pg.goto(BASE + "/", wait_until="load")
    pg.locator("a", has_text="NOVA S01E01").first.click()
    pg.wait_for_load_state("load")
    pg.locator("button", has_text="Mark resolved").first.click()
    pg.wait_for_selector("#notice p.notice-ok")
    check("Marked resolved." in pg.inner_text("#notice"), "Resolve gave no 'Marked resolved.'")
    check("?done=" not in pg.url, f"?done= left in the URL: {pg.url}")
    check(pg.locator("#notice").count() == 1, "more than one #notice after Resolve")
    check(pg.locator("nav").count() == 1, "more than one tabs nav after Resolve")
    pg.close()


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
        interactions(br)
        br.close()
    print("statuses:", sorted(SEED["statuses"].items()))
    print("walk ok")


if __name__ == "__main__":
    main()
