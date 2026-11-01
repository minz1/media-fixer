import json
import os
import sys
import urllib.request

from playwright.sync_api import TimeoutError as PlaywrightTimeout
from playwright.sync_api import sync_playwright

from shots import BASE, PAGES, SCHEMES, SEED, VIEWPORTS

SONARR = "http://127.0.0.1:18989"
SHOTS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "state", "shots")
os.makedirs(SHOTS, exist_ok=True)

EXPECT = {
    "incident": ["Link Click S01E02 will not play"],
    "monitoring": ["Samurai Champloo"],
    "maintenance": ["Pack sweep", "Stuck queue", "Incident timers"],
}

POLL_JS = """
(url) => {
  const el = document.getElementById("stuck-results");
  el.setAttribute("hx-get", url);
  el.setAttribute("hx-trigger", "every 1s");
  el.setAttribute("hx-swap", "outerHTML");
  htmx.process(el);
}
"""

CONTRAST_JS = r"""
() => {
  const cv = document.createElement("canvas");
  cv.width = cv.height = 1;
  const cx = cv.getContext("2d", { willReadFrequently: true });
  const rgba = (css) => {
    cx.clearRect(0, 0, 1, 1);
    cx.fillStyle = "#000";
    cx.fillStyle = css;
    cx.fillRect(0, 0, 1, 1);
    const d = cx.getImageData(0, 0, 1, 1).data;
    return [d[0], d[1], d[2], d[3] / 255];
  };
  const lum = ([r, g, b]) => {
    const f = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4; };
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
  };
  const probe = document.createElement("div");
  probe.style.backgroundColor = "Canvas";
  document.body.appendChild(probe);
  const canvasBg = rgba(getComputedStyle(probe).backgroundColor);
  probe.remove();
  const bgOf = (el) => {
    for (let e = el; e; e = e.parentElement) {
      const c = rgba(getComputedStyle(e).backgroundColor);
      if (c[3] > 0.5) return c;
    }
    return canvasBg;
  };
  const name = (el) => el.tagName.toLowerCase() + (el.id ? "#" + el.id : "") +
    (el.className && typeof el.className === "string" ? "." + el.className.trim().split(/\s+/).join(".") : "");
  const bad = [];
  for (const el of document.body.querySelectorAll("*")) {
    if (["SCRIPT", "STYLE", "OPTION"].includes(el.tagName) || el.closest(":disabled, [hidden]")) continue;
    const text = [...el.childNodes].filter((n) => n.nodeType === 3).map((n) => n.textContent).join("").trim();
    if (!text) continue;
    const r = el.getBoundingClientRect();
    const st = getComputedStyle(el);
    if (!r.width || !r.height || st.visibility === "hidden" || st.display === "none") continue;
    if (el.closest("details:not([open]) > :not(summary)")) continue;
    const fg = rgba(st.color);
    const l1 = lum(fg), l2 = lum(bgOf(el));
    const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
    const px = parseFloat(st.fontSize), bold = parseInt(st.fontWeight, 10) >= 700;
    const need = px >= 24 || (bold && px >= 18.66) ? 3 : 4.5;
    if (ratio < need) bad.push(`${name(el)} ${JSON.stringify(text.slice(0, 40))} ${ratio.toFixed(2)}`);
  }
  return bad;
}
"""


def layout(pg, where):
    out = []
    sw, iw = pg.evaluate("[document.documentElement.scrollWidth, window.innerWidth]")
    if sw > iw:
        out.append(f"{where}: scrolls sideways, scrollWidth {sw} > {iw}")
    bad = pg.evaluate(CONTRAST_JS)
    if bad:
        out.append(f"{where}: {len(bad)} low-contrast texts:\n  " + "\n  ".join(bad))
    return out


def check(cond, msg):
    if not cond:
        sys.exit(f"FAIL: {msg}")


def writes():
    with urllib.request.urlopen(SONARR + "/_writes", timeout=5) as r:
        return json.load(r)


