import sys, pathlib
from playwright.sync_api import sync_playwright
def render(svg_path, out, size, bg=None):
    svg = pathlib.Path(svg_path).read_text()
    html = f"<html><body style='margin:0;background:{bg or 'transparent'}'><div style='width:{size}px;height:{size}px'>{svg.replace('width=\"512\" height=\"512\" role', f'width=\"{size}\" height=\"{size}\" role',1)}</div></body></html>"
    with sync_playwright() as p:
        b = p.chromium.launch(executable_path='/usr/bin/google-chrome', args=['--no-sandbox'])
        pg = b.new_page(viewport={'width': size, 'height': size}, device_scale_factor=1)
        pg.set_content(html)
        pg.screenshot(path=out, omit_background=(bg is None), clip={'x':0,'y':0,'width':size,'height':size})
        b.close()
if __name__ == '__main__':
    render(sys.argv[1], sys.argv[2], int(sys.argv[3]))
