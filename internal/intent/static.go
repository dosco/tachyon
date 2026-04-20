package intent

const Grammar = `intent_version "0.2"

listener {
  addr ":8080"
}

tls {
  listen ":8443"
  cert   "./certs/prod.pem"   # resolved relative to this file
  key    "./certs/prod.key"
}

quic {                         # optional HTTP/3 listener (shares cert/key with tls by default)
  listen ":8443"
  alpn   "h3"
}

pool NAME {
  addrs "host:port", "host:port"
  idle_per_host   256
  connect_timeout "1s"
  lb_policy       "rr" | "p2c_ewma"

  outlier_detection {
    consecutive_5xx            5
    consecutive_gateway_errors 5
    ejection_duration          "30s"
    max_ejected_percent        50
  }
  health_check {
    interval "10s"
    path     "/health"
    timeout  "1s"
  }
  retry_budget {
    retry_percent 20
    min_tokens    3
  }
}

route NAME {
  host     "api.example.com"
  path     "/"
  upstream "pool_name"             # single upstream, OR
  upstreams "pool_a" weight 95, "pool_b" weight 5
  apply    "policy_a", "policy_b"  # optional; apply named policies
}

policy NAME {
  priority INT
  match EXPR
  request  { ACTION... }
  response { ACTION... }
  error    { ACTION... }
  case NAME {
    request.method "GET"
    request.host "example.com"
    request.path "/"
    expect.status 200
    expect.body "ok"
  }
  budget {
    request.allocs <= 4
    response.allocs <= 4
  }
}

case NAME {   # top-level topology case
  request.method "GET"
  request.host   "api.example.com"
  request.path   "/v1/users"
  expect.route    "api_v1"
  expect.upstream "api"
  expect.applied  "add_request_id"
  expect.status 200
}

Match expressions (combinable with &&):
  req.host == "example.com"
  req.method == "GET"
  req.path == "/exact"
  req.path.has_prefix("/api/")
  req.path.has_suffix(".json")
  req.header("x-api-key") == "secret"
  req.query("debug") == "1"
  req.cookie("session") == "abc"
  req.ip == "10.0.0.1"`

const Primitives = `respond
deny
redirect
route_to
canary
set_header
remove_header
strip_prefix
add_prefix
rate_limit_local
auth_external
log
emit_metric`

const Example = `intent_version "0.2"

listener { addr ":8080" }

pool api {
  addrs "127.0.0.1:9000"
}

policy add_request_id {
  priority 100
  match req.host == "api.example.com"
  request { set_header("x-request-id", "gen") }
}

route api_v1 {
  host "api.example.com"
  path "/"
  upstream "api"
  apply "add_request_id"
}

case api_v1_routes_to_api_pool {
  request.method "GET"
  request.host   "api.example.com"
  request.path   "/v1/users"
  expect.route    "api_v1"
  expect.upstream "api"
  expect.applied  "add_request_id"
}`

func Scaffold(name string) string {
	if name == "" {
		name = "sample"
	}
	return `intent_version "0.2"

listener { addr ":8080" }

pool origin {
  addrs "127.0.0.1:9000"
}

policy ` + name + ` {
  priority 100
  match req.host == "example.com"

  request {
    set_header("x-proxy", "tachyon")
  }

  response {
    set_header("x-served-by", "tachyon")
  }

  case happy_path {
    request.method "GET"
    request.host "example.com"
    request.path "/"
    expect.request_header "x-proxy", "tachyon"
    expect.response_header "x-served-by", "tachyon"
  }

  budget {
    request.allocs <= 4
    response.allocs <= 4
  }
}

route default {
  host "example.com"
  path "/"
  upstream "origin"
  apply "` + name + `"
}`
}
