/// Minimal Pingora reverse proxy for the tachyon benchmark.
///
/// Listens on :8080, forwards every request to 127.0.0.1:9000.
///
/// Production-realistic tuning:
///   - threads = num_cpus                (match nginx's worker_processes auto)
///   - work_stealing enabled             (Tokio default, but explicit)
///   - upstream peer uses HTTP/1.1 w/ connection reuse (HttpPeer default)
///
/// Build:
///   cargo build --release
use async_trait::async_trait;
use pingora_core::server::configuration::{Opt, ServerConf};
use pingora_core::server::Server;
use pingora_core::upstreams::peer::HttpPeer;
use pingora_core::Result;
use pingora_proxy::{ProxyHttp, Session};

/// The proxy service: every request goes to 127.0.0.1:9000.
pub struct ReverseProxy;

#[async_trait]
impl ProxyHttp for ReverseProxy {
    type CTX = ();

    fn new_ctx(&self) -> Self::CTX {}

    async fn upstream_peer(
        &self,
        _session: &mut Session,
        _ctx: &mut Self::CTX,
    ) -> Result<Box<HttpPeer>> {
        // HttpPeer::new defaults enable keepalive with idle_timeout = 60s.
        let peer = HttpPeer::new("127.0.0.1:9000", false, String::new());
        Ok(Box::new(peer))
    }
}

fn main() {
    // Let CLI args override, but set sensible production defaults otherwise.
    let opt = Opt::parse_args();
    let mut server = Server::new_with_opt_and_conf(Some(opt), {
        let mut c = ServerConf::default();
        // One Tokio worker thread per logical CPU, matching nginx
        // worker_processes=auto. Pingora normally defaults to num_cpus,
        // but we set it explicitly so the bench is reproducible.
        c.threads = num_cpus::get();
        // Upstream connection pool size (per-worker). Matches nginx's
        // `keepalive 512` upstream block.
        c.upstream_keepalive_pool_size = 512;
        c
    });
    server.bootstrap();

    let mut proxy =
        pingora_proxy::http_proxy_service(&server.configuration, ReverseProxy);
    proxy.add_tcp("0.0.0.0:8080");
    server.add_service(proxy);

    server.run_forever();
}
