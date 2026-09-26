// Synthetic target and upstream proxy for Docker acceptance tests. No real API calls.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
)

func main() {
	listen := flag.String("listen", ":8080", "")
	marker := flag.String("marker", "fixture", "")
	proxy := flag.Bool("proxy", false, "")
	flag.Parse()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, *marker) })
	if *proxy {
		go http.ListenAndServe("127.0.0.1:8089", handler)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "CONNECT" {
				fmt.Fprint(w, *marker)
				return
			}
			up, err := net.Dial("tcp", "127.0.0.1:8089")
			if err != nil {
				http.Error(w, "fixture", 502)
				return
			}
			defer up.Close()
			down, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer down.Close()
			down.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
			go io.Copy(up, buf)
			io.Copy(down, up)
		})
	}
	if err := http.ListenAndServe(*listen, handler); err != nil {
		panic(err)
	}
}
