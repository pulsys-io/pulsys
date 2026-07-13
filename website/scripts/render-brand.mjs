// Render brand PNGs (GitHub avatar, OG image, apple-touch-icon) from inline
// SVG sources. Run manually after changing the mark:
//
//   node scripts/render-brand.mjs
//
// Outputs are checked in so the site build does not depend on this script.
import sharp from 'sharp';
import { mkdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const pub = path.join(__dirname, '../public');
const brandDir = path.join(pub, 'brand');

const ACCENT = '#68a8ff';
const BG = '#0b0d10';

/** Pulse mark path on a 32x32 grid, stroke-width 3. */
const markPath = (sw = 3) =>
  `<path d="M3 16 H10 L13.5 7 L18.5 25 L22 16 H29" fill="none" stroke="${ACCENT}" stroke-width="${sw}" stroke-linecap="round" stroke-linejoin="round"/>`;

/** GitHub org avatar: full-bleed dark square, centered mark. */
const avatarSvg = `<svg xmlns="http://www.w3.org/2000/svg" width="512" height="512" viewBox="0 0 512 512">
  <rect width="512" height="512" fill="${BG}"/>
  <radialGradient id="glow" cx="0.5" cy="0.42" r="0.65">
    <stop offset="0" stop-color="${ACCENT}" stop-opacity="0.16"/>
    <stop offset="1" stop-color="${ACCENT}" stop-opacity="0"/>
  </radialGradient>
  <rect width="512" height="512" fill="url(#glow)"/>
  <g transform="translate(96 96) scale(10)">${markPath(3)}</g>
</svg>`;

/** Apple touch icon: dark rounded square (iOS applies its own mask, keep square). */
const touchSvg = `<svg xmlns="http://www.w3.org/2000/svg" width="180" height="180" viewBox="0 0 180 180">
  <rect width="180" height="180" fill="${BG}"/>
  <g transform="translate(26 26) scale(4)">${markPath(3)}</g>
</svg>`;

/** OG / social card: mark + wordmark + tagline. */
const ogSvg = `<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <rect width="1200" height="630" fill="${BG}"/>
  <radialGradient id="glow" cx="0.32" cy="0.3" r="0.85">
    <stop offset="0" stop-color="${ACCENT}" stop-opacity="0.14"/>
    <stop offset="1" stop-color="${ACCENT}" stop-opacity="0"/>
  </radialGradient>
  <rect width="1200" height="630" fill="url(#glow)"/>
  <g transform="translate(96 200) scale(5.5)">${markPath(3)}</g>
  <text x="316" y="308" font-family="Helvetica, Arial, sans-serif" font-weight="bold" font-size="118" fill="#f5f7fa" letter-spacing="-3">Pulsys</text>
  <text x="100" y="436" font-family="Helvetica, Arial, sans-serif" font-size="42" fill="#b2becd">Open-source pull-through cache for Hugging Face.</text>
  <text x="100" y="500" font-family="Helvetica, Arial, sans-serif" font-size="42" fill="#b2becd">Pull once. Serve from disk at wire speed.</text>
</svg>`;

await mkdir(brandDir, { recursive: true });

await sharp(Buffer.from(avatarSvg))
  .png()
  .toFile(path.join(brandDir, 'github-avatar-512.png'));
await sharp(Buffer.from(touchSvg))
  .png()
  .toFile(path.join(pub, 'apple-touch-icon.png'));
await sharp(Buffer.from(ogSvg)).png().toFile(path.join(pub, 'og.png'));

// Favicon PNG fallbacks rendered from the checked-in favicon.svg (single
// source of truth for the mark-on-rounded-square icon).
const faviconSvg = await readFile(path.join(pub, 'favicon.svg'));
for (const size of [16, 32, 48, 180, 512]) {
  await sharp(faviconSvg, { density: (72 * size) / 32 })
    .resize(size, size)
    .png()
    .toFile(path.join(pub, `favicon-${size}.png`));
}

console.log(
  'brand assets written: brand/github-avatar-512.png, apple-touch-icon.png, og.png, favicon-{16,32,48,180,512}.png'
);
