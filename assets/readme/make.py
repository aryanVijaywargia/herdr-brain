#!/usr/bin/env python3
"""Build assets/readme/hero.svg: a particle portrait of Plato that turns slowly.

Rebuild: python3 assets/readme/make.py  (needs numpy and Pillow; downloads the scan once)

Source: 3D scan of a plaster cast of Plato, SMK inv. KAS2111, Statens Museum for Kunst,
Copenhagen, public domain (Public Domain Mark 1.0): https://open.smk.dk/artwork/image/KAS2111

How it works: the script samples points on the scanned mesh, lights them (key light, shadow
map, ambient occlusion and a rim light), and places dots where the surface is lit, so the dot
density is the brightness. The SVG groups the dots by depth. CSS keyframes shift each depth
group a little in x and y, which reads as a slow turn of the head. A few dots drift away and
settle back. The SVG runs no script, so GitHub shows it animated in an <img>.
"""

import argparse
import hashlib
import math
import os
import sys
import tempfile
import urllib.request

import numpy as np
from PIL import Image

SCAN_URL = "https://api.smk.dk/api/v1/download-3d/p8418t28t_KAS2111_small.stl"
SCAN_SHA256 = "b63a1071583df3bc39cec76de77b8ab8fe59a1bab0c90283439c11d7b0c291cf"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "hero.svg")

# All the numbers below are art choices, tuned by eye.
W, H = 3200, 1800         # SVG view box; integer coordinates are precise enough at this size
HEAD_H = 1660             # height of the shading image in view box units
SEED = 7
SAMPLES = 6_000_000       # mesh samples for the depth, shadow and occlusion maps
RES = 700                 # shading image height in pixels
FRAME_MM = 112.0          # scan millimetres from the top of the image to its bottom
DOTS = 52_000             # dots in the head
JITTER = 0.5             # dot jitter in grid cells, so no lattice shows
LOOSE = 0.09              # share of the head dots that drift away and settle back
DUST = 260                # dots that float in the background
GAMMA = 2.1               # brightness to dot density
YAW0, PITCH0 = 24.0, 6.0  # base view in degrees
LIGHT = (-0.55, 0.62, 0.56)  # key light, view space (x right, y up, z to the viewer)
RIM = (0.9, 0.25, -0.35)     # rim light from behind on the right
AO_DIRS = 28
LAYERS = 24               # depth groups that move together
YAW, PITCH = 7.0, 2.2     # turn amplitude in degrees
YAW_S, PITCH_S = 13.0, 9.0  # seconds for one sweep; a full cycle is twice this
CELL = 400                # grid cell in view box units
BG, INK = "#1c1c1c", "#e8e8e8"
# Dot classes: stroke width in view box units, opacity.
CLASSES = [(3.0, 0.3), (3.5, 0.52), (4.1, 0.82)]
EASE = "cubic-bezier(.37,0,.63,1)"  # close to a sine half wave


def fetch(cache):
    """Return the path of the scan, downloaded once into cache."""
    path = os.path.join(cache, "KAS2111_small.stl")
    if not os.path.exists(path):
        os.makedirs(cache, exist_ok=True)
        print("downloading", SCAN_URL, file=sys.stderr)
        tmp = path + ".part"
        urllib.request.urlretrieve(SCAN_URL, tmp)
        os.replace(tmp, path)
    with open(path, "rb") as f:
        got = hashlib.sha256(f.read()).hexdigest()
    if got != SCAN_SHA256:
        sys.exit(f"{path}: sha256 {got}, want {SCAN_SHA256}; delete it and run again")
    return path


def load_stl(path):
    """Return the triangles of a binary STL as an (n, 3, 3) array."""
    with open(path, "rb") as f:
        data = f.read()
    n = int.from_bytes(data[80:84], "little")
    dt = np.dtype([("n", "<f4", 3), ("v", "<f4", (3, 3)), ("a", "<u2")])
    return np.frombuffer(data, dtype=dt, count=n, offset=84)["v"].astype(np.float64)


def rot_y(deg):
    a = math.radians(deg)
    c, s = math.cos(a), math.sin(a)
    return np.array([[c, 0, s], [0, 1, 0], [-s, 0, c]])


