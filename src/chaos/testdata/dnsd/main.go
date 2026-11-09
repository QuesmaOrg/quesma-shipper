// A DNS responder for the chaos tier, run in a container on the test network: every name in the zone
// answers with one address unless the harness set a fault for it over HTTP. Faults take effect on the
// next query, which a CoreDNS Corefile reload (polled every few seconds) cannot do mid-tick.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type fault struct {
	Rcode   string `json:"rcode,omitempty"`
	Drop    bool   `json:"drop,omitempty"`
	Address string `json:"address,omitempty"`
}

const (
	rcodeNoError  = 0
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeRefused  = 5
	typeA         = 1
)

var (
	zone   = flag.String("zone", "chaos.test", "the zone this server answers for")
	answer = flag.String("answer", "", "the IPv4 address every name in the zone resolves to")

	mu     sync.Mutex
	faults = map[string]fault{}
)

func main() {
	flag.Parse()
	def := net.ParseIP(*answer).To4()
	if def == nil {
		log.Fatalf("-answer %q is not an IPv4 address", *answer)
	}
	pc, err := net.ListenPacket("udp", ":53")
	if err != nil {
		log.Fatal(err)
	}
	go serveDNS(pc, def)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("PUT /names/{name}", func(w http.ResponseWriter, r *http.Request) {
		var f fault
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.Rcode != "" && f.Rcode != "NXDOMAIN" && f.Rcode != "SERVFAIL" {
			http.Error(w, "unknown rcode "+strconv.Quote(f.Rcode)+", want NXDOMAIN or SERVFAIL", http.StatusBadRequest)
			return
		}
		if f.Address != "" && net.ParseIP(f.Address).To4() == nil {
			http.Error(w, "address "+strconv.Quote(f.Address)+" is not IPv4", http.StatusBadRequest)
			return
		}
		mu.Lock()
		faults[strings.ToLower(r.PathValue("name"))] = f
		mu.Unlock()
		log.Printf("fault %s = %+v", r.PathValue("name"), f)
	})
	mux.HandleFunc("DELETE /names/{name}", func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(faults, strings.ToLower(r.PathValue("name")))
		mu.Unlock()
		log.Printf("fault %s cleared", r.PathValue("name"))
	})
	log.Fatal(http.ListenAndServe(":8053", mux))
}

func serveDNS(pc net.PacketConn, def net.IP) {
	buf := make([]byte, 4096)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			log.Fatal(err)
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			if resp := respond(query, def); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}()
	}
}

// The answer to one query, or nil to drop it; AA+RA keep Go from reading an empty answer as lame.
func respond(q []byte, def net.IP) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	name, end, ok := readName(q, 12)
	if !ok || end+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end : end+2])
	question := q[12 : end+4]

	rcode, address := rcodeNoError, def
	mu.Lock()
	f, faulted := faults[name]
	mu.Unlock()
	switch {
	case name != *zone && !strings.HasSuffix(name, "."+*zone):
		rcode = rcodeRefused
	case f.Drop:
		log.Printf("%s type %d: dropped", name, qtype)
		return nil
	case f.Rcode == "NXDOMAIN":
		rcode = rcodeNXDomain
	case f.Rcode == "SERVFAIL":
		rcode = rcodeServFail
	case f.Address != "":
		address = net.ParseIP(f.Address).To4()
	}
	withAnswer := rcode == rcodeNoError && qtype == typeA && address != nil
	resp := make([]byte, 12, 12+len(question)+16)
	copy(resp[0:2], q[0:2])
	flags := uint16(0x8000|0x0400|0x0080) | binary.BigEndian.Uint16(q[2:4])&0x0100 | uint16(rcode)
	binary.BigEndian.PutUint16(resp[2:4], flags)
	binary.BigEndian.PutUint16(resp[4:6], 1)
	if withAnswer {
		binary.BigEndian.PutUint16(resp[6:8], 1)
	}
	resp = append(resp, question...)
	if withAnswer {
		resp = append(resp, 0xc0, 0x0c, 0, typeA, 0, 1, 0, 0, 0, 0, 0, 4)
		resp = append(resp, address...)
	}
	if faulted {
		log.Printf("%s type %d: rcode %d answer %v", name, qtype, rcode, withAnswer)
	}
	return resp
}

// readName decodes an uncompressed question name, lowercased and without the trailing dot.
func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	for off < len(b) {
		n := int(b[off])
		off++
		if n == 0 {
			return strings.ToLower(strings.Join(labels, ".")), off, true
		}
		if n > 63 || off+n > len(b) {
			return "", 0, false
		}
		labels = append(labels, string(b[off:off+n]))
		off += n
	}
	return "", 0, false
}
