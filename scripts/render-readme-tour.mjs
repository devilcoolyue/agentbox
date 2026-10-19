#!/usr/bin/env node
// Assemble the README tour from the frames scripts/capture-readme.mjs recorded:
// a chapter bar above the unmodified UI, an H.264 MP4 and a looping GIF per
// language, and the screenshots copied into docs/. Needs ffmpeg with libx264
// (set FFMPEG to use a binary outside PATH).
//
//   node scripts/render-readme-tour.mjs [--lang en|zh-CN|all]
import { spawnSync } from 'node:child_process';
import { readFile, writeFile, mkdir, copyFile, readdir, rm } from 'node:fs/promises';
import { resolve, basename } from 'node:path';
import { pathToFileURL } from 'node:url';

const ffmpeg = process.env.FFMPEG || 'ffmpeg';
const input = resolve('output/playwright/readme');
const LANGS = ['en', 'zh-CN'];
const SHOTS = ['chat-dark', 'chat-light', 'chat-live', 'chat-context', 'chat-mobile', 'terminal', 'changes', 'files', 'preview',
  'usage', 'skills', 'mcp', 'accounts', 'tunnel', 'extensions', 'styles'];
const W = 1920, UI_H = 1280, BAR = 144, FPS = 30;
const GIF_W = 1200, GIF_FPS = 10, GIF_CHAPTERS = 4;
const arg = name => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : ''; };
const langs = !arg('--lang') || arg('--lang') === 'all' ? LANGS : [arg('--lang')];

function run(args) {
  const result = spawnSync(ffmpeg, ['-hide_banner', '-loglevel', 'error', '-y', ...args], { stdio: ['ignore', 'inherit', 'pipe'], encoding: 'utf8' });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`ffmpeg failed (${result.status}): ${result.stderr}`);
  return result.stderr;
}
/** Seconds of an encoded file, read from ffmpeg's own probe output. */
function duration(file) {
  const probe = spawnSync(ffmpeg, ['-hide_banner', '-i', file], { encoding: 'utf8' });
  const m = /Duration: (\d+):(\d+):([\d.]+)/.exec(probe.stderr || '');
  if (!m) throw new Error('Cannot read the duration of ' + file);
  return Number(m[1]) * 3600 + Number(m[2]) * 60 + Number(m[3]);
}
const escape = s => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');

async function banners(lang, chapters, dir) {
  const { chromium } = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE
    ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
  const browser = await chromium.launch({ headless: true,
    ...(process.env.AGENTBOX_BROWSER_CHANNEL ? { channel: process.env.AGENTBOX_BROWSER_CHANNEL } : {}) });
  const logo = await readFile(resolve('internal/web/static/img/logo.svg'), 'utf8');
  const note = lang === 'en' ? 'Synthetic demo data' : '合成演示数据';
  const files = [];
  try {
    const page = await browser.newPage({ viewport: { width: W, height: BAR }, deviceScaleFactor: 1 });
    for (const [i, chapter] of chapters.entries()) {
      const dots = chapters.map((_, j) => `<i class="${j < i ? 'done' : j === i ? 'on' : ''}"></i>`).join('');
      await page.setContent(`<!doctype html><meta charset="utf-8"><style>
        *{box-sizing:border-box}
        body{margin:0;height:${BAR}px;background:#0f1218;color:#e8ebf1;display:flex;align-items:center;gap:48px;padding:0 44px;
          font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC","Noto Sans SC",sans-serif;border-bottom:1px solid #232835}
        .brand{display:flex;align-items:center;gap:16px;min-width:300px}.brand svg{width:56px;height:56px}
        .name{font-size:27px;font-weight:700;letter-spacing:.5px}.note{font-size:15px;color:#8d96a8;margin-top:4px}
        .num{font-size:46px;font-weight:700;color:#eeb34c;font-variant-numeric:tabular-nums;width:76px}
        .title{font-size:33px;font-weight:650;letter-spacing:.2px}.sub{font-size:20px;color:#aab3c3;margin-top:7px}
        .dots{margin-left:auto;display:flex;gap:10px}.dots i{width:12px;height:12px;border-radius:50%;background:#2c3241}
        .dots i.done{background:#7a5a24}.dots i.on{background:#eeb34c;box-shadow:0 0 0 4px rgba(238,179,76,.18)}
        </style>
        <div class="brand">${logo}<div><div class="name">agentbox</div><div class="note">${note}</div></div></div>
        <div class="num">${String(i + 1).padStart(2, '0')}</div>
        <div><div class="title">${escape(chapter.title)}</div><div class="sub">${escape(chapter.subtitle)}</div></div>
        <div class="dots">${dots}</div>`);
      await page.evaluate(() => document.fonts.ready);
      const file = resolve(dir, `chapter-${i + 1}.png`);
      await page.screenshot({ path: file });
      files.push(file);
    }
  } finally { await browser.close(); }
  return files;
}