def rot_x(deg):
    a = math.radians(deg)
    c, s = math.cos(a), math.sin(a)
    return np.array([[1, 0, 0], [0, c, -s], [0, s, c]])


def unit(v):
    v = np.asarray(v, dtype=np.float64)
    return v / np.linalg.norm(v)


def smoothstep(a, b, x):
    t = np.clip((x - a) / (b - a), 0, 1)
    return t * t * (3 - 2 * t)


def sample(tris, n, rng):
    """Sample n points on the mesh, uniform by area. Returns points and unit normals."""
    e1 = tris[:, 1] - tris[:, 0]
    e2 = tris[:, 2] - tris[:, 0]
    cross = np.cross(e1, e2)
    area = np.linalg.norm(cross, axis=1)
    keep = area > 0
    tris, e1, e2, cross, area = tris[keep], e1[keep], e2[keep], cross[keep], area[keep]
    idx = rng.choice(len(tris), size=n, p=area / area.sum())
    r1 = np.sqrt(rng.random(n))[:, None]
    r2 = rng.random(n)[:, None]
    pts = tris[idx, 0] + e1[idx] * (r1 * (1 - r2)) + e2[idx] * (r1 * r2)
    return pts, cross[idx] / area[idx, None]


def basis(d):
    """Return an orthonormal basis (right, up, d) for a view along -d."""
    d = unit(d)
    up = np.array([0.0, 1.0, 0.0]) if abs(d[1]) < 0.9 else np.array([1.0, 0.0, 0.0])
    right = unit(np.cross(up, d))
    return np.stack([right, np.cross(d, right), d])


class DepthMap:
    """An orthographic depth map of points seen from direction d (the viewer sits at +d)."""

    def __init__(self, pts, d, size, lo, hi):
        self.b = basis(d)
        self.lo, self.size = lo, size
        self.scale = (size - 1) / (hi - lo)
        q = pts @ self.b.T
        i, j = self.texel(q)
        self.depth = np.full((size, size), -np.inf)
        np.maximum.at(self.depth, (j, i), q[:, 2])

    def texel(self, q):
        i = np.clip(((q[:, 0] - self.lo) * self.scale).astype(int), 0, self.size - 1)
        j = np.clip(((q[:, 1] - self.lo) * self.scale).astype(int), 0, self.size - 1)
        return i, j

    def lit(self, pts, bias):
        """Fraction of the 3x3 texels around each point that do not occlude it."""
        q = pts @ self.b.T
        i, j = self.texel(q)
        s = self.size - 1
        seen = np.zeros(len(pts))
        for dj in (-1, 0, 1):
            for di in (-1, 0, 1):
                d = self.depth[np.clip(j + dj, 0, s), np.clip(i + di, 0, s)]
                seen += d <= q[:, 2] + bias
        return seen / 9


