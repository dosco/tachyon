-- wrk2 Lua script: POST 64 KB body to stress the request body forwarding
-- path. At this size chunked encoding and buffer pressure are exercised.
wrk.method = "POST"
wrk.body   = string.rep("x", 65536)
wrk.headers["Content-Type"]   = "application/octet-stream"
wrk.headers["Content-Length"] = "65536"
