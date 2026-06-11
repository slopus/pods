from PIL import Image, ImageDraw, ImageFilter


SIZE = 1024
SCALE = 4
W = SIZE * SCALE
OUT_PATH = "/tmp/fav/favicon-square.png"

NAVY = (11, 16, 32, 255)
DEEP_NAVY = (6, 10, 24, 255)
SKY = (125, 211, 252, 255)
WHITE_HOT = (255, 248, 224, 255)


def spx(value):
    return int(round(value * SCALE))


def box(cx, cy, r):
    return [
        spx(cx - r),
        spx(cy - r),
        spx(cx + r),
        spx(cy + r),
    ]


def alpha_layer(size=W):
    return Image.new("RGBA", (size, size), (0, 0, 0, 0))


def composite_blurred_circle(base, cx, cy, radius, color, blur):
    layer = alpha_layer()
    draw = ImageDraw.Draw(layer)
    draw.ellipse(box(cx, cy, radius), fill=color)
    if blur:
        layer = layer.filter(ImageFilter.GaussianBlur(spx(blur)))
    base.alpha_composite(layer)


def lerp(a, b, t):
    return int(round(a + (b - a) * t))


def mix(c1, c2, t):
    return tuple(lerp(c1[i], c2[i], t) for i in range(4))


def draw_radial_disc(draw, cx, cy, radius, stops, steps=96):
    # Draw large-to-small discs so each inner color becomes the visible gradient.
    for i in range(steps, -1, -1):
        pos = 1.0 - (i / steps)
        left = stops[0]
        right = stops[-1]
        for a, b in zip(stops, stops[1:]):
            if a[0] <= pos <= b[0]:
                left, right = a, b
                break
        span = max(right[0] - left[0], 0.0001)
        t = (pos - left[0]) / span
        color = mix(left[1], right[1], t)
        r = radius * (i / steps)
        draw.ellipse(box(cx, cy, r), fill=color)


def draw_ring(draw, cx, cy, outer_r, inner_r, fill, inner_fill):
    draw.ellipse(box(cx, cy, outer_r), fill=fill)
    draw.ellipse(box(cx, cy, inner_r), fill=inner_fill)


def main():
    img = Image.new("RGBA", (W, W), NAVY)
    draw = ImageDraw.Draw(img)

    # Subtle full-bleed space depth behind the mark.
    composite_blurred_circle(img, 512, 512, 520, (125, 211, 252, 28), 95)
    composite_blurred_circle(img, 512, 512, 365, (246, 189, 96, 22), 105)
    composite_blurred_circle(img, 512, 512, 710, (0, 0, 0, 34), 130)

    # Soft terminal-like horizontal bands, intentionally faint at favicon size.
    band = alpha_layer()
    band_draw = ImageDraw.Draw(band)
    for y in range(0, W, spx(64)):
        band_draw.rectangle([0, y, W, y + spx(10)], fill=(125, 211, 252, 8))
    img.alpha_composite(band)

    cx = cy = 512

    # The centered pod eye: bold geometry first, glow second.
    composite_blurred_circle(img, cx, cy, 360, (125, 211, 252, 58), 48)
    composite_blurred_circle(img, cx, cy, 250, (246, 189, 96, 58), 34)

    draw = ImageDraw.Draw(img)
    draw_ring(draw, cx, cy, 310, 214, SKY, DEEP_NAVY)

    # Small geometric breaks make the ring feel like a pod-bay aperture.
    cut = DEEP_NAVY
    notch_w = spx(62)
    draw.rectangle(
        [spx(cx) - notch_w // 2, spx(cy - 326), spx(cx) + notch_w // 2, spx(cy - 232)],
        fill=cut,
    )
    draw.rectangle(
        [spx(cx) - notch_w // 2, spx(cy + 232), spx(cx) + notch_w // 2, spx(cy + 326)],
        fill=cut,
    )
    draw.rectangle(
        [spx(cx - 326), spx(cy) - notch_w // 2, spx(cx - 232), spx(cy) + notch_w // 2],
        fill=cut,
    )
    draw.rectangle(
        [spx(cx + 232), spx(cy) - notch_w // 2, spx(cx + 326), spx(cy) + notch_w // 2],
        fill=cut,
    )

    # Crisp inner lens and warm hot core.
    draw.ellipse(box(cx, cy, 198), fill=(9, 15, 34, 255))
    draw.ellipse(box(cx, cy, 168), fill=(16, 30, 54, 255))
    draw_radial_disc(
        draw,
        cx,
        cy,
        142,
        [
            (0.0, (246, 189, 96, 255)),
            (0.55, (255, 203, 116, 255)),
            (0.82, (255, 229, 168, 255)),
            (1.0, WHITE_HOT),
        ],
    )

    # HAL-camera-style glass glints, kept large enough to survive 16 px output.
    draw.ellipse(box(cx - 54, cy - 58, 34), fill=(255, 255, 244, 205))
    draw.ellipse(box(cx - 66, cy - 70, 15), fill=(125, 211, 252, 210))

    final = img.resize((SIZE, SIZE), Image.Resampling.LANCZOS).convert("RGB")
    final.save(OUT_PATH)
    print(f"{final.size[0]}x{final.size[1]}")


if __name__ == "__main__":
    main()
