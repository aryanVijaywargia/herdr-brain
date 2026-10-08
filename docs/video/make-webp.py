"""Make the animated README WebP from the rendered MP4.

Usage: python3 make-webp.py <in.mp4> <out.webp>

ffmpeg takes 15 frames a second at 720x720, and Pillow writes them as one WebP that loops.
The local ffmpeg has no WebP encoder, so Pillow does that step.
"""

import pathlib
import subprocess
import sys
import tempfile

from PIL import Image

FPS = 15  # half of the 30 fps video, as the herdr-projects WebP
SIZE = 720
QUALITY = 72  # keeps the 22 s WebP under 3.5 MB


def main(src: str, dst: str) -> None:
    with tempfile.TemporaryDirectory() as tmp:
        subprocess.run(
            [
                "ffmpeg", "-v", "error", "-i", src,
                "-vf", f"fps={FPS},scale={SIZE}:{SIZE}:flags=lanczos",
                f"{tmp}/f-%04d.png",
            ],
            check=True,
        )
        frames = [Image.open(p).convert("RGB") for p in sorted(pathlib.Path(tmp).glob("f-*.png"))]
        frames[0].save(
            dst,
            save_all=True,
            append_images=frames[1:],
            duration=round(1000 / FPS),
            loop=0,
            quality=QUALITY,
            method=6,
        )
    print(f"{dst}: {len(frames)} frames, {pathlib.Path(dst).stat().st_size} bytes")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
