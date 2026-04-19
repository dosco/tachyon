package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	addr := "127.0.0.1:19090"
	log.Printf("example origin listening on http://%s", addr)
	if err := http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin-Seen-Proxy", r.Header.Get("X-Example-Proxy"))
		fmt.Fprintf(w, "origin path=%s", r.URL.Path)
	})); err != nil {
		log.Fatal(err)
	}
}
