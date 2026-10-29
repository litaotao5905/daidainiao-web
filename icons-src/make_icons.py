#!/usr/bin/env python3
"""Regenerates every icon in ../web from icon.svg (master) and icon-small.svg
(simplified, used for the 16/32 px favicon sizes). Needs Playwright + Chrome
(for rasterising) and Pillow:  python3 icons-src/make_icons.py"""
import io, pathlib, re, shutil, struct, sys
from PIL import Image
from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).resolve().parent
WEB = HERE.parent / "web"
CHROME = "/usr/bin/google-chrome"

master = (HERE / "icon.svg").read_text()
small = (HERE / "icon-small.svg").read_text()

# full-bleed variants (maskable / apple-touch): square background, glyph shrunk into the safe zone
m = re.search(r'(.*?<defs>.*?</defs>\n)(.*?)(  <g filter=.*)</svg>', master, re.S)
head, _bg, glyph = m.groups()

def bleed(scale):
    return (head
            + '  <rect width="512" height="512" fill="url(#bg)"/>\n'
            + '  <rect width="512" height="512" fill="url(#shine)"/>\n'
            + f'  <g transform="translate(256 256) scale({scale}) translate(-258 -285)">\n'
            + glyph + '  </g>\n</svg>\n')

(HERE / "icon-maskable.svg").write_text(bleed(0.76))
(HERE / "icon-apple.svg").write_text(bleed(0.86))

def raster(pw, svg_text, size, transparent=True):
    svg = svg_text.replace('width="512" height="512" role', f'width="{size}" height="{size}" role', 1)
    pg = pw.new_page(viewport={"width": size, "height": size}, device_scale_factor=1)
    pg.set_content(f"<html><body style='margin:0;background:transparent'>{svg}</body></html>")
    png = pg.screenshot(omit_background=transparent, clip={"x": 0, "y": 0, "width": size, "height": size})
    pg.close()
    return png

def write_ico(path, frames):
    """frames: [(size, png_bytes)] -> PNG-compressed .ico"""
    out = struct.pack("<HHH", 0, 1, len(frames))
    offset = 6 + 16 * len(frames)
    data = b""
    for size, png in frames:
        out += struct.pack("<BBBBHHII", size % 256, size % 256, 0, 0, 1, 32, len(png), offset + len(data))
        data += png
    path.write_bytes(out + data)

with sync_playwright() as p:
    b = p.chromium.launch(executable_path=CHROME, args=["--no-sandbox"])
    shutil.copyfile(HERE / "icon.svg", WEB / "favicon.svg")
    (WEB / "icon-192.png").write_bytes(raster(b, master, 192))
    (WEB / "icon-512.png").write_bytes(raster(b, master, 512))
    (WEB / "icon-maskable-512.png").write_bytes(raster(b, (HERE / "icon-maskable.svg").read_text(), 512, transparent=False))
    (WEB / "apple-touch-icon.png").write_bytes(raster(b, (HERE / "icon-apple.svg").read_text(), 180, transparent=False))
    write_ico(WEB / "favicon.ico", [(16, raster(b, small, 16)), (32, raster(b, small, 32)), (48, raster(b, master, 48))])
    b.close()
print("icons written to", WEB)
