// Command origin is a trivial HTTP/1.1 keep-alive responder used as the
// upstream under test for benchmarks.
//
// It intentionally uses net/http (not tachyon's own http1) so the origin
// doesn't become a confound in the proxy's numbers. We want the origin to
// be faster than the proxy at all sizes, which net/http easily is for this
// workload.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
)

func main() {
	addr := flag.String("addr", ":9000", "listen address")
	size := flag.Int("size", 1024, "response body size in bytes")
	flag.Parse()

	body := make([]byte, *size)
	for i := range body {
		body[i] = 'x'
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Method, r.URL.Path)
	})

	log.Printf("origin listening on %s (body=%d)", *addr, *size)
	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}
