#!/usr/bin/env python3
"""Renders ../icon-preview.png: 512/192/48/16 px on light and dark backgrounds
(+ maskable with circle/squircle masks and the apple-touch icon)."""
import base64, io, pathlib, struct
from PIL import Image
from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).resolve().parent
WEB = HERE.parent / "web"

def b64(path):
    return "data:image/png;base64," + base64.b64encode(pathlib.Path(path).read_bytes()).decode()

def ico_frame(size):
    d = (WEB / "favicon.ico").read_bytes()
    n = struct.unpack_from("<H", d, 4)[0]
    for i in range(n):
        w, _, _, _, _, _, ln, off = struct.unpack_from("<BBBBHHII", d, 6 + 16 * i)
        if (w or 256) == size:
            return "data:image/png;base64," + base64.b64encode(d[off:off + ln]).decode()
    raise SystemExit("no frame %d" % size)

def panel(cls, title):
    return f"""
<section class="{cls}">
  <h2>{title}</h2>
  <div class="big"><img src="{b64(WEB/'icon-512.png')}" width="512" height="512"><span>512</span></div>
  <div class="row">
    <figure><img src="{b64(WEB/'icon-192.png')}" width="192" height="192"><figcaption>192</figcaption></figure>
    <figure><img src="{b64(WEB/'apple-touch-icon.png')}" width="180" height="180" style="border-radius:40px"><figcaption>apple-touch 180</figcaption></figure>
  </div>
  <div class="row">
    <figure><img src="{ico_frame(48)}" width="48" height="48"><figcaption>48</figcaption></figure>
    <figure><img src="{ico_frame(32)}" width="32" height="32"><figcaption>32</figcaption></figure>
    <figure><img src="{ico_frame(16)}" width="16" height="16"><figcaption>16</figcaption></figure>
    <figure><img src="{ico_frame(16)}" width="32" height="32" style="image-rendering:pixelated"><figcaption>16 ×2</figcaption></figure>
    <figure><img src="{b64(WEB/'icon-maskable-512.png')}" width="96" height="96" style="border-radius:50%"><figcaption>maskable 圆形</figcaption></figure>
    <figure><img src="{b64(WEB/'icon-maskable-512.png')}" width="96" height="96" style="border-radius:26px"><figcaption>maskable 圆角</figcaption></figure>
  </div>
  <div class="tab"><img src="{ico_frame(16)}" width="16" height="16"><span>飞书话题</span></div>
</section>"""

html = f"""<!doctype html><meta charset=utf-8><style>
body {{ margin:0; display:flex; font:13px -apple-system,'Segoe UI','PingFang SC','Noto Sans CJK SC',sans-serif; }}
section {{ width:620px; padding:24px 28px 28px; box-sizing:border-box; }}
.light {{ background:#f4f5f7; color:#1f2329; }} .dark {{ background:#17181b; color:#dfe1e6; }}
h2 {{ margin:0 0 14px; font-size:15px; }}
.big {{ position:relative; width:512px; }} .big span {{ position:absolute; right:-6px; top:0; transform:translateX(100%); opacity:.6 }}
.row {{ display:flex; gap:22px; align-items:flex-end; margin-top:20px; }}
figure {{ margin:0; display:flex; flex-direction:column; align-items:center; gap:6px; }} figcaption {{ opacity:.65; font-size:12px; }}
.tab {{ margin-top:22px; display:inline-flex; align-items:center; gap:8px; padding:7px 14px; border-radius:9px 9px 0 0; }}
.light .tab {{ background:#fff; }} .dark .tab {{ background:#2a2c31; }}
</style>{panel('light','浅色背景')}{panel('dark','深色背景')}"""
out = HERE.parent / "icon-preview.png"
with sync_playwright() as p:
    b = p.chromium.launch(executable_path="/usr/bin/google-chrome", args=["--no-sandbox"])
    pg = b.new_page(viewport={"width": 1240, "height": 960}, device_scale_factor=1)
    pg.set_content(html)
    pg.wait_for_timeout(300)
    pg.screenshot(path=str(out), full_page=True)
    b.close()
print(out)
