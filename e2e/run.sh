#!/usr/bin/env bash
set -euo pipefail

mode=${1:?usage: run.sh walk | shots <outdir>}
here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
state=$here/state
cd "$root"

rm -rf "$state"
mkdir -p "$state"

pids=()
cleanup() {
	((${#pids[@]})) && kill "${pids[@]}" 2>/dev/null || true
}
trap cleanup EXIT

cat >"$state/config.toml" <<TOML
[server]
addr = "127.0.0.1:18080"
base_url = "/media"
[db]
path = "$state/mf.db"
[discord]
token = "dummy"
guild_id = "1"
owner_id = "1"
[llm]
base_url = "http://127.0.0.1:9/v1"
api_key = "dummy"
model = "dummy"
[sonarr]
url = "http://127.0.0.1:18989"
api_key = "dummy"
[radarr]
url = "http://127.0.0.1:18878"
api_key = "dummy"
TOML

go build -o "$state/media-fixer" .
go run ./e2e/seed -db "$state/mf.db" >"$state/seed.json"

fixtures=$root/internal/sweep/testdata
python3 -I "$here/fakearr.py" 18989 sonarr "$fixtures" 2>"$state/sonarr.log" &
pids+=($!)
python3 -I "$here/fakearr.py" 18878 radarr "$fixtures" 2>"$state/radarr.log" &
pids+=($!)
"$state/media-fixer" -config "$state/config.toml" >"$state/server.log" 2>&1 &
pids+=($!)

for _ in $(seq 100); do
	curl -fs -o /dev/null http://127.0.0.1:18080/media/ && break
	sleep 0.2
done
curl -fs -o /dev/null http://127.0.0.1:18080/media/ || { echo "server did not start" >&2; exit 1; }

case $mode in
walk) python3 "$here/walk.py" ;;
shots) python3 "$here/shots.py" "${2:?outdir}" ;;
*) echo "unknown mode $mode" >&2; exit 2 ;;
esac
