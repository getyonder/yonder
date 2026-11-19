//go:build !js

package direct

import (
	"context"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// A browser gives its address on the local network as a name that ends in
// ".local", which only a question to that network (mDNS) turns into the
// address. Without the address this end cannot send to the browser first,
// and a firewall here that lets in only answers then drops every check
// the browser sends: no path forms between an app and a browser on one
// network.
//
// pion can ask, but it asks from a port of its own. A device answers such
// a question to that port alone, and the same firewall drops the answer.
// So the question is asked here from the mDNS port, 5353: the answer then
// goes to the whole network, which a firewall lets in (ufw does by its
// first rules). pion's own mDNS stays off.
//
// What is read from the network is small: packets to the mDNS group,
// only while a description is taken in, parsed by Go's DNS parser, and
// of them only an IPv4 address for a name that was asked for.

const (
	// localWait is the longest that the names of one description are
	// waited for. A device on the network answers in well under a second.
	localWait = 1500 * time.Millisecond
	// localAgain is the time between two askings; a packet can be lost.
	localAgain = 400 * time.Millisecond
	// maxLocalNames bounds the names asked for from one description.
	maxLocalNames = 8
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// localCandidate finds the name in a candidate line of a description:
// "candidate:1 1 udp 2113937151 0a1b….local 49862 typ host".
var localCandidate = regexp.MustCompile(`(?i)(candidate:\S+ \d+ udp \d+ )([0-9a-z-]{1,63}\.local)( \d+ typ host)`)

// localNames gives d with the address in place of each ".local" name that
// the local network answered for. A name with no answer stays, and pion
// passes it over.
func localNames(ctx context.Context, d webrtc.SessionDescription) webrtc.SessionDescription {
	d.SDP = withLocalNames(d.SDP, func(names []string) map[string]net.IP {
		ctx, cancel := context.WithTimeout(ctx, localWait)
		defer cancel()
		return askLocal(ctx, names)
	})
	return d
}

// withLocalNames is localNames for the text of a description; ask gives
// the address of each name that it found. It is not called when the
// description has no ".local" name.
func withLocalNames(sdp string, ask func(names []string) map[string]net.IP) string {
	var names []string
	for _, m := range localCandidate.FindAllStringSubmatch(sdp, -1) {
		if name := strings.ToLower(m[2]); !slices.Contains(names, name) && len(names) < maxLocalNames {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return sdp
	}
	found := ask(names)
	return localCandidate.ReplaceAllStringFunc(sdp, func(line string) string {
		m := localCandidate.FindStringSubmatch(line)
		if ip, ok := found[strings.ToLower(m[2])]; ok {
			return m[1] + ip.String() + m[3]
		}
		return line
	})
}

// askLocal asks the local network for the IPv4 address of each name, on
// every interface that is used for a path, until each has an answer or
// ctx ends.
func askLocal(ctx context.Context, names []string) map[string]net.IP {
	msg := dnsmessage.Message{}
	for _, name := range names {
		n, err := dnsmessage.NewName(name + ".")
		if err != nil {
			continue
		}
		msg.Questions = append(msg.Questions, dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	}
	question, err := msg.Pack()
	if err != nil || len(msg.Questions) == 0 {
		return nil
	}

	var (
		mu    sync.Mutex
		found = map[string]net.IP{}
		wg    sync.WaitGroup
	)
	all := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(found) == len(msg.Questions)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 || !usableInterface(ifi.Name) {
			continue
		}
		// The socket is at port 5353 and in the group, on this interface.
		conn, err := net.ListenMulticastUDP("udp4", &ifi, mdnsGroup)
		if err != nil {
			continue
		}
		ipv4.NewPacketConn(conn).SetMulticastInterface(&ifi)
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		wg.Add(2)
		go func() { // asks
			defer wg.Done()
			for {
				conn.WriteToUDP(question, mdnsGroup)
				select {
				case <-ctx.Done():
					return
				case <-time.After(localAgain):
				}
			}
		}()
		go func() { // reads
			defer wg.Done()
			defer stop()
			defer func() { recover() }() // a packet is from anyone on the network
			buf := make([]byte, 9000)
			for {
				n, _, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				for name, ip := range localAnswers(buf[:n], names) {
					mu.Lock()
					if _, ok := found[name]; !ok {
						found[name] = ip
					}
					mu.Unlock()
				}
				if all() {
					cancel()
				}
			}
		}()
	}
	wg.Wait()
	return found
}

// localAnswers reads one mDNS packet and gives the IPv4 address of each
// of names that it answers. Anything else in the packet is passed over.
func localAnswers(packet []byte, names []string) map[string]net.IP {
	var p dnsmessage.Parser
	h, err := p.Start(packet)
	if err != nil || !h.Response {
		return nil
	}
	if p.SkipAllQuestions() != nil {
		return nil
	}
	out := map[string]net.IP{}
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			return out
		}
		name := strings.ToLower(strings.TrimSuffix(rh.Name.String(), "."))
		if rh.Type != dnsmessage.TypeA || !slices.Contains(names, name) {
			if p.SkipAnswer() != nil {
				return out
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return out
		}
		ip := net.IP(a.A[:])
		// A ".local" name is a device on this network.
		if (ip.IsPrivate() || ip.IsLinkLocalUnicast()) && usableIP(ip) {
			out[name] = ip
		}
	}
}
