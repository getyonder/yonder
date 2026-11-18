// Command yonder-stun is Yonder's STUN server (internal/stun): it tells a
// device the public address its request came from, which a browser and the
// desktop app need before a data channel. It runs beside the relay, whose
// own STUN on UDP 3478 answers only Tailscale's code. It reads no file,
// writes none, and logs no address: only, each hour, how many requests it
// answered and how many it did not.
package main

import (
	"flag"
	"log"
	"net"
	"time"

	"yonder/internal/stun"
)

func main() {
	var (
		listen = flag.String("listen", ":3479", "UDP address to listen on")
		every  = flag.Duration("log", time.Hour, "how often the counts are logged, if there are any")
	)
	flag.Parse()
	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("yonder-stun: listening on UDP %s", conn.LocalAddr())
	s := stun.NewServer()
	go func() {
		for range time.Tick(*every) {
			if answered, refused := s.Counts(); answered+refused > 0 {
				log.Printf("yonder-stun: %d answered, %d not answered", answered, refused)
			}
		}
	}()
	log.Fatal(s.Serve(conn))
}
