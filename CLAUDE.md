# cx - Image Browser for HPC Clusters

## Building
- Always build with `CGO_ENABLED=0` to produce static binaries (no glibc dependency)
- Go binary path: `/usr/local/go/bin/go`
- Build command: `CGO_ENABLED=0 /usr/local/go/bin/go build -o cx .`
