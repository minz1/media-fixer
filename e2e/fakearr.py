import glob, gzip, json, re, sys
from urllib.parse import urlparse, parse_qs
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port = int(sys.argv[1]); kind = sys.argv[2]
RELEASES = []
for f in sorted(glob.glob(sys.argv[3] + "/rel-*.json.gz")):
    with gzip.open(f) as fh:
        RELEASES += json.load(fh)
SERIES = [
    {"id": 1, "title": "American Horror Story", "path": "/tv/AHS", "monitored": True, "seriesType": "standard", "added": "2024-01-01T00:00:00Z"},
    {"id": 2, "title": "NOVA", "path": "/tv/NOVA", "monitored": True, "seriesType": "standard", "added": "2024-01-01T00:00:00Z"},
    {"id": 3, "title": "Link Click", "path": "/tv/LC", "monitored": True, "seriesType": "anime", "added": "2025-06-01T00:00:00Z"},
    {"id": 4, "title": "Samurai Champloo", "path": "/tv/SC", "monitored": True, "seriesType": "anime", "added": "2025-06-01T00:00:00Z"},
    {"id": 5, "title": "Knot", "path": "/tv/Knot", "monitored": False, "seriesType": "standard", "added": "2026-10-01T00:00:00Z"},
]
def eps(sid):
    out = []
    for s in (1, 2):
        for e in range(1, 9):
            out.append({"id": sid*1000+s*100+e, "seriesId": sid, "title": f"Episode {e}", "seasonNumber": s,
                        "episodeNumber": e, "episodeFileId": 0, "hasFile": (e + sid + s) % 3 != 0, "monitored": True,
                        "airDateUtc": "2025-0%d-%02dT00:00:00Z" % (s, e), "absoluteEpisodeNumber": (s-1)*8+e})
    return out
def rec(i, dl, title, sid, ep, has, msg, state="importPending"):
    return {"id": i, "downloadId": dl, "title": title, "status": "completed", "trackedDownloadState": state,
            "seriesId": sid, "episodeId": sid*1000+100+ep, "seasonNumber": 1, "episodeHasFile": has,
            "episode": {"episodeNumber": ep}, "statusMessages": [{"title": title, "messages": [msg]}]}
records = [] if kind == "radarr" else [
    rec(1, "AHS0103", "American.Horror.Story.S01E03.1080p", 1, 3, True, "Not an upgrade for existing episode file(s)"),
    rec(2, "NOVA0101", "NOVA.S01E01.1080p", 2, 1, False, "Unable to determine if file is a sample"),
    rec(3, "LC0102", "Link.Click.S01E02.1080p", 3, 2, False, "Episode has a TBA title and recently aired"),
]
HIST = [{"id": 9, "seriesId": 3, "episodeId": 3102, "eventType": "downloadFailed", "date": "2026-10-30T10:00:00Z",
         "sourceTitle": "Link.Click.S01E02.1080p", "data": {"indexer": "Nyaa", "message": "Download failed: no seeders"}}]
Q = {"quality": {"name": "WEBDL-1080p"}, "revision": {"version": 1}}
L = [{"id": 1, "name": "English"}]
def cand(name, size, eps, rej=()):
    return {"path": "/dl/LC0102/" + name, "size": size, "downloadId": "LC0102", "releaseGroup": "grp", "releaseType": "singleEpisode",
            "seriesId": 3, "series": {"id": 3}, "seasonNumber": 1, "indexerFlags": 0, "quality": Q, "languages": L,
            "episodes": [{"id": e, "seasonNumber": 1, "episodeNumber": e % 100, "hasFile": False} for e in eps],
            "rejections": [{"reason": r} for r in rej]}
CANDS = [
    cand("Link.Click.S01E02.mkv", 1500000000, [3102]),
    cand("Link.Click.S01E05.mkv", 1400000000, [3105]),
    cand("extras.mkv", 90000000, []),
    cand("Sample/sample.mkv", 20000000, [3108], ["Sample"]),
]
WRITES = []
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): sys.stderr.write("%s %s\n" % (self.command, self.path))
    def send(self, obj, code=200):
        b = json.dumps(obj).encode(); self.send_response(code)
        self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        u = urlparse(self.path); q = parse_qs(u.query); p = u.path
        if p == "/api/v3/queue":
            return self.send({"page": 1, "pageSize": 200, "totalRecords": len(records), "records": records})
        if p == "/api/v3/queue/details": return self.send(records)
        if p == "/api/v3/series": return self.send(SERIES if kind == "sonarr" else [])
        m = re.match(r"/api/v3/series/(\d+)$", p)
        if m:
            s = [x for x in SERIES if x["id"] == int(m.group(1))]
            return self.send(s[0]) if s else self.send({"message": "NotFound"}, 404)
        if p == "/api/v3/episode": return self.send(eps(int(q.get("seriesId", ["1"])[0])))
        if p in ("/api/v3/history/series", "/api/v3/history/since"):
            sid = int(q.get("seriesId", ["0"])[0] or 0)
            return self.send([h for h in HIST if not sid or h["seriesId"] == sid])
        if p == "/api/v3/release": return self.send(RELEASES if kind == "sonarr" else [])
        if p == "/api/v3/movie": return self.send([])
        if p == "/_writes": return self.send(WRITES)
        if p == "/api/v3/manualimport":
            return self.send(CANDS if q.get("downloadId") == ["LC0102"] else [])
        if p in ("/api/v3/command", "/api/v3/episodefile", "/api/v3/moviefile"): return self.send([])
        if p == "/api/v3/config/downloadclient": return self.send({"autoRedownloadFailed": True})
        return self.send({})
    def do_DELETE(self):
        m = re.match(r"/api/v3/queue/(\d+)", self.path)
        if m: records[:] = [r for r in records if r["id"] != int(m.group(1))]
        return self.send({})
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try: body = json.loads(raw or b"{}")
        except ValueError: body = {}
        if self.path.startswith("/api/v3/command"): WRITES.append(body)
        if self.path.startswith("/api/v3/release"): WRITES.append({"kind": "release", **body})
        return self.send({"id": 1, "status": "completed"})
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
