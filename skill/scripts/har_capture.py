#!/usr/bin/env python3
# har_capture.py — CDP network capture -> HAR 1.2.
# Usage: har_capture.py <cdp_port> <url> <seconds> <out.json>
# Creates its own tab, captures all network traffic for `seconds`,
# includes response bodies (text, <=100KB each), writes HAR JSON.

import asyncio, base64, json, sys, time, urllib.request
import websockets

def http_json(path):
    req = urllib.request.Request(f"http://localhost:{PORT}{path}", method="PUT")
    return json.load(urllib.request.urlopen(req))

PORT = sys.argv[1]
URL = sys.argv[2]
SECONDS = float(sys.argv[3])
OUT = sys.argv[4]

def ts(wall):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(wall)) + f".{int((wall%1)*1000):03d}Z"

async def main():
    tab = http_json(f"/json/new?{URL}")
    ws_url = tab["webSocketDebuggerUrl"]
    requests = {}
    pending_bodies = {}

    async with websockets.connect(ws_url, max_size=2**30) as ws:
        mid = 0
        async def call(method, params=None):
            nonlocal mid
            mid += 1
            await ws.send(json.dumps({"id": mid, "method": method, "params": params or {}}))

        await call("Network.enable", {"maxPostDataSize": 1_000_000})
        await call("Page.navigate", {"url": URL})

        deadline = time.time() + SECONDS
        while time.time() < deadline:
            try:
                raw = await asyncio.wait_for(ws.recv(), timeout=max(0.05, deadline - time.time()))
            except asyncio.TimeoutError:
                break
            m = json.loads(raw)
            method, p = m.get("method"), m.get("params", {})
            if method == "Network.requestWillBeSent":
                req = p["request"]
                requests[p["requestId"]] = {
                    "_seq": p.get("timestamp", 0),
                    "startedDateTime": ts(p.get("wallTime", time.time())),
                    "time": 0,
                    "request": {
                        "method": req["method"], "url": req["url"],
                        "httpVersion": "HTTP/1.1", "headersSize": -1,
                        "bodySize": len(req.get("postData") or ""),
                        "headers": [{"name": k, "value": v} for k, v in req["headers"].items()],
                        "queryString": [
                            {"name": kv.split("=", 1)[0],
                             "value": kv.split("=", 1)[1] if "=" in kv else ""}
                            for kv in (req["url"].split("?", 1)[1].split("&") if "?" in req["url"] else []) if kv],
                        "cookies": [],
                        "postData": ({"mimeType": req["headers"].get("Content-Type", "text/plain"),
                                      "text": req["postData"]} if req.get("postData") else None),
                    },
                    "response": None, "cache": {},
                    "timings": {"blocked": -1, "dns": -1, "connect": -1, "ssl": -1,
                                "send": 0, "wait": -1, "receive": 0},
                    "_resourceType": p.get("type", ""),
                }
            elif method == "Network.responseReceived":
                rid = p["requestId"]
                if rid not in requests:
                    continue
                resp = p["response"]
                mime = resp.get("mimeType", "")
                fetchable = ("text/" in mime or "json" in mime or
                             "javascript" in mime or "xml" in mime or mime == "")
                requests[rid]["response"] = {
                    "url": resp["url"], "status": resp["status"],
                    "statusText": resp.get("statusText", ""),
                    "httpVersion": resp.get("protocol", ""),
                    "headers": [{"name": k, "value": v} for k, v in resp["headers"].items()],
                    "cookies": [],
                    "content": {"size": 0, "mimeType": mime, "compression": 0, "text": None},
                    "redirectURL": "", "headersSize": -1, "bodySize": 0,
                    "_remoteIPAddress": resp.get("remoteIPAddress", ""),
                    "_fetchable": fetchable,
                }
            elif method == "Network.loadingFinished":
                rid = p["requestId"]
                e = requests.get(rid)
                size = p.get("encodedDataLength", 0)
                if e:
                    if e["response"]:
                        e["response"]["bodySize"] = size
                        e["response"]["content"]["size"] = size
                    if e["response"] and e["response"].get("_fetchable"):
                        mid += 1
                        pending_bodies[mid] = rid
                        await ws.send(json.dumps({"id": mid, "method": "Network.getResponseBody",
                                                  "params": {"requestId": rid}}))
            elif method == "Network.loadingFailed":
                rid = p["requestId"]
                e = requests.get(rid)
                if e and not e["response"]:
                    e["response"] = {"url": e["request"]["url"], "status": 0,
                                     "statusText": p.get("errorText", "failed"),
                                     "httpVersion": "", "headers": [], "cookies": [],
                                     "content": {"size": 0, "mimeType": "", "text": None},
                                     "redirectURL": "", "headersSize": -1, "bodySize": 0}
            elif "id" in m and m["id"] in pending_bodies and "result" in m:
                rid = pending_bodies.pop(m["id"])
                e = requests.get(rid)
                if not e or not e.get("response"):
                    continue
                body = m["result"].get("body", "")
                if m["result"].get("base64Encoded"):
                    try:
                        body = base64.b64decode(body).decode("utf-8", "replace")
                    except Exception:
                        body = f"<binary, {len(body)} b64 chars>"
                if len(body) <= 100_000:
                    e["response"]["content"]["text"] = body

        # stop page activity cleanly
        try:
            await call("Page.stopLoading")
        except Exception:
            pass

    entries = []
    for e in requests.values():
        e.pop("_seq", None)
        resp = e.get("response")
        if resp is None:
            e["response"] = {"url": e["request"]["url"], "status": 0, "statusText": "no response",
                             "httpVersion": "", "headers": [], "cookies": [],
                             "content": {"size": 0, "mimeType": "", "text": None},
                             "redirectURL": "", "headersSize": -1, "bodySize": 0}
        else:
            resp.pop("_fetchable", None)
        e["timings"]["wait"] = 0
        entries.append(e)
    entries.sort(key=lambda e: e["startedDateTime"])
    har = {"log": {"version": "1.2",
                   "creator": {"name": "jqlm-har-capture", "version": "1.0"},
                   "pages": [{"startedDateTime": ts(time.time()), "id": "page_1",
                              "title": URL, "pageTimings": {"onContentLoad": -1, "onLoad": -1}}],
                   "entries": entries}}
    with open(OUT, "w") as f:
        json.dump(har, f, indent=1)
    n_bodies = sum(1 for e in entries if e["response"]["content"]["text"])
    print(f"wrote {OUT}: {len(entries)} entries, {n_bodies} with response bodies")

asyncio.run(main())
