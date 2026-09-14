# scripts/

Agent helper scripts for the jqlm skill.

- `har_capture.py` — capture a website's network traffic as a HAR 1.2 file via
  Chrome DevTools Protocol. Requires an isolated Chrome instance on an Xvfb
  display (see the x11-gui-automation skill) with `--remote-debugging-port`.

  Usage:
  ```
  har_capture.py <cdp_port> <url> <seconds> <out.har>
  ```
  Creates its own tab, records all requests/responses for `seconds`, embeds
  text response bodies (<=100KB each) as `.response.content.text`, writes HAR
  1.2 JSON. Requires `pip install websockets`.

Contribution rules (from the global agent instructions):
- scripts live here, one per tool, with a usage comment header
- link new scripts from `SKILL.md` so they're discoverable
