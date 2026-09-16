// Package mdnsadvert advertises the backend on the local network via
// mDNS/DNS-SD (service type "_lectable._tcp"), so the Android app can
// discover it instead of requiring the user to type in an IP (see
// android/CLAUDE.md's "Server address" section). Advertising only - the
// backend never needs to browse for anything itself.
package mdnsadvert

import (
	"fmt"
	"os"

	"github.com/hashicorp/mdns"
)

// ServiceType is the DNS-SD service type the backend advertises itself
// under. The Android app browses for this same string.
const ServiceType = "_lectable._tcp"

// Server is a running advertisement; call Shutdown when done.
type Server = mdns.Server

// Start registers an mDNS service record for the backend and returns it so
// the caller can Shutdown() it on graceful exit (a clean exit sends a
// goodbye packet so clients drop the entry immediately instead of waiting
// for it to time out).
func Start(port int) (*Server, error) {
	instance, err := os.Hostname()
	if err != nil || instance == "" {
		instance = "lectable"
	}

	service, err := mdns.NewMDNSService(instance, ServiceType, "", "", port, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build mdns service record: %w", err)
	}

	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("start mdns server: %w", err)
	}

	return server, nil
}
