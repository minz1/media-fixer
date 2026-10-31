Browser harness for the dashboard: fake Sonarr/Radarr, a seeded database, a real server and Playwright.

    nix develop .#e2e -c e2e/run.sh walk
    nix develop .#e2e -c e2e/run.sh shots e2e/shots/before

`walk` asserts every page loads clean in light/dark and desktop/phone; `shots` writes one PNG per page, scheme and width.
State (db, config, logs) lives in `e2e/state/`, screenshots in `e2e/shots/`; both are git-ignored. The config uses dummy keys only.
