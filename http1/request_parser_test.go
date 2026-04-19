package http1

import (
	"bytes"
	"testing"
)

// mustParse parses src and fails the test if Parse returns an error.
func mustParse(t *testing.T, src []byte) Request {
	t.Helper()
	var r Request
	n, err := Parse(src, &r)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	if n <= 0 || n > len(src) {
		t.Fatalf("Parse returned n=%d; want in (0,%d]", n, len(src))
	}
	return r
}

// TestParseBasicGET is the smoke test: a minimal GET parses cleanly,
// n points past the CRLF CRLF, and the common fields are populated.
func TestParseBasicGET(t *testing.T) {
	src := []byte("GET /foo HTTP/1.1\r\nHost: x\r\n\r\n")
	r := mustParse(t, src)
	if got := string(r.MethodBytes()); got != "GET" {
		t.Fatalf("method: got %q want GET", got)
	}
	if got := string(r.PathBytes()); got != "/foo" {
		t.Fatalf("path: got %q want /foo", got)
	}
	if r.Minor != 1 {
		t.Fatalf("minor: got %d want 1", r.Minor)
	}
	if host := r.Lookup(HdrHost); !bytes.Equal(host, []byte("x")) {
		t.Fatalf("host lookup: got %q want x", host)
	}
}

// TestLookupExpect covers the Phase 1.I header: the handler reads it
// immediately after Parse to decide whether to send a 100-continue
// interim response.
func TestLookupExpect(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"expect present lowercase",
			"POST /u HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 0\r\n\r\n",
			"100-continue",
		},
		{
			"expect case-insensitive",
			"POST /u HTTP/1.1\r\nHost: x\r\nEXPECT: 100-continue\r\nContent-Length: 0\r\n\r\n",
			"100-continue",
		},
		{
			"expect missing",
			"POST /u HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustParse(t, []byte(c.src))
			got := r.Lookup(HdrExpect)
			if string(got) != c.want {
				t.Fatalf("Lookup(Expect): got %q want %q", got, c.want)
			}
			if c.want != "" && !EqualFold(got, Value100Continue) {
				t.Fatalf("EqualFold(%q, Value100Continue) = false; want true", got)
			}
		})
	}
}

// TestEqualFold is the host of the Expect check. Miss on this and we
// either always send 100, always never send it, or misclassify when the
// client varies caps.
func TestEqualFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"100-continue", "100-continue", true},
		{"100-CONTINUE", "100-continue", true},
		{"100-Continue", "100-continue", true},
		{"100continue", "100-continue", false},
		{"", "", true},
		{"", "a", false},
		{"a", "", false},
		{"Expect", "expect", true},
		{"Exp3ct", "expect", false},
	}
	for _, c := range cases {
		if got := EqualFold([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("EqualFold(%q,%q): got %v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestParseContentLengthAndChunked checks the derived flags that the
// handler uses to decide body framing. POST-with-body scenarios rely on
// these being right.
func TestParseContentLengthAndChunked(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		wantCL    int64
		wantChunk bool
	}{
		{
			"content-length",
			"POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 42\r\n\r\n",
			42, false,
		},
		{
			"chunked",
			"POST /x HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n",
			-1, true,
		},
		{
			"no body declarations",
			"GET /x HTTP/1.1\r\nHost: x\r\n\r\n",
			-1, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustParse(t, []byte(c.src))
			if r.ContentLength != c.wantCL {
				t.Errorf("ContentLength: got %d want %d", r.ContentLength, c.wantCL)
			}
			if r.Chunked != c.wantChunk {
				t.Errorf("Chunked: got %v want %v", r.Chunked, c.wantChunk)
			}
		})
	}
}

// TestParseNeedsMore: a truncated header block returns ErrNeedMore so
// the handler reads more bytes.
func TestParseNeedsMore(t *testing.T) {
	var r Request
	_, err := Parse([]byte("GET /x HTTP/1.1\r\nHost: x\r\n"), &r)
	if err != ErrNeedMore {
		t.Fatalf("got %v want ErrNeedMore", err)
	}
}

// TestResponse100ContinueIsValid asserts the pre-encoded interim
// response is a real HTTP/1.1 100 line. This guards against a typo in
// the constant.
func TestResponse100ContinueIsValid(t *testing.T) {
	got := string(Response100Continue)
	want := "HTTP/1.1 100 Continue\r\n\r\n"
	if got != want {
		t.Fatalf("Response100Continue:\n  got  %q\n  want %q", got, want)
	}
}
