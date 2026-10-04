//! Native credential identity stays independent of renderer preferences. Tests
//! use a separate service and fresh synthetic identities, never real logins.
use crate::remote::{Error, Result};
use sha2::{Digest, Sha256};

const SERVICE: &str = "io.github.devilcoolyue.agentbox.desktop";

fn identity(server: &str, user: &str) -> String {
    let pair = serde_json::to_vec(&(server, user)).expect("string pair");
    format!("{:x}", Sha256::digest(pair))
}

pub(crate) fn credential(server: &str, user: &str) -> Result<keyring::Entry> {
    keyring::Entry::new(SERVICE, &identity(server, user))
        .map_err(|_| Error::new("credential", "无法访问系统凭证库；不会回退为明文保存"))
}

#[cfg(all(test, any(target_os = "macos", windows)))]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    // Opt-in because this deliberately exercises the OS store. Every entry is
    // isolated from the application's service and removed even after assertion
    // failure; no real credential is read, changed or revoked.
    #[test]
    fn native_credentials_persist_isolate_and_delete() {
        if std::env::var("AGENTBOX_CREDENTIAL_TEST").as_deref() != Ok("1") {
            eprintln!("native credential test not requested; set AGENTBOX_CREDENTIAL_TEST=1");
            return;
        }
        const VALIDATION_SERVICE: &str = "io.github.devilcoolyue.agentbox.desktop.validation";
        let stamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let user = format!("fixture-{}-{stamp}", std::process::id());
        let other_user = format!("{user}-other");
        let specs = [
            ("https://agentbox.invalid/one/", user.as_str()),
            ("https://agentbox.invalid/two/", user.as_str()),
            ("https://agentbox.invalid/one/", other_user.as_str()),
        ];
        let mut created = Cleanup(Vec::new());
        for (index, (server, username)) in specs.iter().enumerate() {
            let key = identity(server, username);
            let entry = keyring::Entry::new(VALIDATION_SERVICE, &key).unwrap();
            assert!(matches!(entry.get_password(), Err(keyring::Error::NoEntry)));
            // Record ownership only after confirming the identity is unused.
            created.0.push(key.clone());
            entry
                .set_password(&format!("synthetic-token-{index}"))
                .unwrap();
            drop(entry);
            let reopened = keyring::Entry::new(VALIDATION_SERVICE, &key).unwrap();
            assert_eq!(
                reopened.get_password().unwrap(),
                format!("synthetic-token-{index}")
            );
        }
        for (index, key) in created.0.iter().enumerate() {
            let entry = keyring::Entry::new(VALIDATION_SERVICE, key).unwrap();
            assert_eq!(
                entry.get_password().unwrap(),
                format!("synthetic-token-{index}")
            );
            entry.delete_credential().unwrap();
            assert!(matches!(entry.get_password(), Err(keyring::Error::NoEntry)));
        }
        eprintln!("native credential persistence, server/user isolation and deletion passed");

        struct Cleanup(Vec<String>);
        impl Drop for Cleanup {
            fn drop(&mut self) {
                for key in &self.0 {
                    if let Ok(entry) = keyring::Entry::new(VALIDATION_SERVICE, key) {
                        let _ = entry.delete_credential();
                    }
                }
            }
        }
    }
}
