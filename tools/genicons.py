#!/usr/bin/env python3
"""Генерация иконок трея из исходного PNG: assets/*.png (кадры анимации и состояния) и assets/app.ico."""
import sys, numpy as np
from PIL import Image, ImageDraw

src = sys.argv[1]
SIZES = [16, 20, 24, 32, 40, 48]
FRAMES = 8                      # 8 кадров на пол-оборота (стрелки симметричны через 180°)
im = Image.open(src).convert('RGBA')
W = im.size[0]
arr = np.array(im).astype(np.float32)
x0, y0, x1, y1 = 190, 178, 760, 640            # внутренняя область экрана (без рамки)
BLUE = np.array([19, 97, 220], np.float32)
WHITE = np.array([250, 249, 248], np.float32)
reg = arr[y0:y1, x0:x1, 0]
alpha = np.clip((reg - BLUE[0]) / (WHITE[0] - BLUE[0]), 0, 1)
ys, xs = np.where(alpha > 0.5)
cx = x0 + (xs.min() + xs.max()) / 2.0
cy = y0 + (ys.min() + ys.max()) / 2.0

base = arr.copy()
base[y0:y1, x0:x1, 0:3] = BLUE
base[y0:y1, x0:x1, 3] = 255
full_alpha = np.zeros((W, W), np.float32)
full_alpha[y0:y1, x0:x1] = alpha
amask = Image.fromarray((full_alpha * 255).astype(np.uint8), 'L')

def compose(angle):
    m = amask.rotate(angle, resample=Image.BICUBIC, center=(cx, cy)) if angle else amask
    a = np.array(m).astype(np.float32)[..., None] / 255.0
    out = base.copy()
    out[..., 0:3] = base[..., 0:3] * (1 - a) + WHITE * a
    return Image.fromarray(np.clip(out, 0, 255).astype(np.uint8), 'RGBA')

def badge(img, color, kind):
    big = img.copy()
    d = ImageDraw.Draw(big)
    r = int(W * 0.21)
    cxb, cyb = int(W * 0.76), int(W * 0.76)
    d.ellipse([cxb - r - 14, cyb - r - 14, cxb + r + 14, cyb + r + 14], fill=(255, 255, 255, 255))
    d.ellipse([cxb - r, cyb - r, cxb + r, cyb + r], fill=color)
    if kind == 'pause':
        bw, bh = int(r * 0.28), int(r * 1.1)
        for dx in (-int(r * 0.42), int(r * 0.14)):
            d.rectangle([cxb + dx, cyb - bh // 2, cxb + dx + bw, cyb + bh // 2], fill=(255, 255, 255, 255))
    return big

idle = compose(0)
spin = [compose(-i * 180.0 / FRAMES) for i in range(FRAMES)]
g = idle.convert('LA').convert('RGBA')
paused = badge(Image.blend(idle, g, 0.85), (245, 158, 11, 255), 'pause')
error = badge(idle, (220, 38, 38, 255), 'error')

def save(img, name):
    for s in SIZES:
        img.resize((s, s), Image.LANCZOS).save(f'assets/{name}_{s}.png', optimize=True)

save(idle, 'idle'); save(paused, 'paused'); save(error, 'error')
for i, f in enumerate(spin):
    save(f, f'spin{i}')
idle.save('assets/app.ico', sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
print('ok', cx, cy)
