"""Verification commands and their resource boundaries; no commands run on import."""


def step(key, category, argv, note, **options):
    return dict(id=key, category=category, argv=argv, note=note, **options)


STEPS = [
    step('go.build', 'offline', ['go', 'build', './...'], 'Build all Go programs.'),
    step('go.vet', 'offline', ['go', 'vet', './...'], 'Static Go checks.'),
    step('go.unit', 'offline', ['{python}', 'scripts/verify.py', '_go_nonserver'], 'Go packages except server; gated live tests disabled.'),
    step('go.chown-denial', 'linux', ['go', 'test', '-json', './internal/server', '-run', '^(TestSyncMutationLinuxChownDeniedPreservesTarget|TestProjectUploadLinuxChownDenied)$', '-count=1'], 'Linux non-root EPERM regression; root cannot prove this gate.', env={'AGENTBOX_CHOWN_DENIAL_TEST':'1'}, platform='linux', nonroot=True),
    step('race.chat', 'offline', ['{python}', 'scripts/verify.py', '_race_chat'], 'Targeted receipt, lifecycle, store/usage and executor race tests; Linux server runs via sudo.'),
    step('go.server', 'offline', ['{python}', 'scripts/verify.py', '_go_server'], 'Server tests; Linux non-root uses sudo -n only for the test executable.'),
    step('web.types', 'offline', ['npm', 'run', 'check'], 'TypeScript type checking.', requires=['node_modules/.bin/tsc']),
    step('web.artifacts', 'offline', ['{python}', 'scripts/verify.py', '_web_artifacts'], 'Rebuild and compare complete JS inventory/bytes with the starting worktree.', requires=['node_modules/.bin/tsc']),
    *[step('web.'+name, 'offline', ['npm','run','test:'+name], 'Synthetic '+name+' contract/catalog checks.') for name in ['i18n','problems','diagnostics','drafts','outbox','contracts','chat-history','feature-lifetimes']],
    *[step('policy.'+name, 'offline', ['{python}','scripts/test-'+name+'.py'], 'Local synthetic policy regression; not real systemd or installation.') for name in ['browser-runtime','deployment','update','install','uninstall','image-policy','repair-claude-cumulative']],
    step('policy.shell', 'offline', ['{python}','scripts/verify.py','_shell'], 'Shell syntax checks.'),
    step('policy.verification-stability', 'offline', ['{python}','scripts/test-verification-stability.py'], 'Synthetic sample aggregation and uncached-test policy; not twenty real CI runs.'),
    step('runner', 'offline', ['{python}','scripts/test-verification.py'], 'Runner evidence, environment isolation and failure/cancellation regression.'),
    *[step('browser.'+name, 'browser', ['node','scripts/test-'+name+'.mjs'], 'Real browser with synthetic API; no Docker or provider.', requires=['node_modules/.bin/tsc']) for name in ['browser','onboarding','chat-drafts','chat-outbox','workspace-filter','git','preview','web-i18n','link-i18n']],
    *[step('webkit.'+name, 'browser', ['node','scripts/test-'+name+'.mjs'], 'Same synthetic-API scenario in Playwright WebKit; not real Safari, iOS or a device.', env={'AGENTBOX_BROWSER_ENGINE':'webkit'}, requires=['node_modules/.bin/tsc']) for name in ['browser','onboarding','chat-drafts','chat-outbox']],
    step('ui.image-update-gate', 'browser', ['node','scripts/test-browser.mjs'], 'Focused candidate-update UI, preceded by diagnostics/problem fixtures; synthetic API, three languages and narrow layout.', env={'AGENTBOX_BROWSER_ONLY_IMAGE_UPDATES':'1'}),
    step('integration.chat', 'browser', ['node','scripts/test-chat-integration.mjs'], 'Real local agentbox binary/API/SQLite with synthetic Docker/CLI output; native non-root asserts attachment chown denial. Use docker.chat-reliability for the full Linux matrix.'),
    step('docker.chat-reliability', 'docker', ['node','scripts/test-chat-integration.mjs','--linux','--image','{image}'], 'Actual Linux server process, crash/restart, browser HTTP/WS and ledger; synthetic Docker/CLI sidecar, temporary volume, no provider.'),
    step('audit.licenses', 'offline', ['{python}','scripts/verify-third-party.py'], 'Check bundled bytes and the linked-module license inventory.'),
    step('backup', 'offline', ['bash','scripts/test-backup.sh'], 'Actual backup/verify/restore CLI on synthetic temporary data, without Docker opt-in.'),
    step('docker.recovery-drill', 'docker', ['{python}','scripts/test-recovery-drill.py','--image','{recovery_image}'], 'Owned privileged/private-cgroup Linux systemd fixture; real Docker socket mount checks, synthetic scale/ledger/files, schema 9 upgrade, system/full backup and recovery timings. No host app data.'),
    step('docker.filesystem', 'docker', ['bash','scripts/test-filesystem-linux.sh'], 'Builds minimal test image; isolated Linux tests with no user mounts.'),
    step('docker.admin-recovery', 'docker', ['bash','scripts/test-admin-password-linux.sh'], 'Existing agentbox-client-test:local image; isolated UID 1000 recovery/PTY tests.'),
    step('docker.cli-candidate', 'docker', ['go','test','-json','./internal/dockerx','-run','^TestCLIImageProbeLive$','-count=1'], 'Production candidate gate: immutable local image, network=none, real CLI against synthetic upstream; no account/workspace mounts.', env={'AGENTBOX_CLI_PROBE_TEST_IMAGE':'{image}'}),
    step('docker.reasoning', 'docker', ['go','test','-json','./internal/agent','-run','TestReasoningCLI','-count=1'], 'Real CLI against synthetic upstream, network=none; no model charge.', env={'AGENTBOX_CLI_TEST_IMAGE':'{image}'}),
    step('docker.mcp', 'docker', ['{python}','scripts/test-mcp-live.py','--image','{image}'], 'Real Claude MCP with synthetic upstream, not a paid model.'),
    step('docker.mcp-server', 'docker', ['{python}','scripts/test-mcp-server.py','--binary','{binary}','--image','{image}'], 'Actual Go API to isolated container; Linux binary required.'),
    step('docker.terminal', 'docker', ['node','scripts/test-terminal.mjs'], 'Real browser to Docker PTY/tmux/Vim.', env={'AGENTBOX_TERMINAL_IMAGE':'{image}'}),
    step('docker.browser-proxy', 'docker', ['{python}','scripts/test-browser-proxy-live.py','--image','{image}'], 'Real browser/proxy on synthetic sites; browser-enabled image required.'),
    step('docker.remote-browser', 'docker', ['{python}','scripts/test-remote-browser-live.py','--binary','{binary}','--image','{image}'], 'Isolated Go API to real browser; includes the live .mjs helper.'),
    step('docker.transparent', 'docker', ['{python}','scripts/test-transparent-network.py','--server','{binary}','--client','{client}','--fixture','{fixture}','--image','{image}','--network-image','{network_image}'], 'Isolated transparent TCP/DNS/network namespace regression.'),
    step('release.archives', 'docker', ['{python}','scripts/test-release.py','{artifacts}'], 'Seven candidate archives/checksums and packaged Linux binaries; no publishing.'),
    step('release.server', 'docker', ['{python}','scripts/test-release-server.py','--binary','{binary}','--image','{image}','--usage','--restart'], 'Real temporary Linux server/container restart and synthetic terminal usage.'),
    step('release.deployment', 'docker', ['{python}','scripts/test-deployment-linux.py','--binary','{binary}','--image','{image}'], 'Isolated actual Linux installation and recovery with simulated systemctl; not real systemd or production.'),
    step('release.rollback', 'linux', ['{python}','scripts/test-rollback-drill.py','--old','{previous_package}','--new','{package}'], 'Previous and candidate Linux release packages: release.py upgrade, refused direct downgrade, compatible-backup restore to a new directory and previous-release start. Simulated systemctl, stub Docker API, synthetic data; root or UID 1000.', platform='linux'),
    step('release.install', 'docker', ['{python}','scripts/test-install-linux.py','--binary','{binary}','--image','{image}'], 'Isolated Linux install/uninstall; not production.'),
    step('desktop.web', 'offline', ['npm','--prefix','desktop','run','build'], 'Desktop renderer types and bundle.', requires=['desktop/node_modules/.bin/vite']),
    step('desktop.unit', 'offline', ['npm','--prefix','desktop','test'], 'Desktop Vitest, no native app launch.', requires=['desktop/node_modules/.bin/vitest']),
    *[step('desktop.'+name, 'offline', ['{python}','desktop/scripts/test-'+name+'.py'], 'Desktop synthetic harness/policy checks, not device acceptance.') for name in ['candidate-config','release-manifest','smoke-harness','webview2-harness','license-encoding','cross-os-harness']],
    step('desktop.rust', 'native', ['cargo','test','--locked','--manifest-path','desktop/src-tauri/Cargo.toml'], 'Native unit tests; OS SDK required, real credential-store opt-in disabled.'),
    step('desktop.compat', 'docker', ['{python}','desktop/scripts/test-server-compat.py','--image','{image}','--browser-smoke'], 'Frozen server/web/link compatibility using isolated Docker volumes; may fetch baseline source.'),
]

