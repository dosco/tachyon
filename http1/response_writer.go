package http1

import "strconv"

// AppendStatus writes "HTTP/1.1 <code> <reason>\r\n" to dst and returns the
// extended slice. Uses the pre-encoded StatusLine table for common codes and
// falls back to a formatted build for rare ones. No heap use.
func AppendStatus(dst []byte, code int) []byte {
	if line := StatusLine(code); line != nil {
		return append(dst, line...)
	}
	dst = append(dst, HTTP11...)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, int64(code), 10)
	dst = append(dst, ' ', 'O', 'K') // generic reason; most clients ignore it
	dst = append(dst, CRLF...)
	return dst
}

// AppendHeader writes "<name>: <value>\r\n" to dst. name and value are
// appended verbatim; the caller is responsible for validity.
func AppendHeader(dst, name, value []byte) []byte {
	dst = append(dst, name...)
	dst = append(dst, ColonSP...)
	dst = append(dst, value...)
	dst = append(dst, CRLF...)
	return dst
}

// AppendContentLength writes "Content-Length: <n>\r\n" into dst.
func AppendContentLength(dst []byte, n int64) []byte {
	dst = append(dst, "Content-Length: "...)
	dst = strconv.AppendInt(dst, n, 10)
	dst = append(dst, CRLF...)
	return dst
}

// AppendEndOfHeaders writes the blank CRLF that terminates the header section.
func AppendEndOfHeaders(dst []byte) []byte {
	return append(dst, CRLF...)
}

// AppendRequestLine writes "<method> <path> HTTP/1.1\r\n".
func AppendRequestLine(dst, method, path []byte) []byte {
	dst = append(dst, method...)
	dst = append(dst, ' ')
	dst = append(dst, path...)
	dst = append(dst, ' ')
	dst = append(dst, HTTP11...)
	dst = append(dst, CRLF...)
	return dst
}