async function render(lang) {
  const dir = resolve(input, lang);
  const timeline = JSON.parse(await readFile(resolve(dir, 'tour-timeline.json'), 'utf8'));
  const { chapters, end } = timeline;
  const start = chapters[0].start, length = end - start;
  // Screencast frames arrive only when the screen changes: hold each one until
  // the next, starting from the last frame shown when the first chapter began.
  const frames = timeline.frames.map(f => ({ file: resolve(dir, 'frames', basename(f.file)), t: f.t })).sort((a, b) => a.t - b.t);
  const first = Math.max(0, frames.findLastIndex(f => f.t <= start));
  const shown = frames.slice(first).filter(f => f.t <= end);
  shown[0] = { ...shown[0], t: start };
  let list = 'ffconcat version 1.0\n';
  shown.forEach((f, i) => {
    const next = i + 1 < shown.length ? shown[i + 1].t : end;
    list += `file '${f.file}'\nduration ${Math.max(0.001, next - f.t).toFixed(4)}\n`;
  });
  list += `file '${shown.at(-1).file}'\n`;
  const listFile = resolve(dir, 'frames.ffconcat');
  await writeFile(listFile, list);
  const bars = await banners(lang, chapters, dir);

  const media = resolve('docs/media'), images = resolve('docs/images', lang);
  await mkdir(media, { recursive: true });
  await mkdir(images, { recursive: true });
  const mp4 = resolve(media, `agentbox-tour-${lang}.mp4`);
  const args = ['-f', 'concat', '-safe', '0', '-i', listFile];
  for (const bar of bars) args.push('-i', bar);
  args.push('-f', 'lavfi', '-i', `color=c=0xeeb34c:s=${W}x3:r=${FPS}`);
  let filter = `[0:v]fps=${FPS},scale=${W}:${UI_H}:flags=lanczos,setsar=1,pad=${W}:${UI_H + BAR}:0:${BAR}:color=0x0f1218[v0]`;
  chapters.forEach((c, i) => {
    const from = (c.start - start).toFixed(3), until = (i + 1 < chapters.length ? chapters[i + 1].start - start : length + 1).toFixed(3);
    filter += `;[v${i}][${i + 1}:v]overlay=0:0:enable='between(t,${from},${until})'[v${i + 1}]`;
  });
  // A progress line across the bottom of the chapter bar.
  filter += `;[v${chapters.length}][${chapters.length + 1}:v]overlay=x='-W+W*t/${length.toFixed(3)}':y=${BAR - 3}:shortest=1[out]`;
  run([...args, '-filter_complex', filter, '-map', '[out]', '-t', length.toFixed(3), '-an',
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '20', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', mp4]);

  // The looping GIF for GitHub Markdown, which does not play repository videos
  // inline: the first chapters, the core loop from task to reviewed diff. Two
  // passes, so the palette never needs every frame in memory.
  const gif = resolve(images, 'tour.gif'), palette = resolve(dir, 'palette.png');
  const gifSeconds = (chapters[GIF_CHAPTERS]?.start ?? end) - start;
  const scale = `fps=${GIF_FPS},scale=${GIF_W}:-1:flags=lanczos`;
  run(['-t', gifSeconds.toFixed(3), '-i', mp4, '-vf', `${scale},palettegen=max_colors=256:stats_mode=diff`, palette]);
  run(['-t', gifSeconds.toFixed(3), '-i', mp4, '-i', palette, '-lavfi', `${scale}[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle`, '-loop', '0', gif]);

  for (const old of await readdir(images)) if (old.endsWith('.png')) await rm(resolve(images, old));
  for (const name of SHOTS) await copyFile(resolve(dir, name + '.png'), resolve(images, name + '.png'));
  return { lang, mp4: `docs/media/agentbox-tour-${lang}.mp4`, gif: `docs/images/${lang}/tour.gif`, seconds: Number(duration(mp4).toFixed(2)),
    gif_seconds: Number(duration(gif).toFixed(2)),
    chapters: chapters.map(c => ({ start_seconds: Number((c.start - start).toFixed(2)), title: c.title, subtitle: c.subtitle })),
    screenshots: SHOTS.map(name => `docs/images/${lang}/${name}.png`) };
}

const results = [];
for (const lang of langs) {
  const result = await render(lang);
  results.push(result);
  console.log(`Published ${lang}: ${result.seconds}s MP4, GIF and ${result.screenshots.length} screenshots.`);
}
const manifest = resolve('docs/media/capture.json');
const previous = JSON.parse(await readFile(manifest, 'utf8').catch(() => '{}'));
const revision = spawnSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).stdout.trim();
const tours = Object.fromEntries((previous.tours || []).map(t => [t.lang, t]));
for (const r of results) tours[r.lang] = { ...r, source_revision: revision, generated_at: new Date().toISOString() };
await writeFile(manifest, JSON.stringify({
  data: 'Synthetic fixtures; no model calls, real accounts or production access.',
  screenshot_css_viewport: { width: 1440, height: 960 },
  screenshot_device_scale_factor: 2,
  mobile_css_viewport: { width: 390, height: 844 },
  video: { width: W, height: UI_H + BAR, fps: FPS, format: 'H.264 / yuv420p, silent, faststart', capture: 'Chrome screencast at 2x pixel density, scaled to 1920 px' },
  gif: { width: GIF_W, fps: GIF_FPS, chapters: GIF_CHAPTERS },
  tours: LANGS.filter(l => tours[l]).map(l => tours[l]),
}, null, 2) + '\n');
