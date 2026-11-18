//go:build ignore

// serve gives the files of a folder to a browser on 127.0.0.1:8467, prints
// what the page posts to /done, and ends. See run.sh.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	done := make(chan bool)
	http.Handle("/", http.FileServer(http.Dir(os.Args[1])))
	http.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Println(string(b))
		close(done)
	})
	go func() {
		fmt.Fprintln(os.Stderr, http.ListenAndServe("127.0.0.1:8467", nil))
		os.Exit(2)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		fmt.Println("no result from the browser")
		os.Exit(1)
	}
}
