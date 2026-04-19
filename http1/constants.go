package http1

// Pre-encoded byte strings. Stored as package-level []byte so they live in
// .rodata and can be appended without allocation. Consumers must not mutate.

var (
	CRLF     = []byte("\r\n")
	CRLFCRLF = []byte("\r\n\r\n")
	ColonSP  = []byte(": ")

	HTTP11 = []byte("HTTP/1.1")

	// Methods we recognise by identity. The parser matches by length first,
	// then byte-compares. Unknown methods are passed through as a Span.
	MethodGET     = []byte("GET")
	MethodHEAD    = []byte("HEAD")
	MethodPOST    = []byte("POST")
	MethodPUT     = []byte("PUT")
	MethodDELETE  = []byte("DELETE")
	MethodOPTIONS = []byte("OPTIONS")
	MethodPATCH   = []byte("PATCH")
	MethodCONNECT = []byte("CONNECT")
	MethodTRACE   = []byte("TRACE")

	// Common header names we look up. Kept lowercase because our scanner
	// compares case-insensitively using a lowercase target.
	HdrHost            = []byte("host")
	HdrContentLength   = []byte("content-length")
	HdrTransferEncode  = []byte("transfer-encoding")
	HdrConnection      = []byte("connection")
	HdrKeepAlive       = []byte("keep-alive")
	HdrUpgrade         = []byte("upgrade")
	HdrProxyConnection = []byte("proxy-connection")
	HdrTE              = []byte("te")
	HdrTrailer         = []byte("trailer")
	HdrXForwardedFor   = []byte("x-forwarded-for")
	HdrExpect          = []byte("expect")

	ValueChunked      = []byte("chunked")
	ValueClose        = []byte("close")
	Value100Continue  = []byte("100-continue")

	// Response100Continue is the interim response a server writes before
	// reading the body when the client asked for Expect: 100-continue.
	// Pre-encoded so handlers can write it with a single syscall and no
	// allocation.
	Response100Continue = []byte("HTTP/1.1 100 Continue\r\n\r\n")
)

// StatusLine returns the pre-encoded status line for common codes. Fast
// path for the writer; rare codes fall back to formatted output.
func StatusLine(code int) []byte {
	switch code {
	case 200:
		return []byte("HTTP/1.1 200 OK\r\n")
	case 204:
		return []byte("HTTP/1.1 204 No Content\r\n")
	case 301:
		return []byte("HTTP/1.1 301 Moved Permanently\r\n")
	case 302:
		return []byte("HTTP/1.1 302 Found\r\n")
	case 304:
		return []byte("HTTP/1.1 304 Not Modified\r\n")
	case 400:
		return []byte("HTTP/1.1 400 Bad Request\r\n")
	case 404:
		return []byte("HTTP/1.1 404 Not Found\r\n")
	case 500:
		return []byte("HTTP/1.1 500 Internal Server Error\r\n")
	case 502:
		return []byte("HTTP/1.1 502 Bad Gateway\r\n")
	case 503:
		return []byte("HTTP/1.1 503 Service Unavailable\r\n")
	case 504:
		return []byte("HTTP/1.1 504 Gateway Timeout\r\n")
	}
	return nil
}

// MaxHeaders is the hard cap on headers per message. Exceeded → 431. Keeping
// this bounded lets us use a fixed-size header array on Request and Response.
const MaxHeaders = 64
