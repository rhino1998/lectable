module github.com/rhino1998/lectable/backend

go 1.26.0

require (
	github.com/coder/websocket v1.8.15
	github.com/hashicorp/mdns v1.0.7
	github.com/kazzmir/opus-go v1.6.0
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/pressly/goose/v3 v3.28.0
	github.com/prometheus/client_golang v1.24.1
	golang.org/x/net v0.59.0
	golang.org/x/text v0.42.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

require (
	github.com/miekg/dns v1.1.72 // indirect
	github.com/rhino1998/lectable/audiocpp-go v0.0.0
	github.com/rhino1998/lectable/llamacpp-go v0.0.0
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/tools v0.49.0
)

replace github.com/rhino1998/lectable/audiocpp-go => ../audiocpp-go

replace github.com/rhino1998/lectable/llamacpp-go => ../llamacpp-go
