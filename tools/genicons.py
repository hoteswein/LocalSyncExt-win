#!/usr/bin/env python3
"""Иконки из присланных ICO: python3 tools/genicons.py <папка с ICO>
   copilot-mu_1.ico — иконка программы (exe, favicon), copilot-mu-ON.ico — рабочий режим,
   copilot-mu-Active.ico — идёт синхронизация (мигает с ON), copilot-mu-Paused.ico — пауза.
   Для состояния «ошибка» значок ON получает красный бейдж."""
import sys, os
from PIL import Image, ImageDraw

src = sys.argv[1]
SIZES = [16, 20, 24, 32, 40, 48]

def load(name):
    return Image.open(os.path.join(src, name)).convert('RGBA')

prog, on, active, paused = (load('copilot-mu_1.ico'), load('copilot-mu-ON.ico'),
                            load('copilot-mu-Active.ico'), load('copilot-mu-Paused.ico'))

def with_badge(img):
    big = img.resize((256, 256), Image.LANCZOS)
    d = ImageDraw.Draw(big)
    r, cx, cy = 54, 196, 196
    d.ellipse([cx - r - 10, cy - r - 10, cx + r + 10, cy + r + 10], fill=(255, 255, 255, 255))
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=(220, 38, 38, 255))
    return big

def save(img, name):
    for s in SIZES:
        img.resize((s, s), Image.LANCZOS).save(f'assets/{name}_{s}.png', optimize=True)

save(on, 'on'); save(active, 'active'); save(paused, 'paused'); save(with_badge(on), 'error')
prog.save('assets/app.ico', sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)])
os.makedirs('web', exist_ok=True)
prog.save('web/favicon.ico', sizes=[(16, 16), (32, 32), (48, 48), (64, 64)])
print('ok')
