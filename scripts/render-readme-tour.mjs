#!/usr/bin/env node
// Assemble a captioned browser recording. All captions stay outside the product UI.
import { spawnSync } from 'node:child_process';
import { readFile, writeFile, mkdir, copyFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const input = resolve('output/playwright/readme');
const output = resolve('docs/media');
await mkdir(output, { recursive: true });
const timeline = JSON.parse(await readFile(resolve(input, 'tour-timeline.json'), 'utf8'));
const { chapters, end } = timeline;
const start = chapters[0].start;
const duration = end - start;
const { chromium } = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
const browser = await chromium.launch({ headless: true,
  ...(process.env.AGENTBOX_BROWSER_CHANNEL ? { channel: process.env.AGENTBOX_BROWSER_CHANNEL } : {}) });
const banners = [];
const escape = s => s.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 112 }, deviceScaleFactor: 1 });
  for (const [i, chapter] of chapters.entries()) {
    await page.setContent(`<!doctype html><meta charset="utf-8"><style>
      *{box-sizing:border-box}body{margin:0;background:#151922;color:#e6e9ef;font:20px -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC",sans-serif;height:112px;display:flex;align-items:center;padding:0 32px;gap:44px;border-bottom:2px solid #d99a2b}
      .brand{font-size:20px;letter-spacing:2px;color:#eeb34c;font-weight:700}.note{font-size:12px;color:#a8b1c1;letter-spacing:0;margin-top:10px;font-weight:400}.en{font-size:25px;font-weight:650}.cn{font-size:19px;color:#c1c8d4;margin-top:7px}.count{margin-left:auto;color:#eeb34c;font-size:24px;font-variant-numeric:tabular-nums}
      </style><div class="brand">AGENTBOX<div class="note">Demo data · 演示数据</div></div><div><div class="en">${escape(chapter.en.slice(4))}</div><div class="cn">${escape(chapter.cn)}</div></div><div class="count">${String(i + 1).padStart(2, '0')} / ${String(chapters.length).padStart(2, '0')}</div>`);
    await page.evaluate(() => document.fonts.ready);
    const banner = resolve(input, `chapter-${i + 1}.png`);
    await page.screenshot({ path: banner });
    banners.push(banner);
  }
} finally { await browser.close(); }

function run(command, args) {
  const result = spawnSync(command, args, { stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} failed (${result.status})`);
}
const args = ['-hide_banner', '-loglevel', 'warning', '-y', '-i', resolve(input, 'tour.webm')];
for (const banner of banners) args.push('-i', banner);
let filter = `[0:v]trim=start=${start}:end=${end},setpts=PTS-STARTPTS,pad=iw:ih+112:0:112:color=0x151922[v0]`;
for (let i = 0; i < chapters.length; i++) {
  const from = i === 0 ? 0 : chapters[i].start - start;
  const until = i + 1 < chapters.length ? chapters[i + 1].start - start : duration + 1;
  filter += `;[v${i}][${i + 1}:v]overlay=0:0:enable='between(t,${from},${until})'[v${i + 1}]`;
}
const mp4 = resolve(output, 'agentbox-tour.mp4');
run('ffmpeg', [...args, '-filter_complex', filter, '-map', `[v${chapters.length}]`,
  '-an', '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', mp4]);
// Short silent loop for GitHub Markdown; the linked MP4 has all eight chapters.
run('ffmpeg', ['-hide_banner', '-loglevel', 'warning', '-y', '-i', mp4,
  '-t', String(chapters[4].start - start), '-filter_complex',
  'fps=6,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=192:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4',
  '-loop', '0', resolve('docs/images/tour.gif')]);
const names = ['chat-dark', 'chat-light', 'chat-mobile', 'terminal', 'changes', 'files', 'preview', 'skills', 'mcp', 'usage', 'accounts', 'tunnel'];
for (const name of names) await copyFile(resolve(input, name + '.png'), resolve('docs/images', name + '.png'));
const revision = spawnSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).stdout.trim();
const probe = spawnSync('ffprobe', ['-v', 'error', '-show_entries', 'format=duration', '-of', 'json', mp4], { encoding: 'utf8' });
if (probe.status !== 0) throw new Error('Cannot inspect encoded video duration');
const encodedDuration = Number(JSON.parse(probe.stdout).format.duration);
await writeFile(resolve(output, 'capture.json'), JSON.stringify({
  source_revision: revision,
  generated_at: new Date().toISOString(),
  data: 'Synthetic fixtures; no model calls, real accounts or production access.',
  screenshot_css_viewport: { width: 1440, height: 960 },
  screenshot_device_scale_factor: 2,
  mobile_css_viewport: { width: 390, height: 844 },
  video: { width: 1440, height: 1072, duration_seconds: encodedDuration,
    format: 'H.264 / yuv420p, silent, faststart',
    chapters: chapters.map(c => ({ start_seconds: Number((c.start - start).toFixed(2)), en: c.en, zh_CN: c.cn })) },
  screenshots: names.map(name => `docs/images/${name}.png`),
}, null, 2) + '\n');
console.log(`Published ${names.length} screenshots, ${duration.toFixed(1)}s MP4 and short GIF to docs/.`);
