import { execFileSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const desktop = fileURLToPath(new URL('..', import.meta.url));
const host = execFileSync('rustc', ['-vV'], { encoding: 'utf8' }).match(/^host: (.+)$/m)?.[1];
const target = process.env.TAURI_ENV_TARGET_TRIPLE || process.argv[2] || host;
const targets = { 'aarch64-apple-darwin': ['darwin', 'arm64'], 'x86_64-apple-darwin': ['darwin', 'amd64'], 'x86_64-pc-windows-msvc': ['windows', 'amd64'] };
if (!(target in targets)) throw new Error(`Unsupported desktop target: ${target}`);
const [os, arch] = targets[target];
const output = path.join(desktop, 'src-tauri', 'binaries', `abox-sync-${target}${os === 'windows' ? '.exe' : ''}`);
mkdirSync(path.dirname(output), { recursive: true });
execFileSync('go', ['build', '-trimpath', '-o', output, './cmd/abox-sync'], { cwd: path.dirname(desktop), env: { ...process.env, CGO_ENABLED: '0', GOOS: os, GOARCH: arch }, stdio: 'inherit' });
console.log(`Built ${path.basename(output)}`);
