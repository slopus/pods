from PIL import Image, ImageDraw

src = Image.open("favicon-square.png").convert("RGBA")
N = src.width  # 1024

# Supersampled circular mask for a crisp, anti-aliased edge.
SS = 4
mask_big = Image.new("L", (N*SS, N*SS), 0)
ImageDraw.Draw(mask_big).ellipse((0, 0, N*SS-1, N*SS-1), fill=255)
mask = mask_big.resize((N, N), Image.LANCZOS)

round_img = src.copy()
round_img.putalpha(mask)

out_dir = "/Users/steve/conductor/workspaces/pods/sarajevo-v1/internal/server/web"
def save_png(size, name):
    round_img.resize((size, size), Image.LANCZOS).save(f"{out_dir}/{name}")
    print("  ", name, f"{size}x{size}")

save_png(512, "icon-512.png")
save_png(192, "icon-192.png")
save_png(180, "apple-touch-icon.png")
save_png(32,  "favicon-32.png")
save_png(16,  "favicon-16.png")

# Multi-resolution .ico
ico = round_img.resize((256, 256), Image.LANCZOS)
ico.save(f"{out_dir}/favicon.ico", sizes=[(16,16),(32,32),(48,48),(64,64)])
print("   favicon.ico (16/32/48/64)")

# Keep a full-res round master for reference/preview.
round_img.save("/tmp/fav/favicon-round-preview.png")
print("done")