PROFILES = {
    'web': [s['id'] for s in STEPS if s['id'].startswith('web.')],
    'go': [s['id'] for s in STEPS if s['id'].startswith('go.')],
    'policy': [s['id'] for s in STEPS if s['id'].startswith('policy.')] + ['runner'],
    'browser': [s['id'] for s in STEPS if s['id'].startswith('browser.')],
    'browser-webkit': [s['id'] for s in STEPS if s['id'].startswith('webkit.')],
    'docker-core': ['docker.filesystem','docker.admin-recovery'],
    'desktop-unit': ['desktop.web','desktop.unit'] + [s['id'] for s in STEPS if s['id'].startswith('desktop.') and s['category']=='offline' and s['id'] not in ('desktop.web','desktop.unit')],
    'release': ['release.archives','release.server','release.deployment','release.install'],
}
PROFILES['quick'] = PROFILES['web'] + PROFILES['go'] + PROFILES['policy']
# Tier names describe resources; native/signing/manual gates remain explicit.
PROFILES['pr'] = PROFILES['quick']
PROFILES['linux-integration'] = ['race.chat', 'docker.chat-reliability']
PROFILES['release-full'] = PROFILES['quick'] + ['race.chat', 'backup', 'docker.chat-reliability', 'docker.cli-candidate', 'docker.recovery-drill'] + PROFILES['browser'] + PROFILES['browser-webkit'] + PROFILES['desktop-unit'] + PROFILES['release'] + ['release.rollback']

