// SPDX-License-Identifier: AGPL-3.0-only
//! English and Dutch texts. English is the default; Dutch is picked when the
//! Windows display language is Dutch, or with `--lang nl`.

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Lang {
    En,
    Nl,
}

impl Lang {
    pub fn detect(explicit: Option<&str>) -> Lang {
        let l = explicit
            .map(str::to_string)
            .or_else(sys_locale::get_locale)
            .unwrap_or_default()
            .to_ascii_lowercase();
        if l.starts_with("nl") {
            Lang::Nl
        } else {
            Lang::En
        }
    }

    pub fn code(self) -> &'static str {
        match self {
            Lang::En => "en",
            Lang::Nl => "nl",
        }
    }
}

/// (key, English, Dutch). `{0}`, `{1}` are replaced by arguments.
const TEXTS: &[(&str, &str, &str)] = &[
    ("title", "QuickSupport", "QuickSupport"),
    (
        "connecting",
        "Connecting to the support server…",
        "Verbinden met de supportserver…",
    ),
    ("your_code", "Your session code", "Je sessiecode"),
    (
        "give_code",
        "Give this code to your technician at {0}.",
        "Geef deze code aan je technicus van {0}.",
    ),
    (
        "privacy",
        "Nobody can see or control this computer until you click Allow.",
        "Niemand kan deze computer zien of bedienen totdat jij op Toestaan klikt.",
    ),
    (
        "ready",
        "Ready for your technician",
        "Klaar voor je technicus",
    ),
    ("copy", "Copy", "Kopiëren"),
    ("copied", "Copied", "Gekopieerd"),
    (
        "request_title",
        "Allow remote support?",
        "Hulp op afstand toestaan?",
    ),
    (
        "request_body",
        "{0} from {1} wants to view and control this computer.",
        "{0} van {1} wil deze computer bekijken en bedienen.",
    ),
    (
        "request_rec",
        "This session will be recorded.",
        "Deze sessie wordt opgenomen.",
    ),
    ("allow", "Allow", "Toestaan"),
    ("decline", "Decline", "Weigeren"),
    (
        "answer_in",
        "Declines automatically in {0} s",
        "Wordt over {0} s automatisch geweigerd",
    ),
    (
        "active_title",
        "Remote support is active",
        "Hulp op afstand is actief",
    ),
    (
        "active_body",
        "{0} can see and control your screen.",
        "{0} kan je scherm zien en bedienen.",
    ),
    ("recording", "Recording", "Opname"),
    ("end_session", "End session", "Sessie beëindigen"),
    ("ended", "The session has ended.", "De sessie is beëindigd."),
    (
        "declined",
        "You declined the request.",
        "Je hebt het verzoek geweigerd.",
    ),
    (
        "cancelled",
        "The technician cancelled the request.",
        "De technicus heeft het verzoek ingetrokken.",
    ),
    (
        "offline",
        "No connection to the server. Trying again in {0} s.",
        "Geen verbinding met de server. Opnieuw proberen over {0} s.",
    ),
    ("error", "Something went wrong", "Er ging iets mis"),
    ("retry", "Try again", "Opnieuw proberen"),
    (
        "no_server",
        "This QuickSupport has no server configured. Download it again from your support provider.",
        "Deze QuickSupport heeft geen server ingesteld. Download hem opnieuw bij je supportleverancier.",
    ),
    (
        "not_https",
        "The server address must start with https://, so the connection is encrypted.",
        "Het serveradres moet met https:// beginnen, zodat de verbinding versleuteld is.",
    ),
    (
        "engine_missing",
        "This build does not include the remote control engine.",
        "Deze versie bevat de remote-control-module niet.",
    ),
    (
        "encrypted",
        "Encrypted connection",
        "Versleutelde verbinding",
    ),
    (
        "unencrypted_dev",
        "Not encrypted (development)",
        "Niet versleuteld (ontwikkeling)",
    ),
    (
        "close_hint",
        "Close this window to end support. Nothing stays installed.",
        "Sluit dit venster om de hulp te stoppen. Er blijft niets geïnstalleerd.",
    ),
    ("info", "Info", "Info"),
    ("version", "Version", "Versie"),
    ("server", "Server", "Server"),
    ("license", "License", "Licentie"),
    ("source", "Source code", "Broncode"),
    (
        "check_updates",
        "Check for updates",
        "Controleren op updates",
    ),
    ("checking", "Checking…", "Controleren…"),
    (
        "up_to_date",
        "You have the latest version.",
        "Je hebt de nieuwste versie.",
    ),
    (
        "update_available",
        "Version {0} is available.",
        "Versie {0} is beschikbaar.",
    ),
    (
        "download_update",
        "Download and restart",
        "Downloaden en herstarten",
    ),
    ("downloading", "Downloading…", "Downloaden…"),
    (
        "update_failed",
        "Could not check for updates: {0}",
        "Controleren op updates mislukt: {0}",
    ),
    ("close", "Close", "Sluiten"),
    (
        "demo",
        "Demo mode: no connection is made.",
        "Demomodus: er wordt geen verbinding gemaakt.",
    ),
];

pub fn t(lang: Lang, key: &str) -> &'static str {
    TEXTS
        .iter()
        .find(|(k, _, _)| *k == key)
        .map(|(_, en, nl)| if lang == Lang::Nl { *nl } else { *en })
        .unwrap_or("?")
}

pub fn tf(lang: Lang, key: &str, args: &[&str]) -> String {
    let mut s = t(lang, key).to_string();
    for (i, a) in args.iter().enumerate() {
        s = s.replace(&format!("{{{i}}}"), a);
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_keys_translated_and_unique() {
        let mut seen = std::collections::HashSet::new();
        for (k, en, nl) in TEXTS {
            assert!(seen.insert(*k), "duplicate key {k}");
            assert!(!en.is_empty() && !nl.is_empty(), "empty text for {k}");
            assert_eq!(
                en.matches('{').count(),
                nl.matches('{').count(),
                "placeholders differ for {k}"
            );
        }
    }

    #[test]
    fn format() {
        assert_eq!(
            tf(Lang::Nl, "request_body", &["Jan", "RHIS"]),
            "Jan van RHIS wil deze computer bekijken en bedienen."
        );
        assert_eq!(Lang::detect(Some("nl-NL")), Lang::Nl);
        assert_eq!(Lang::detect(Some("en-US")), Lang::En);
    }
}
