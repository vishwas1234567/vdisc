// Copyright © 2019 NVIDIA Corporation
package httputil

import (
	"context"
	"github.com/lukealonso/dnscache"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var sharedResolver = &dnscache.Resolver{
	NoCacheFailures: true,
}
var sharedResolverClear sync.Once

// Modifes the transport to use a dns cache.
func AddDNSCache(t *http.Transport) {
	sharedResolverClear.Do(func() {
		go func() {
			t := time.NewTicker(5*time.Minute + time.Duration(rand.Int31n(5))*time.Minute)
			defer t.Stop()
			for range t.C {
				sharedResolver.Refresh(true)
			}
		}()
	})
	t.DialContext = func(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
		separator := strings.LastIndex(addr, ":")
		ips, err := sharedResolver.LookupHost(ctx, addr[:separator])
		if err != nil {
			return nil, err
		}
		len := len(ips)
		count := 0
		// Choose a random starting point.
		id := rand.Intn(len)
		for {
			var d net.Dialer
			d.Timeout = 10 * time.Second
			conn, err = d.DialContext(ctx, network, ips[id]+addr[separator:])
			if err == nil {
				break
			}
			count++
			// Stop when we have gone through all the IPs.
			if count == len {
				break
			}
			// round robin until we find a good IP.
			id = (id + 1) % len
		}
		return
	}
}
