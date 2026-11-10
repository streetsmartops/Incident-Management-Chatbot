#!/usr/bin/env bash
# Re-render docs/diagrams/*.mmd to SVG and PNG with mermaid-cli (ELK layout).
#   npm i -g @mermaid-js/mermaid-cli@11.12.0   (uses its bundled Chromium, or set PUPPETEER_EXECUTABLE_PATH)
set -euo pipefail
cd "$(dirname "$0")"
for f in *.mmd; do
  b="${f%.mmd}"
  mmdc -q -c mermaid-config.json -b white -i "$f" -o "$b.svg"
  mmdc -q -c mermaid-config.json -b white -s 2 -i "$f" -o "$b.png"
done