def shade(tris, rng):
    """Render the head in view space.

    Returns the dot weight image, the depth image (view space z, mm), the pixel size in mm
    and the depth of the turn axis.
    """
    # The scan is z-up and faces -y. View space: x right, y up, z toward the viewer.
    to_view = np.array([[1, 0, 0], [0, 0, 1], [0, -1, 0]], dtype=np.float64)
    m = rot_x(PITCH0) @ rot_y(YAW0) @ to_view
    scan_pts, scan_nrm = sample(tris, SAMPLES, rng)
    pts = scan_pts @ m.T
    nrm = scan_nrm @ m.T

    top = pts[:, 1].max() + 4.0  # [mm] margin above the skull
    px = FRAME_MM / RES
    skull = pts[:, 1] > top - 44.0
    cx = np.median(pts[skull, 0])
    axis_z = np.median(pts[skull, 2])  # the head turns about a vertical axis in the skull
    width = int(RES * 0.95)
    x0 = cx - width * px / 2
    u = ((pts[:, 0] - x0) / px).astype(int)
    v = ((top - pts[:, 1]) / px).astype(int)
    inside = (u >= 0) & (u < width) & (v >= 0) & (v < RES)

    # Visible surface: the nearest sample in each pixel.
    k = np.flatnonzero(inside)
    pix = v[k] * width + u[k]
    order = np.lexsort((pts[k, 2], pix))
    last = np.r_[pix[order][1:] != pix[order][:-1], True]
    win = k[order[last]]
    wpix = pix[order[last]]
    depth = np.full(RES * width, -np.inf)
    depth[wpix] = pts[win, 2]
    pos = np.zeros((RES * width, 3))
    pos[wpix] = pts[win]
    nor = np.zeros((RES * width, 3))
    nor[wpix] = nrm[win]
    depth = depth.reshape(RES, width)
    pos = pos.reshape(RES, width, 3)
    nor = nor.reshape(RES, width, 3)

    # Holes and back faces that leak through: take the nearest 3x3 neighbour instead.
    for _ in range(2):
        best = depth.copy()
        bpos, bnor = pos.copy(), nor.copy()
        for dv in (-1, 0, 1):
            for du in (-1, 0, 1):
                sd = np.roll(np.roll(depth, dv, 0), du, 1)
                take = sd > best + 1.5  # [mm] a real surface is within 1.5 mm of its neighbours
                best = np.where(take, sd, best)
                bpos = np.where(take[..., None], np.roll(np.roll(pos, dv, 0), du, 1), bpos)
                bnor = np.where(take[..., None], np.roll(np.roll(nor, dv, 0), du, 1), bnor)
        depth, pos, nor = best, bpos, bnor
    mask = np.isfinite(depth)

    p = pos[mask]
    n = nor[mask]
    n /= np.linalg.norm(n, axis=1, keepdims=True) + 1e-12
    lo, hi = pts.min() - 1, pts.max() + 1
    sub = pts[rng.choice(len(pts), size=len(pts) // 3, replace=False)]

    key = unit(LIGHT)
    lam = np.clip(n @ key, 0, 1)
    sh = DepthMap(pts, key, 1400, lo, hi).lit(p, bias=0.6)

    # Ambient occlusion: visibility from many directions on the viewer's side.
    ao = np.zeros(len(p))
    golden = math.pi * (3 - math.sqrt(5))
    for i in range(AO_DIRS):
        zc = 1 - (i + 0.5) / AO_DIRS
        r = math.sqrt(1 - zc * zc)
        d = np.array([r * math.cos(i * golden), r * math.sin(i * golden), zc])
        ao += np.clip(n @ d, 0, 1) * DepthMap(sub, d, 420, lo, hi).lit(p, bias=1.2)
    ao /= AO_DIRS / 2

    grazing = (1 - np.clip(n[:, 2], 0, 1)) ** 2.5
    rim = grazing * np.clip(n @ unit(RIM), 0, 1)
    b = 0.95 * lam * (0.25 + 0.75 * sh) * (0.55 + 0.45 * ao) + 0.22 * ao ** 1.5 + 0.55 * rim
    b = np.clip(b / np.percentile(b, 99.5), 0, 1)

    # Keep the head: drop the pedestal and the neck block behind the beard, and let the end
    # of the beard thin out. The limits are scan millimetres (z up, the face toward -y).
    s = p @ m
    keep = smoothstep(27.0, 44.0, s[:, 2])
    keep *= 1 - smoothstep(30.0, 35.0, s[:, 1]) * (1 - smoothstep(54.0, 63.0, s[:, 2]))

    weight = np.zeros(depth.shape)
    weight[mask] = b ** GAMMA * keep
    bright = np.zeros(depth.shape)
    bright[mask] = b
    z = np.zeros(depth.shape)
    z[mask] = p[:, 2] - axis_z
    return weight, bright, z, px


def stipple(weight, bright, z, px, rng):
    """Place DOTS dots with density weight. Returns x, y, z (view box units) and brightness."""
    h, w = weight.shape
    flat = weight.ravel()
    # Error diffusion on a grid sized for DOTS dots spreads the dots evenly.
    f = math.sqrt(DOTS / flat.sum())
    gw, gh = round(w * f), round(h * f)
    g = Image.fromarray(weight.astype(np.float32)).resize((gw, gh), Image.BILINEAR)
    hit = diffuse(np.clip(np.asarray(g, dtype=np.float64), 0, 1))
    gj, gi = np.nonzero(hit)
    jj = (gj + 0.5 + rng.uniform(-JITTER, JITTER, len(gj))) / f
    ii = (gi + 0.5 + rng.uniform(-JITTER, JITTER, len(gi))) / f
    j = np.clip(jj.astype(int), 0, h - 1)
    i = np.clip(ii.astype(int), 0, w - 1)
    idx = j * w + i
    scale = HEAD_H / h
    x = ii * scale + (W - w * scale) / 2
    y = jj * scale + (H - HEAD_H) / 2
    zu = z.ravel()[idx] * scale / px
    return x, y, zu, bright.ravel()[idx]


def diffuse(g):
    """Floyd-Steinberg error diffusion in serpentine order. Returns a bool image of dots."""
    h, w = g.shape
    err = g.tolist()
    hit = np.zeros((h, w), dtype=bool)
    for y in range(h):
        row = err[y]
        nxt = err[y + 1] if y + 1 < h else [0.0] * w
        fwd = y % 2 == 0
        d = 1 if fwd else -1
        for x in range(w) if fwd else range(w - 1, -1, -1):
            v = row[x]
            if v >= 0.5:
                hit[y, x] = True
                v -= 1.0
            if 0 <= x + d < w:
                row[x + d] += v * 7 / 16
                nxt[x + d] += v * 1 / 16
            if 0 <= x - d < w:
                nxt[x - d] += v * 3 / 16
            nxt[x] += v * 5 / 16
    return hit


def path(xs, ys):
    """Return SVG path data with one dot (a zero-length line) at each point."""
    xs = np.rint(xs).astype(int)
    ys = np.rint(ys).astype(int)
    band = ys // 60
    order = np.lexsort((np.where(band % 2 == 0, xs, -xs), band))
    out, px_, py_ = [], None, None
    for x, y in zip(xs[order], ys[order]):
        if px_ is None:
            out.append(f"M{x} {y}h0")
        else:
            dx, dy = x - px_, y - py_
            out.append(f"m{dx}{'' if dy < 0 else ' '}{dy}h0")
        px_, py_ = x, y
    return "".join(out)


def svg(x, y, z, b, rng):
    """Return the hero SVG text."""
    n = len(x)
    # Brighter places get more of the brighter dot classes.
    r = rng.random(n)
    cls = np.where(r < 0.15 + 0.7 * b ** 1.5, 2, np.where(r < 0.55 + 0.4 * b, 1, 0))
    loose = rng.random(n) < LOOSE
    zmin, zmax = z.min(), z.max()
    layer = np.minimum(((z - zmin) / (zmax - zmin) * LAYERS).astype(int), LAYERS - 1)

    css = [
        f".d{{fill:none;stroke:{INK};stroke-linecap:round}}",
        *[f".c{k}{{stroke-width:{sw};opacity:{op}}}" for k, (sw, op) in enumerate(CLASSES)],
        f".t{{font:500 30px ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;"
        f"letter-spacing:6px;fill:{INK};opacity:.34}}",
    ]
    body = []

    # Turn: each depth layer shifts in x (yaw) and y (pitch) in proportion to its depth.
    sy, sp = math.sin(math.radians(YAW)), math.sin(math.radians(PITCH))
    for L in range(LAYERS):
        zl = zmin + (L + 0.5) * (zmax - zmin) / LAYERS
        ax, ay = zl * sy, zl * sp
        css.append(f"@keyframes x{L}{{from{{transform:translateX({-ax:.1f}px)}}"
                   f"to{{transform:translateX({ax:.1f}px)}}}}")
        css.append(f"@keyframes y{L}{{from{{transform:translateY({-ay:.1f}px)}}"
                   f"to{{transform:translateY({ay:.1f}px)}}}}")
        css.append(f".x{L}{{animation:x{L} {YAW_S}s {EASE} infinite alternate}}"
                   f".y{L}{{animation:y{L} {PITCH_S}s {EASE} {-PITCH_S / 2}s infinite alternate}}")
        inner = []
        for k in range(len(CLASSES)):
            sel = (layer == L) & (cls == k) & ~loose
            if sel.any():
                inner.append(f'<path class="d c{k}" d="{path(x[sel], y[sel])}"/>')
        body.append(f'<g class="x{L}"><g class="y{L}">{"".join(inner)}</g></g>')

    # Drift: loose dots float outward from the head centre, fade, and settle back.
    cx, cy = W / 2, H / 2
    groups = 36
    angle = np.arctan2(y - cy, x - cx)
    sector = ((angle + math.pi) / (2 * math.pi) * 12).astype(int) % 12
    group = sector * 3 + rng.integers(0, 3, n)
    coarse = np.minimum(layer * 6 // LAYERS, 5)
    for g in range(groups):
        a = (g // 3 + 0.5) / 12 * 2 * math.pi - math.pi + rng.normal(0, 0.25)
        dist = rng.uniform(40, 120)
        dur = rng.uniform(8, 15)
        delay = -rng.uniform(0, dur)
        dx, dy = dist * math.cos(a), dist * math.sin(a) - 25
        css.append(f"@keyframes f{g}{{0%,35%{{transform:translate(0,0);opacity:1}}"
                   f"to{{transform:translate({dx:.0f}px,{dy:.0f}px);opacity:.1}}}}"
                   f".f{g}{{animation:f{g} {dur:.1f}s {EASE} {delay:.1f}s infinite alternate}}")
    for c in range(6):
        L = int((c + 0.5) * LAYERS / 6)
        inner = []
        for g in range(groups):
            sel = loose & (coarse == c) & (group == g)
            if sel.any():
                inner.append(f'<path class="d c1 f{g}" d="{path(x[sel], y[sel])}"/>')
        body.append(f'<g class="x{L}"><g class="y{L}">{"".join(inner)}</g></g>')

    # Dust: faint dots that float in the background.
    dust = []
    for g in range(8):
        k = DUST // 8
        dx_ = rng.uniform(0, W, k)
        dy_ = rng.uniform(0, H, k)
        mx, my = rng.uniform(-60, 60), rng.uniform(-90, -30)
        dur = rng.uniform(14, 24)
        css.append(f"@keyframes u{g}{{from{{transform:translate(0,0)}}"
                   f"to{{transform:translate({mx:.0f}px,{my:.0f}px)}}}}"
                   f".u{g}{{animation:u{g} {dur:.1f}s {EASE} {-rng.uniform(0, dur):.1f}s "
                   f"infinite alternate}}")
        dust.append(f'<path class="d c0 u{g}" style="opacity:.22" d="{path(dx_, dy_)}"/>')

    grid = "".join(f"M{gx} 0V{H}" for gx in range(CELL, W, CELL))
    grid += "".join(f"M0 {gy}H{W}" for gy in range(CELL // 2, H, CELL))
    css.append("@media (prefers-reduced-motion:reduce){*{animation:none!important}}")

    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
        f'role="img" aria-label="A head of Plato drawn in small light dots, turning slowly">'
        f"<style>{''.join(css)}</style>"
        f'<clipPath id="p"><rect width="{W}" height="{H}" rx="28"/></clipPath>'
        f'<g clip-path="url(#p)">'
        f'<rect width="{W}" height="{H}" fill="{BG}"/>'
        f'<path d="{grid}" stroke="#fff" stroke-width="2" opacity=".035"/>'
        f"{''.join(dust)}{''.join(body)}"
        f'<text class="t" x="96" y="{H - 90}">PLATO &#183; SMK KAS2111 &#183; PUBLIC DOMAIN</text>'
        f"</g></svg>\n"
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--cache", default=os.path.join(tempfile.gettempdir(), "brain-readme"))
    ap.add_argument("--shade", help="also write the shading image to this PNG")
    args = ap.parse_args()

    rng = np.random.default_rng(SEED)
    tris = load_stl(fetch(args.cache))
    weight, bright, z, px = shade(tris, rng)
    if args.shade:
        Image.fromarray((np.clip(weight, 0, 1) * 255).astype(np.uint8)).save(args.shade)
    x, y, zu, b = stipple(weight, bright, z, px, rng)
    text = svg(x, y, zu, b, rng)
    with open(OUT, "w", encoding="utf-8") as f:
        f.write(text)
    print(f"wrote {OUT}: {len(text) / 1e6:.2f} MB, {len(x)} dots", file=sys.stderr)


if __name__ == "__main__":
    main()
