-- wrk2 Lua script: POST 1 KB body to simulate a small API request.
wrk.method = "POST"
wrk.body   = string.rep("x", 1024)
wrk.headers["Content-Type"]   = "application/octet-stream"
wrk.headers["Content-Length"] = "1024"