def import_and_grab(br):
    for scheme in SCHEMES:
        for tag, width in VIEWPORTS:
            pg = br.new_page(color_scheme=scheme, viewport={"width": width, "height": 900})
            pg.on("dialog", lambda d: d.accept())
            pg.goto(BASE + "/monitoring/3", wait_until="load")
            pg.locator("form[hx-post$='/import/propose'] button").first.click()
            pg.wait_for_selector("form[hx-post$='/import'] input[name=row]", timeout=10000)
            check("Nothing is imported until you confirm" in pg.inner_text("body"), "proposal lacks the confirm line")
            check(pg.locator("input[name=row]:checked").count() == 2, "the two clean mapped files are not the ticked ones")
            check(pg.locator("input[name=row]:not(:checked)").count() == 1, "the Sample row is ticked or missing")
            for msg in layout(pg, f"proposal {scheme} {tag}"):
                check(False, msg)
            pg.screenshot(path=f"{SHOTS}/proposal-{scheme}-{tag}.png", full_page=True)
            if (scheme, tag) == ("light", "desk"):
                pg.click("button:has-text('Import selected files')")
                pg.wait_for_selector("#notice p.notice-ok")
                check("Import started" in pg.inner_text("#notice"), "import gave no 'Import started'")
                imports = [w for w in writes() if w.get("name") == "ManualImport"]
                check(len(imports) == 1, f"{len(imports)} ManualImport commands, want 1")
                check(len(imports[0]["files"]) == 2, "ManualImport did not carry the two ticked files")
            pg.locator("form[hx-post$='/releases'] button").first.click()
            pg.wait_for_selector("#monitor-show table button:has-text('Grab'), #monitor-show .empty, #monitor-show .notice-err", timeout=15000)
            check(pg.locator("button:has-text('Grab')").count() > 0, "Find releases listed nothing to grab")
            for msg in layout(pg, f"releases {scheme} {tag}"):
                check(False, msg)
            pg.screenshot(path=f"{SHOTS}/releases-{scheme}-{tag}.png", full_page=True)
            if (scheme, tag) == ("light", "desk"):
                pg.locator("button:has-text('Grab')").first.click()
                pg.wait_for_selector("#notice p.notice-ok")
                check("Grabbed." in pg.inner_text("#notice"), "grab gave no 'Grabbed.'")
                posts = [w for w in writes() if w.get("kind") == "release"]
                check(len(posts) == 1, f"{len(posts)} release POSTs, want 1")
            pg.close()


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
    pg.route("**/stuck-queue/results", lambda r: r.abort())
    pg.evaluate(POLL_JS, BASE + "/maintenance/stuck-queue/results")
    pg.wait_for_selector("#notice p.notice-err")
    pg.unroute("**/stuck-queue/results")
    try:
        pg.wait_for_selector("#notice p", state="detached", timeout=5000)
    except PlaywrightTimeout:
        pass
    check(pg.locator("#notice p").count() == 0, "a poll's own error notice stays after the poll recovers")

    pg.goto(BASE + "/maintenance", wait_until="load")
    sent = []
    pg.on("request", lambda r: sent.append(r.url) if "incident-timers" in r.url else None)
    pg.fill("#timer-stall_minutes", "99")
    pg.click("[data-reset=stall_minutes]")
    check(pg.input_value("#timer-stall_minutes") == "60", "timers Reset did not restore the default")
    check(not sent, f"timers Reset sent a request: {sent}")
    pg.fill("#timer-stall_minutes", "61")
    held = []
    pg.route("**/incident-timers/settings", lambda r: held.append(r))
    save = pg.locator("#incident-timers button[type=submit]")
    save.click()
    pg.wait_for_timeout(300)
    check(save.is_disabled(), "timers Save not disabled while saving")
    check(not pg.locator("#incident-timers [data-reset=stall_minutes]").is_disabled(), "timers Reset disabled by Save")
    check(len(held) == 1, "timers Save request was not held")
    held[0].continue_()
    pg.wait_for_selector("#notice p.notice-ok")
    pg.unroute("**/incident-timers/settings")
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

    import_and_grab(br)

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


def live_steps(br):
    pg = br.new_page(viewport={"width": 390, "height": 900})
    pg.goto(BASE + PAGES["transcript-investigating"], wait_until="load")
    steps = pg.locator("#incident-transcript [data-step]")
    before = steps.count()
    check(before >= 3, f"investigating transcript has {before} steps")
    check(pg.locator(".tl-live").count() == 1, "investigating transcript lacks the working indicator")
    check(pg.locator(".tl-new").count() == 0, "steps highlighted on first load")
    pg.evaluate(
        """() => {
          const old = document.getElementById("incident-transcript");
          const next = old.cloneNode(true);
          const last = next.querySelector("[data-step='2']");
          const add = last.cloneNode(true);
          add.dataset.step = "3";
          last.after(add);
          old.replaceWith(next);
        }"""
    )
    pg.wait_for_timeout(200)
    check(steps.count() == before + 1, "swap did not add a step")
    check(pg.locator(".tl-new").count() == 1, "new step not highlighted after swap")
    pg.close()


def main():
    with sync_playwright() as pw:
        br = pw.chromium.launch()
        problems = []
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
                    problems += layout(pg, where)
                    if name == "incidents":
                        rows = pg.locator("a[href*='/incidents/']").count()
                        check(rows == 5, f"{where}: {rows} incident rows, want 5")
                pg.close()
        check(not problems, "layout:\n" + "\n".join(problems))
        interactions(br)
        live_steps(br)
        br.close()
    print("statuses:", sorted(SEED["statuses"].items()))
    print("walk ok")


if __name__ == "__main__":
    main()
