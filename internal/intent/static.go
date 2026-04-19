package intent

const Grammar = `intent_version "0.1"

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
    request.header "x-api-key", "k1"
    request.query "q", "search"
    request.cookie "session", "abc"
    expect.status 451
    expect.body "blocked"
    expect.request_header "x-proxy", "tachyon"
    expect.response_header "x-served-by", "tachyon"
  }
  budget {
    request.allocs <= 4
    response.allocs <= 4
  }
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

const Example = `intent_version "0.1"

policy sample {
  priority 100
  match req.host == "example.com" && req.path.has_prefix("/api/")

  request {
    set_header("x-proxy", "tachyon")
    route_to("pool-a")
  }

  response {
    set_header("x-served-by", "tachyon")
  }

  case happy_path {
    request.method "GET"
    request.host "example.com"
    request.path "/api/users"
    expect.upstream "pool-a"
    expect.request_header "x-proxy", "tachyon"
    expect.response_header "x-served-by", "tachyon"
  }

  budget {
    request.allocs <= 4
    response.allocs <= 4
  }
}`

func Scaffold(name string) string {
	if name == "" {
		name = "sample"
	}
	return `intent_version "0.1"

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
}`
}
