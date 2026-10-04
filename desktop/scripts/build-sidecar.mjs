import { execFileSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const desktop = fileURLToPath(new URL('..', import.meta.url));
let target = process.env.TAURI_ENV_TARGET_TRIPLE || process.argv[2];
if (!target) {
  console.log('Resolving the native Rust target…');
  target = execFileSync('rustc', ['-vV'], {
    encoding: 'utf8', timeout: 60_000, stdio: ['ignore', 'pipe', 'inherit'],
  }).match(/^host: (.+)$/m)?.[1];
}
const targets = { 'aarch64-apple-darwin': ['darwin', 'arm64'], 'x86_64-apple-darwin': ['darwin', 'amd64'], 'x86_64-pc-windows-msvc': ['windows', 'amd64'] };
if (!(target in targets)) throw new Error(`Unsupported desktop target: ${target}`);
const [os, arch] = targets[target];
const output = path.join(desktop, 'src-tauri', 'binaries', `abox-sync-${target}${os === 'windows' ? '.exe' : ''}`);
mkdirSync(path.dirname(output), { recursive: true });
console.log(`Building Go sidecar for ${target} (15-minute limit)…`);
execFileSync('go', ['build', '-trimpath', '-o', output, './cmd/abox-sync'], { cwd: path.dirname(desktop), env: { ...process.env, CGO_ENABLED: '0', GOOS: os, GOARCH: arch }, stdio: 'inherit', timeout: 15 * 60_000 });
console.log(`Built ${path.basename(output)}`);
