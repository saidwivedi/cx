# cx -- Cluster Explorer

A fast file and image browser for cluster storage that runs in your local browser.

Browsing cluster files through VS Code remote or SSHFS is painfully slow, especially with images. Every file listing and thumbnail requires multiple network round-trips. `cx` runs a lightweight server directly on a compute node where disk access is local -- it generates thumbnails on the node and only sends small previews over the network.

**A folder with 200k images loads in a couple of seconds.**

Single static Go binary, zero dependencies. Works with **Slurm** and **HTCondor**.

## Quick Start

```bash
CGO_ENABLED=0 go build -o cx main.go
```

### On an HPC cluster

```bash
cx config    # one-time setup, auto-detects Slurm/HTCondor
cx start     # submit job, wait for node, open SSH tunnel, print URL
cx status    # check if running
cx stop      # kill the job
```

### Direct (any machine)

```bash
./cx server --root /path/to/browse --cache-dir /tmp/cx-cache --port 8899
# open http://localhost:8899
```

## How It Works

`cx start` submits the server as a batch job (`sbatch`/`condor_submit_bid`), polls until it lands on a compute node, sets up an SSH tunnel from your workstation, and prints a localhost URL.

Thumbnails are generated on first access (pure Go, no ImageMagick) and cached to disk. Directory listings are cached for 30s.

## Features

- Image thumbnail grid (PNG, JPG, GIF, WebP, BMP, TIFF) with full-resolution viewer
- Video file listing (MP4, AVI, MOV, WebM, MKV)
- Sort by name, date, or size with lazy-loaded pagination
- Keyboard navigation, dark theme, gzip compression

## Requirements

- **Go 1.21+** to build
- For scheduler mode: Slurm or HTCondor on the cluster

## License

MIT
