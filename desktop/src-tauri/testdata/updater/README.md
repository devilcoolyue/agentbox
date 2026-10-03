# Synthetic updater signatures

These bytes are test fixtures, not an installable archive or a release key.
Generated using the repository's pinned Tauri CLI `signer generate --ci` and
`signer sign --app-version 9.0.0` (versioned.sig), then without `--app-version`
(legacy.sig). The ephemeral private key was deleted; only its public key remains.

The tests run the actual updater plugin's HTTP download and minisign verification.
The Tauri runtime is mocked. They never install an update, access the keychain,
or contact GitHub. Installed upgrades and credential retention need separate
platform acceptance tests.
