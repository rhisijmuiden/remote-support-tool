//! TLS-only connections to the server.
//!
//! Every connection is HTTPS or WSS. The server certificate is checked against the
//! operating system's root store plus the Mozilla roots, or pinned by its SHA-256 when
//! the server has no public certificate. Plain `http://` is refused, except for a
//! loopback address with `insecure_dev` (development only).
use std::sync::Arc;

use anyhow::{Context, Result, anyhow, bail};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::{CryptoProvider, WebPkiSupportedAlgorithms};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};
use sha2::{Digest, Sha256};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::http::HeaderValue;
use tokio_tungstenite::{Connector, MaybeTlsStream, WebSocketStream};
use url::Url;

pub type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// Where and how to reach the server.
#[derive(Clone, Debug)]
pub struct ServerConfig {
    pub url: Url,
    /// SHA-256 of the server's leaf certificate. When set, only that certificate is accepted.
    pub pin_sha256: Option<[u8; 32]>,
    tls: Arc<rustls::ClientConfig>,
}

fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// Parses a pin given as hex, optionally prefixed with `sha256:` and with colons.
pub fn parse_pin(s: &str) -> Result<[u8; 32]> {
    let s = s.trim().trim_start_matches("sha256:").replace(':', "");
    let b = hex::decode(&s).context("certificate pin must be hex")?;
    b.try_into()
        .map_err(|_| anyhow!("certificate pin must be 32 bytes (SHA-256)"))
}

impl ServerConfig {
    /// Validates the server URL. `insecure_dev` allows `http://` to a loopback address only.
    pub fn new(url: &str, pin: Option<&str>, insecure_dev: bool) -> Result<Self> {
        let mut url = Url::parse(url.trim()).context("invalid server URL")?;
        match url.scheme() {
            "https" => {}
            "http" if insecure_dev && is_loopback(&url) => {}
            "http" => bail!("the server URL must start with https:// (all traffic is encrypted)"),
            s => bail!("unsupported server URL scheme {s}"),
        }
        if url.host_str().is_none() {
            bail!("the server URL has no host");
        }
        url.set_path("");
        url.set_query(None);
        url.set_fragment(None);
        let pin_sha256 = pin
            .filter(|p| !p.trim().is_empty())
            .map(parse_pin)
            .transpose()?;
        let tls = Arc::new(tls_config(pin_sha256)?);
        Ok(Self {
            url,
            pin_sha256,
            tls,
        })
    }

    pub fn https(&self, path: &str) -> String {
        format!("{}{}", self.url.as_str().trim_end_matches('/'), path)
    }

    pub fn wss(&self, path: &str) -> String {
        let base = self.url.as_str().trim_end_matches('/');
        let base = base
            .strip_prefix("https")
            .map(|r| format!("wss{r}"))
            .unwrap_or_else(|| base.replacen("http", "ws", 1));
        format!("{base}{path}")
    }

    pub fn tls(&self) -> Arc<rustls::ClientConfig> {
        self.tls.clone()
    }

    /// An HTTP client that uses the same TLS settings as the WebSocket connections.
    pub fn http_client(&self) -> Result<reqwest::Client> {
        Ok(reqwest::Client::builder()
            .use_preconfigured_tls((*self.tls).clone())
            .https_only(self.url.scheme() == "https")
            .user_agent(concat!("rhis-agent/", env!("CARGO_PKG_VERSION")))
            .timeout(std::time::Duration::from_secs(30))
            .build()?)
    }

    /// Opens a WebSocket to `path` with a bearer token.
    pub async fn connect_ws(&self, path: &str, token: &str) -> Result<Ws> {
        let mut req = self.wss(path).into_client_request()?;
        req.headers_mut().insert(
            "Authorization",
            HeaderValue::from_str(&format!("Bearer {token}"))?,
        );
        req.headers_mut().insert(
            "User-Agent",
            HeaderValue::from_static(concat!("rhis-agent/", env!("CARGO_PKG_VERSION"))),
        );
        let connector = if self.url.scheme() == "https" {
            Some(Connector::Rustls(self.tls.clone()))
        } else {
            None
        };
        let (ws, _) = tokio::time::timeout(
            std::time::Duration::from_secs(20),
            tokio_tungstenite::connect_async_tls_with_config(req, None, true, connector),
        )
        .await
        .map_err(|_| anyhow!("connection timed out"))??;
        Ok(ws)
    }
}

fn is_loopback(url: &Url) -> bool {
    match url.host() {
        Some(url::Host::Ipv4(ip)) => ip.is_loopback(),
        Some(url::Host::Ipv6(ip)) => ip.is_loopback(),
        Some(url::Host::Domain(d)) => d == "localhost",
        None => false,
    }
}

fn tls_config(pin: Option<[u8; 32]>) -> Result<rustls::ClientConfig> {
    let prov = provider();
    let builder = rustls::ClientConfig::builder_with_provider(prov.clone())
        .with_safe_default_protocol_versions()?;
    let cfg = match pin {
        Some(pin) => builder
            .dangerous()
            .with_custom_certificate_verifier(Arc::new(PinnedVerifier {
                pin,
                algs: prov.signature_verification_algorithms,
            }))
            .with_no_client_auth(),
        None => {
            let mut roots = rustls::RootCertStore::empty();
            roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
            // The OS store adds company CAs (for example an internal PKI).
            for c in rustls_native_certs::load_native_certs().certs {
                let _ = roots.add(c);
            }
            builder.with_root_certificates(roots).with_no_client_auth()
        }
    };
    Ok(cfg)
}

/// Accepts exactly one certificate, identified by its SHA-256. The handshake signature is still verified,
/// so a party without the certificate's private key cannot impersonate the server.
#[derive(Debug)]
struct PinnedVerifier {
    pin: [u8; 32],
    algs: WebPkiSupportedAlgorithms,
}

impl ServerCertVerifier for PinnedVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let got: [u8; 32] = Sha256::digest(end_entity.as_ref()).into();
        if got == self.pin {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(rustls::Error::General(
                "server certificate does not match the pinned SHA-256".into(),
            ))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(message, cert, dss, &self.algs)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(message, cert, dss, &self.algs)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.algs.supported_schemes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn refuses_plain_http() {
        assert!(ServerConfig::new("http://remote.example.com", None, false).is_err());
        assert!(ServerConfig::new("http://remote.example.com", None, true).is_err());
        assert!(ServerConfig::new("http://127.0.0.1:8080", None, false).is_err());
        assert!(ServerConfig::new("http://127.0.0.1:8080", None, true).is_ok());
        assert!(ServerConfig::new("ftp://x", None, true).is_err());
    }

    #[test]
    fn urls() {
        let c = ServerConfig::new("https://remote.example.com/some/path?x=1", None, false).unwrap();
        assert_eq!(
            c.wss("/api/agent/v1/control"),
            "wss://remote.example.com/api/agent/v1/control"
        );
        assert_eq!(
            c.https("/api/agent/v1/enroll"),
            "https://remote.example.com/api/agent/v1/enroll"
        );
        let d = ServerConfig::new("http://localhost:8080", None, true).unwrap();
        assert_eq!(d.wss("/x"), "ws://localhost:8080/x");
    }

    #[test]
    fn pins() {
        let hexpin = "AB".repeat(32);
        assert_eq!(parse_pin(&format!("sha256:{hexpin}")).unwrap(), [0xab; 32]);
        assert!(parse_pin("abcd").is_err());
    }
}
