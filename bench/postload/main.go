// postload: minimal POST-body load generator.
// Usage: postload -url http://127.0.0.1:8080/ -size 65536 -c 64 -dur 30s
// Emits one JSON line at the end: {"rps":NNN,"done":N,"errs":N}.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/", "target URL")
	size := flag.Int("size", 65536, "POST body size in bytes")
	conc := flag.Int("c", 64, "concurrency")
	dur := flag.Duration("dur", 30*time.Second, "test duration")
	flag.Parse()

	body := bytes.Repeat([]byte{'x'}, *size)

	tr := &http.Transport{
		MaxIdleConnsPerHost: *conc,
		MaxConnsPerHost:     *conc,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	var done, errs atomic.Int64
	stop := time.After(*dur)
	doneCh := make(chan struct{})

	for i := 0; i < *conc; i++ {
		go func() {
			for {
				select {
				case <-doneCh:
					return
				default:
				}
				req, _ := http.NewRequest("POST", *url, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/octet-stream")
				req.ContentLength = int64(*size)
				resp, err := client.Do(req)
				if err != nil {
					errs.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done.Add(1)
			}
		}()
	}
	<-stop
	close(doneCh)
	// Short grace for in-flight.
	time.Sleep(200 * time.Millisecond)

	d := done.Load()
	e := errs.Load()
	rps := float64(d) / dur.Seconds()
	fmt.Printf(`{"rps":%.0f,"done":%d,"errs":%d}`+"\n", rps, d, e)
}