# These require device/OS/installer/credential or provider authorization and
# deliberately have no automatic "all" profile. The original interfaces remain.
EXTERNAL = {
    'scripts/verification_stability.py': 'manual: repeated full PR profile on one Linux runner allocation, no paid models; preserves all samples, not part of default checks',
    'desktop/scripts/test-installed.py': 'native: actual package/previous package, disposable Windows user; installation changes OS state',
    'desktop/scripts/test-installed-harness.py': 'native: explicitly built sidecar path',
    'desktop/scripts/test-linux-sync.py': 'native: isolated peer URL, run ID and native sidecar',
    'desktop/scripts/test-windows-linux.py': 'native: Windows PE sidecar, real WSL2 Linux peer, disposable runner',
    'desktop/scripts/test-update-disk-full.py': 'native: macOS target, isolated disk image and updater test',
    'desktop/scripts/test-webview2-hook.py': 'native: makensis and optional executable fixtures',
    'desktop/scripts/test-webview2-offline.py': 'native/manual: NSIS package and clean offline Windows state',
    'desktop/scripts/test-webview2-package.py': 'artifact: actual Windows package and generated NSIS script',
    'desktop/scripts/test-legacy-browser.mjs': 'browser helper invoked by test-server-compat.py; not a standalone fixture server',
    'scripts/test-remote-browser-live.mjs': 'browser helper invoked by test-remote-browser-live.py; synthetic stdin configuration',
    'desktop/scripts/smoke.py': 'native: explicit Smoke.app/executable; legacy/projects/sync must run serially',
    'internal/agent/appserver_live_test.go': 'paid/manual: CODEX_LIVE_TEST=1 calls the logged-in provider; explicit authorization required',
    'scripts/scan-secrets.py': 'audit: fetched refs, gitleaks and private output directory; raw findings must be reviewed before sharing',
    'internal/server/market_test.go': 'external/manual: MARKET_LIVE_TEST performs external marketplace requests',
    'desktop/src-tauri/src/credentials.rs': 'native/manual: AGENTBOX_CREDENTIAL_TEST=1 touches the OS credential store; isolated runner only',
}

BROWSER_HELPERS = ['feature-lifetimes-browser','actions','chat-footer','diagnostics-browser','image-updates','update-components','mcp','pricing','problems-browser','remote-browser','responsive','skins','themes','switch-account','account-models','term-touch','motion']
COVERED = {f'scripts/test-{name}.mjs':'browser.browser' for name in BROWSER_HELPERS}
COVERED['scripts/test-remote-browser-live.mjs']='docker.remote-browser'
COVERED['desktop/scripts/test-legacy-browser.mjs']='desktop.compat'

# npm wrappers are catalogued here as parents, not duplicated in the self-test.
COVERED.update({f'scripts/test-{name}.mjs': 'web.'+name for name in ['i18n','problems','diagnostics','drafts','outbox','contracts','chat-history','feature-lifetimes']})
