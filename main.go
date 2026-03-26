package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	rootDir   string
	thumbSize int
	cacheDir  string
)

// pixelReader provides fast direct pixel access, avoiding interface dispatch per pixel.
type pixelReader struct {
	pix    []uint8
	stride int
	bpp    int // bytes per pixel
	minX   int
	minY   int
}

func newPixelReader(img image.Image) *pixelReader {
	switch v := img.(type) {
	case *image.RGBA:
		return &pixelReader{v.Pix, v.Stride, 4, v.Rect.Min.X, v.Rect.Min.Y}
	case *image.NRGBA:
		return &pixelReader{v.Pix, v.Stride, 4, v.Rect.Min.X, v.Rect.Min.Y}
	default:
		// Convert unknown formats to RGBA once
		b := img.Bounds()
		rgba := image.NewRGBA(b)
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				rgba.Set(x, y, img.At(x, y))
			}
		}
		return &pixelReader{rgba.Pix, rgba.Stride, 4, b.Min.X, b.Min.Y}
	}
}

func resizeAreaAvg(dst *image.RGBA, src image.Image) {
	pr := newPixelReader(src)
	sb := src.Bounds()
	db := dst.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := db.Dx(), db.Dy()
	dstPix := dst.Pix
	dstStride := dst.Stride

	for dy := 0; dy < dh; dy++ {
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		for dx := 0; dx < dw; dx++ {
			sx0 := dx * sw / dw
			sx1 := (dx + 1) * sw / dw

			var rSum, gSum, bSum, count uint32
			for y := sy0; y < sy1; y++ {
				off := (y-pr.minY+sb.Min.Y-pr.minY)*pr.stride + (sx0)*pr.bpp
				for x := sx0; x < sx1; x++ {
					rSum += uint32(pr.pix[off])
					gSum += uint32(pr.pix[off+1])
					bSum += uint32(pr.pix[off+2])
					count++
					off += pr.bpp
				}
			}
			if count == 0 {
				count = 1
			}
			dOff := dy*dstStride + dx*4
			dstPix[dOff] = uint8(rSum / count)
			dstPix[dOff+1] = uint8(gSum / count)
			dstPix[dOff+2] = uint8(bSum / count)
			dstPix[dOff+3] = 255
		}
	}
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".tiff": true, ".tif": true, ".webp": true,
}

var videoExts = map[string]bool{
	".mp4": true, ".avi": true, ".mov": true, ".webm": true, ".mkv": true,
}

// Thumbnail cache: path+mtime -> jpeg bytes
var thumbCache sync.Map

type Entry struct {
	Name    string
	Path    string
	Size    string
	RawSize int64
	Mtime   int64
	IsDir   bool
}

type PageData struct {
	CurrentPath string
	Breadcrumbs []Crumb
	Dirs        []Entry
	Images      []Entry
	Videos      []Entry
	OtherFiles  []Entry
	ImagePaths  []ImageRef
	TotalImages int
	TotalVideos int
	TotalFiles  int
	Sort        string
	Stats       string
}

type Crumb struct {
	Name string
	Path string
}

type ImageRef struct {
	Path string
	Name string
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}

func safePath(subpath string) (string, error) {
	cleaned := filepath.Clean("/" + subpath)
	full := filepath.Join(rootDir, cleaned)
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		// File might not exist yet, use cleaned path
		real = full
	}
	realRoot, _ := filepath.EvalSymlinks(rootDir)
	if !strings.HasPrefix(real, realRoot) {
		return "", fmt.Errorf("forbidden")
	}
	return real, nil
}

func thumbCacheKey(path string, mtime int64) string {
	h := md5.Sum([]byte(fmt.Sprintf("%s:%d:%d", path, mtime, thumbSize)))
	return fmt.Sprintf("%x", h)
}

func generateThumb(fullPath string, mtime int64) ([]byte, error) {
	key := thumbCacheKey(fullPath, mtime)

	// Check memory cache
	if cached, ok := thumbCache.Load(key); ok {
		return cached.([]byte), nil
	}

	// Check disk cache
	diskPath := filepath.Join(cacheDir, key+".jpg")
	if data, err := os.ReadFile(diskPath); err == nil {
		thumbCache.Store(key, data)
		return data, nil
	}

	// Generate thumbnail
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Calculate new dimensions preserving aspect ratio
	newW, newH := thumbSize, thumbSize
	if w > h {
		newH = h * thumbSize / w
	} else {
		newW = w * thumbSize / h
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	resizeAreaAvg(dst, img)

	var buf bytes.Buffer
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 60})
	if err != nil {
		return nil, err
	}

	data := buf.Bytes()

	// Store in memory cache
	thumbCache.Store(key, data)

	// Store on disk (fire and forget)
	go func() {
		_ = os.WriteFile(diskPath, data, 0644)
	}()

	return data, nil
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	subpath := strings.TrimPrefix(r.URL.Path, "/browse")
	subpath = strings.TrimPrefix(subpath, "/")

	full, err := safePath(subpath)
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}

	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		http.Error(w, "Not found", 404)
		return
	}

	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "date"
	}

	// Build breadcrumbs
	parts := strings.Split(strings.Trim(subpath, "/"), "/")
	if parts[0] == "" {
		parts = nil
	}
	var breadcrumbs []Crumb
	for i, p := range parts {
		breadcrumbs = append(breadcrumbs, Crumb{
			Name: p,
			Path: strings.Join(parts[:i+1], "/") + "/",
		})
	}

	// Read directory
	entries, err := os.ReadDir(full)
	if err != nil {
		http.Error(w, "Permission denied", 403)
		return
	}

	var dirs []Entry
	var totalImages, totalVideos, totalFiles int

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}

		if e.IsDir() {
			rel := name
			if subpath != "" {
				rel = strings.TrimRight(subpath, "/") + "/" + name
			}
			var mtime int64
			if info, err := e.Info(); err == nil {
				mtime = info.ModTime().Unix()
			}
			dirs = append(dirs, Entry{Name: name, Path: rel + "/", Mtime: mtime, IsDir: true})
		} else {
			ext := strings.ToLower(filepath.Ext(name))
			if imageExts[ext] {
				totalImages++
			} else if videoExts[ext] {
				totalVideos++
			} else {
				totalFiles++
			}
		}
	}

	// Sort dirs
	sortFn := func(a, b Entry) bool {
		switch sortBy {
		case "date":
			return a.Mtime > b.Mtime
		case "size":
			return a.RawSize > b.RawSize
		default:
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return sortFn(dirs[i], dirs[j]) })

	total := len(dirs) + totalImages + totalVideos + totalFiles

	data := PageData{
		CurrentPath: "/" + subpath,
		Breadcrumbs: breadcrumbs,
		Dirs:        dirs,
		TotalImages: totalImages,
		TotalVideos: totalVideos,
		TotalFiles:  totalFiles,
		Sort:        sortBy,
		Stats:       fmt.Sprintf("%d items", total),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Gzip if client supports it
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		if err := pageTmpl.Execute(gz, data); err != nil {
			log.Printf("template error: %v", err)
		}
	} else {
		if err := pageTmpl.Execute(w, data); err != nil {
			log.Printf("template error: %v", err)
		}
	}
}

func handleThumb(w http.ResponseWriter, r *http.Request) {
	subpath := strings.TrimPrefix(r.URL.Path, "/thumb/")

	full, err := safePath(subpath)
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}

	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, "Not found", 404)
		return
	}

	data, err := generateThumb(full, info.ModTime().Unix())
	if err != nil {
		// Fallback: serve original
		http.ServeFile(w, r, full)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func handleRaw(w http.ResponseWriter, r *http.Request) {
	subpath := strings.TrimPrefix(r.URL.Path, "/raw/")

	full, err := safePath(subpath)
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, full)
}

const pageSize = 100

// dirCache caches sorted file listings per directory+sort key.
type dirCacheEntry struct {
	images []Entry
	videos []Entry
	files  []Entry
	time   time.Time
}

var dirCache sync.Map // key: "path|sort" -> *dirCacheEntry

func getDirEntries(full, subpath, sortBy string) (images, videos, files []Entry) {
	cacheKey := full + "|" + sortBy
	if cached, ok := dirCache.Load(cacheKey); ok {
		entry := cached.(*dirCacheEntry)
		if time.Since(entry.time) < 30*time.Second {
			return entry.images, entry.videos, entry.files
		}
	}

	entries, _ := os.ReadDir(full)
	needStat := sortBy == "date" || sortBy == "size"

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		rel := name
		if subpath != "" {
			rel = strings.TrimRight(subpath, "/") + "/" + name
		}
		var size int64
		var mtime int64
		if needStat {
			if info, err := e.Info(); err == nil {
				size = info.Size()
				mtime = info.ModTime().Unix()
			}
		}
		entry := Entry{Name: name, Path: rel, Size: humanSize(size), RawSize: size, Mtime: mtime}

		if imageExts[ext] {
			images = append(images, entry)
		} else if videoExts[ext] {
			videos = append(videos, entry)
		} else {
			files = append(files, entry)
		}
	}

	sortSlice := func(s []Entry) {
		switch sortBy {
		case "date":
			sort.Slice(s, func(i, j int) bool { return s[i].Mtime > s[j].Mtime })
		case "size":
			sort.Slice(s, func(i, j int) bool { return s[i].RawSize > s[j].RawSize })
		default:
			sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i].Name) < strings.ToLower(s[j].Name) })
		}
	}
	sortSlice(images)
	sortSlice(videos)
	sortSlice(files)


	dirCache.Store(cacheKey, &dirCacheEntry{images, videos, files, time.Now()})
	return
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	subpath := strings.TrimPrefix(r.URL.Path, "/api/")
	full, err := safePath(subpath)
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}

	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		http.Error(w, "Not found", 404)
		return
	}

	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "date"
	}
	itemType := r.URL.Query().Get("type")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	allImages, allVideos, allFiles := getDirEntries(full, subpath, sortBy)

	var items []Entry
	switch itemType {
	case "images":
		items = allImages
	case "videos":
		items = allVideos
	case "files":
		items = allFiles
	}

	total := len(items)
	if offset >= total {
		items = nil
	} else {
		end := offset + pageSize
		if end > total {
			end = total
		}
		items = items[offset:end]
	}

	// Lazy stat: only get file sizes for items we're returning
	if items != nil {
		for i := range items {
			if items[i].RawSize == 0 {
				p := filepath.Join(full, filepath.Base(items[i].Name))
				if info, err := os.Stat(p); err == nil {
					items[i].Size = humanSize(info.Size())
					items[i].RawSize = info.Size()
					items[i].Mtime = info.ModTime().Unix()
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=10")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"items":   items,
		"total":   total,
		"offset":  offset,
		"hasMore": offset+pageSize < total,
	})
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	subpath := strings.TrimPrefix(r.URL.Path, "/refresh/")
	full, err := safePath(subpath)
	if err != nil {
		http.Error(w, "Forbidden", 403)
		return
	}

	// Clear dirCache entries for this directory
	dirCache.Range(func(key, _ interface{}) bool {
		if strings.HasPrefix(key.(string), full+"|") {
			dirCache.Delete(key)
		}
		return true
	})

	// Clear thumbnail cache for files in this directory
	entries, _ := os.ReadDir(full)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !imageExts[ext] {
			continue
		}
		filePath := filepath.Join(full, e.Name())
		if info, err := e.Info(); err == nil {
			key := thumbCacheKey(filePath, info.ModTime().Unix())
			thumbCache.Delete(key)
			os.Remove(filepath.Join(cacheDir, key+".jpg"))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/browse/", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

var pageTmpl *template.Template

// ── Config ───────────────────────────────────────────────────────────

type PathMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Config struct {
	RootDir      string        `json:"root_dir"`
	CacheDir     string        `json:"cache_dir"`
	Port         int           `json:"port"`
	CPUs         int           `json:"cpus"`
	MemoryMB     int           `json:"memory_mb"`
	LogDir       string        `json:"log_dir"`
	CondorLog    string        `json:"condor_log_dir,omitempty"` // deprecated, for migration
	LoginNode    string        `json:"login_node"`
	ThumbSize    int           `json:"thumb_size"`
	SchedulerType string      `json:"scheduler"`
	PathMappings []PathMapping `json:"path_mappings,omitempty"`
}

func (cfg Config) mapPath(p string) string {
	for _, m := range cfg.PathMappings {
		p = strings.Replace(p, m.From, m.To, 1)
	}
	return p
}

// ── Scheduler interface ──────────────────────────────────────────────

type JobStatus struct {
	State string // "Idle", "Running", "Completed", "Held", "Removed", "Unknown"
	Host  string
}

type Scheduler interface {
	Submit(cfg Config, exe string, args []string) (jobID string, err error)
	PollForRunning(cfg Config, jobID string, maxWait time.Duration) (host string, err error)
	Status(cfg Config, jobID string) (JobStatus, error)
	Stop(cfg Config, jobID string) error
	IsJobAlive(cfg Config, jobID string) (alive bool, host string)
}

// ── HTCondor scheduler ──────────────────────────────────────────────

type condorScheduler struct{}

func (condorScheduler) Submit(cfg Config, exe string, args []string) (string, error) {
	subContent := fmt.Sprintf(`executable = %s
arguments = "%s"
error = %s/fb_$(Cluster).$(Process).err
output = %s/fb_$(Cluster).$(Process).out
log = %s/fb_$(Cluster).$(Process).log
request_memory = %d
request_cpus = %d
request_gpus = 0
queue 1
`, exe, strings.Join(args, " "),
		cfg.mapPath(cfg.LogDir), cfg.mapPath(cfg.LogDir), cfg.mapPath(cfg.LogDir),
		cfg.MemoryMB, cfg.CPUs)

	subFile := filepath.Join(cfg.LogDir, "auto_submit.sub")
	os.WriteFile(subFile, []byte(subContent), 0644)

	out, err := runCmd(cfg, "condor_submit_bid", "100", cfg.mapPath(subFile))
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	var clusterID string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "submitted to cluster") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "cluster" && i+1 < len(parts) {
					clusterID = strings.TrimSuffix(parts[i+1], ".")
				}
			}
		}
	}
	if clusterID == "" {
		return "", fmt.Errorf("couldn't parse cluster ID from: %s", string(out))
	}
	return clusterID, nil
}

func (condorScheduler) PollForRunning(cfg Config, jobID string, maxWait time.Duration) (string, error) {
	logFile := filepath.Join(cfg.LogDir, fmt.Sprintf("fb_%s.0.log", jobID))
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		data, err := os.ReadFile(logFile)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "Job executing on host:") {
				if idx := strings.Index(line, "alias="); idx >= 0 {
					host := line[idx+6:]
					if end := strings.IndexAny(host, ".&>"); end >= 0 {
						host = host[:end]
					}
					if host != "" {
						return host, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("timed out waiting for job to start")
}

func (condorScheduler) Status(cfg Config, jobID string) (JobStatus, error) {
	out, _ := runCmd(cfg, "condor_q", jobID+".0", "-af", "JobStatus", "RemoteHost")
	fields := strings.Fields(strings.TrimSpace(string(out)))
	statusMap := map[string]string{"1": "Idle", "2": "Running", "3": "Removed", "4": "Completed", "5": "Held"}
	if len(fields) >= 1 {
		s := statusMap[fields[0]]
		if s == "" {
			s = "Unknown (" + fields[0] + ")"
		}
		host := ""
		if len(fields) >= 2 {
			host = fields[1]
		}
		return JobStatus{State: s, Host: host}, nil
	}
	return JobStatus{}, fmt.Errorf("job no longer in queue")
}

func (condorScheduler) Stop(cfg Config, jobID string) error {
	out, err := runCmd(cfg, "condor_rm", jobID)
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (condorScheduler) IsJobAlive(cfg Config, jobID string) (bool, string) {
	logFile := filepath.Join(cfg.LogDir, fmt.Sprintf("fb_%s.0.log", jobID))
	logData, _ := os.ReadFile(logFile)
	logStr := string(logData)
	executing := strings.Contains(logStr, "Job executing on host:")
	terminated := strings.Contains(logStr, "Job terminated") || strings.Contains(logStr, "Job was aborted") || strings.Contains(logStr, "Job was removed")
	if executing && !terminated {
		// Extract host from log
		var host string
		for _, line := range strings.Split(logStr, "\n") {
			if strings.Contains(line, "Job executing on host:") {
				if idx := strings.Index(line, "alias="); idx >= 0 {
					h := line[idx+6:]
					if end := strings.IndexAny(h, ".&>"); end >= 0 {
						h = h[:end]
					}
					host = h
				}
			}
		}
		return true, host
	}
	return false, ""
}

// ── Slurm scheduler ─────────────────────────────────────────────────

type slurmScheduler struct{}

func (slurmScheduler) Submit(cfg Config, exe string, args []string) (string, error) {
	script := fmt.Sprintf(`#!/bin/bash
#SBATCH --job-name=cx
#SBATCH --cpus-per-task=%d
#SBATCH --mem=%dM
#SBATCH --output=%s/cx_%%j.out
#SBATCH --error=%s/cx_%%j.err
#SBATCH --gres=gpu:0
%s %s
`, cfg.CPUs, cfg.MemoryMB,
		cfg.mapPath(cfg.LogDir), cfg.mapPath(cfg.LogDir),
		exe, strings.Join(args, " "))

	scriptFile := filepath.Join(cfg.LogDir, "cx_submit.sh")
	os.WriteFile(scriptFile, []byte(script), 0755)

	out, err := runCmd(cfg, "sbatch", cfg.mapPath(scriptFile))
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}

	// Parse "Submitted batch job 12345"
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) >= 4 {
		return fields[3], nil
	}
	return "", fmt.Errorf("couldn't parse job ID from: %s", string(out))
}

func (slurmScheduler) PollForRunning(cfg Config, jobID string, maxWait time.Duration) (string, error) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		out, err := runCmd(cfg, "squeue", "-j", jobID, "-h", "-o", "%T %N")
		if err != nil {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) >= 2 && fields[0] == "RUNNING" && fields[1] != "" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("timed out waiting for job to start")
}

func (slurmScheduler) Status(cfg Config, jobID string) (JobStatus, error) {
	out, err := runCmd(cfg, "squeue", "-j", jobID, "-h", "-o", "%T %N")
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if err == nil && len(fields) >= 1 && fields[0] != "" {
		stateMap := map[string]string{
			"PENDING": "Idle", "RUNNING": "Running", "COMPLETING": "Running",
			"COMPLETED": "Completed", "CANCELLED": "Removed", "FAILED": "Completed",
			"TIMEOUT": "Completed", "NODE_FAIL": "Completed",
		}
		s := stateMap[fields[0]]
		if s == "" {
			s = "Unknown (" + fields[0] + ")"
		}
		host := ""
		if len(fields) >= 2 {
			host = fields[1]
		}
		return JobStatus{State: s, Host: host}, nil
	}
	return JobStatus{}, fmt.Errorf("job no longer in queue")
}

func (slurmScheduler) Stop(cfg Config, jobID string) error {
	out, err := runCmd(cfg, "scancel", jobID)
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (slurmScheduler) IsJobAlive(cfg Config, jobID string) (bool, string) {
	out, _ := runCmd(cfg, "squeue", "-j", jobID, "-h", "-o", "%T %N")
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) >= 1 && (fields[0] == "RUNNING" || fields[0] == "PENDING") {
		host := ""
		if len(fields) >= 2 {
			host = fields[1]
		}
		return true, host
	}
	return false, ""
}

// ── Scheduler factory ───────────────────────────────────────────────

func isOnCluster() bool {
	for _, cmd := range []string{"condor_q", "squeue"} {
		if _, err := exec.LookPath(cmd); err == nil {
			return true
		}
	}
	return false
}

func getScheduler(cfg Config) Scheduler {
	switch cfg.SchedulerType {
	case "condor":
		return condorScheduler{}
	case "slurm":
		return slurmScheduler{}
	default:
		// Auto-detect from locally available commands
		if _, err := exec.LookPath("condor_q"); err == nil {
			return condorScheduler{}
		}
		if _, err := exec.LookPath("squeue"); err == nil {
			return slurmScheduler{}
		}
		fmt.Fprintf(os.Stderr, "No scheduler configured and none detected locally.\nRun 'cx config' to set your scheduler type.\n")
		os.Exit(1)
		return nil
	}
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cx")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

func statePath() string {
	return filepath.Join(configDir(), "state.json")
}

type State struct {
	ClusterID string `json:"cluster_id"`
	Host      string `json:"host"`
}

// ANSI escape helpers
const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiCyan    = "\033[36m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiBlue    = "\033[34m"
	ansiMagenta = "\033[35m"
	ansiWhite   = "\033[97m"
	ansiBgDark  = "\033[48;5;236m"
	ansiClear   = "\033[2K"
)

func prompt(reader *bufio.Reader, label, desc, defVal string) string {
	fmt.Println()
	fmt.Printf("  %s%s%s %s%s%s\n", ansiBold, ansiCyan, "?", ansiReset, ansiBold, label)
	if desc != "" {
		fmt.Printf("  %s%s%s\n", ansiDim, desc, ansiReset)
	}
	if defVal != "" {
		fmt.Printf("  %s%s› %s%s", ansiGreen, ansiBold, ansiReset, "")
		fmt.Printf("%s%s%s ", ansiDim, defVal, ansiReset)
		// Move cursor back to overwrite
		fmt.Printf("\r  %s%s› %s", ansiGreen, ansiBold, ansiReset)
	} else {
		fmt.Printf("  %s%s› %s", ansiGreen, ansiBold, ansiReset)
	}
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defVal
	}
	return line
}

func promptInt(reader *bufio.Reader, label, desc string, defVal int) int {
	s := prompt(reader, label, desc, strconv.Itoa(defVal))
	v, err := strconv.Atoi(s)
	if err != nil {
		return defVal
	}
	return v
}

func runConfig() {
	reader := bufio.NewReader(os.Stdin)
	cfg := Config{
		Port:      8899,
		CPUs:      2,
		MemoryMB:  4000,
		ThumbSize: 256,
	}

	// Load existing config if present
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &cfg)
	}
	migrateConfig(&cfg)

	// Set smart defaults based on current user
	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}

	// Auto-detect scheduler if not set
	defaultSched := cfg.SchedulerType
	if defaultSched == "" {
		if _, err := exec.LookPath("condor_q"); err == nil {
			defaultSched = "condor"
		} else if _, err := exec.LookPath("squeue"); err == nil {
			defaultSched = "slurm"
		} else {
			defaultSched = "slurm"
		}
	}

	fmt.Println()
	fmt.Printf("  %s%s╭────────────────────╮%s\n", ansiBold, ansiBlue, ansiReset)
	fmt.Printf("  %s%s│  cx Configuration  │%s\n", ansiBold, ansiBlue, ansiReset)
	fmt.Printf("  %s%s╰────────────────────╯%s\n", ansiBold, ansiBlue, ansiReset)
	actualHost, _ := os.Hostname()
	fmt.Printf("  %shost: %s  user: %s%s\n", ansiDim, actualHost, user, ansiReset)

	cfg.SchedulerType = prompt(reader,
		"Scheduler",
		"\"condor\" for HTCondor, \"slurm\" for Slurm",
		defaultSched)

	// Set directory defaults based on scheduler
	homeDir, _ := os.UserHomeDir()
	defaultLogDir := cfg.LogDir
	if defaultLogDir == "" {
		if cfg.SchedulerType == "condor" {
			defaultLogDir = homeDir + "/condor/cx"
		} else {
			defaultLogDir = homeDir + "/slurm/cx"
		}
	}
	if cfg.RootDir == "" {
		cfg.RootDir = homeDir
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = homeDir + "/tmp/cx-cache"
	}

	cfg.RootDir = prompt(reader,
		"Root directory",
		"The top-level directory to browse",
		cfg.RootDir)

	cfg.CacheDir = prompt(reader,
		"Cache directory",
		"Where to store generated thumbnails",
		cfg.CacheDir)

	cfg.LogDir = prompt(reader,
		"Log directory",
		"Where scheduler writes job logs",
		defaultLogDir)

	cfg.LoginNode = prompt(reader,
		"Login node",
		"SSH hostname of the cluster login node (for remote access)",
		func() string {
			if cfg.LoginNode != "" {
				return cfg.LoginNode
			}
			return actualHost
		}())

	cfg.Port = promptInt(reader,
		"Port",
		"HTTP port for the file browser",
		cfg.Port)

	cfg.CPUs = promptInt(reader,
		"CPUs",
		"Number of CPUs to request",
		cfg.CPUs)

	cfg.MemoryMB = promptInt(reader,
		"Memory (MB)",
		"RAM to request",
		cfg.MemoryMB)

	cfg.ThumbSize = promptInt(reader,
		"Thumbnail size (px)",
		"Resolution for generated image thumbnails",
		cfg.ThumbSize)

	// Path mapping (optional)
	defaultMapping := ""
	if len(cfg.PathMappings) > 0 {
		defaultMapping = cfg.PathMappings[0].From + ":" + cfg.PathMappings[0].To
	}
	mappingStr := prompt(reader,
		"Path mapping (optional)",
		"If login and compute nodes mount paths differently, e.g. /nfs/data/:/scratch/data/. Leave empty if same.",
		defaultMapping)
	cfg.PathMappings = nil
	if mappingStr != "" {
		parts := strings.SplitN(mappingStr, ":", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			cfg.PathMappings = []PathMapping{{From: parts[0], To: parts[1]}}
		}
	}

	// Clear deprecated field
	cfg.CondorLog = ""

	os.MkdirAll(configDir(), 0755)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath(), data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "\n  %sError saving config: %v%s\n", ansiYellow, err, ansiReset)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Printf("  %s%s✓%s Config saved to %s%s%s\n\n", ansiBold, ansiGreen, ansiReset, ansiDim, configPath(), ansiReset)
	fmt.Printf("  %scx start%s  — launch server and open browser\n", ansiBold, ansiReset)
	fmt.Printf("  %scx stop%s   — kill the running server\n", ansiBold, ansiReset)
	fmt.Printf("  %scx status%s — check if server is running\n\n", ansiBold, ansiReset)
}

func migrateConfig(cfg *Config) {
	if cfg.LogDir == "" && cfg.CondorLog != "" {
		cfg.LogDir = cfg.CondorLog
		if cfg.SchedulerType == "" {
			cfg.SchedulerType = "condor"
		}
	}
}

func loadConfig() Config {
	var cfg Config
	data, err := os.ReadFile(configPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "No config found. Run: cx config\n")
		os.Exit(1)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Bad config: %v\n", err)
		os.Exit(1)
	}
	migrateConfig(&cfg)
	return cfg
}

func saveState(s State) {
	data, _ := json.Marshal(s)
	os.WriteFile(statePath(), data, 0644)
}

func loadState() (State, bool) {
	var s State
	data, err := os.ReadFile(statePath())
	if err != nil {
		return s, false
	}
	json.Unmarshal(data, &s)
	return s, s.ClusterID != ""
}

// Command full paths for SSH fallback
var cmdPaths = map[string]string{
	"condor_submit_bid": "/usr/local/bin/condor_submit_bid",
	"condor_q":          "/usr/local/bin/condor_q",
	"condor_rm":         "/usr/bin/condor_rm",
	"sbatch":            "/usr/bin/sbatch",
	"squeue":            "/usr/bin/squeue",
	"scancel":           "/usr/bin/scancel",
	"sacct":             "/usr/bin/sacct",
}

var sshControlPath string

// setupSSH creates a persistent SSH connection for reuse.
func setupSSH(cfg Config) {
	if sshControlPath != "" {
		return
	}
	sshControlPath = fmt.Sprintf("/tmp/cx-ssh-%d", os.Getpid())
	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "ControlMaster=yes",
		"-o", "ControlPath="+sshControlPath,
		"-o", "ControlPersist=300",
		"-fN", cfg.LoginNode)
	cmd.Run()
}

// cleanupSSH closes the persistent SSH connection.
func cleanupSSH(cfg Config) {
	if sshControlPath == "" {
		return
	}
	exec.Command("ssh", "-o", "ControlPath="+sshControlPath, "-O", "exit", cfg.LoginNode).Run()
	sshControlPath = ""
}

// runCmd runs a command locally or via SSH to login node if not available locally.
func runCmd(cfg Config, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err == nil {
		return exec.Command(name, args...).CombinedOutput()
	}
	if fullPath, ok := cmdPaths[name]; ok {
		name = fullPath
	}
	setupSSH(cfg)
	sshArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "ControlPath=" + sshControlPath, cfg.LoginNode, name}
	sshArgs = append(sshArgs, args...)
	return exec.Command("ssh", sshArgs...).CombinedOutput()
}

func runStart() {
	cfg := loadConfig()
	sched := getScheduler(cfg)

	// Check if already running
	if st, ok := loadState(); ok {
		if alive, _ := sched.IsJobAlive(cfg, st.ClusterID); alive {
			fmt.Println()
			fmt.Printf("  %s▸%s Already running on %s%s:%d%s %s#%s%s\n", ansiCyan, ansiReset, ansiBold, st.Host, cfg.Port, ansiReset, ansiDim, st.ClusterID, ansiReset)
			if !isOnCluster() {
				fmt.Printf("  %s▸%s SSH tunnel...", ansiCyan, ansiReset)
				tunnelCmd := exec.Command("ssh", "-fNL",
					fmt.Sprintf("%d:%s:%d", cfg.Port, st.Host, cfg.Port),
					cfg.LoginNode)
				if err := tunnelCmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, " %s✗%s\n", ansiYellow, ansiReset)
					printConnect(cfg, st)
				} else {
					fmt.Printf(" %s✓%s\n\n", ansiGreen, ansiReset)
					fmt.Printf("  %s%s✓ Ready — http://localhost:%d%s\n\n", ansiBold, ansiGreen, cfg.Port, ansiReset)
				}
			} else {
				printConnect(cfg, st)
			}
			return
		}
		os.Remove(statePath())
	}

	// Ensure dirs exist
	runCmd(cfg, "mkdir", "-p", cfg.LogDir)
	runCmd(cfg, "mkdir", "-p", cfg.CacheDir)

	exe, _ := os.Executable()
	exeAbs, _ := filepath.Abs(exe)
	clusterExe := cfg.mapPath(exeAbs)
	args := []string{
		"server",
		"--root", cfg.mapPath(cfg.RootDir),
		"--cache-dir", cfg.mapPath(cfg.CacheDir),
		"--port", strconv.Itoa(cfg.Port),
		"--host", "0.0.0.0",
		"--thumb-size", strconv.Itoa(cfg.ThumbSize),
	}

	fmt.Println()
	fmt.Printf("  %s▸%s Submitting job...", ansiCyan, ansiReset)
	jobID, err := sched.Submit(cfg, clusterExe, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, " %s✗%s\n    %s\n", ansiYellow, ansiReset, err)
		os.Exit(1)
	}
	fmt.Printf(" %s✓%s %s#%s%s\n", ansiGreen, ansiReset, ansiDim, jobID, ansiReset)

	fmt.Printf("  %s▸%s Waiting for node...", ansiCyan, ansiReset)
	workerHost, err := sched.PollForRunning(cfg, jobID, 2*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, " %s✗%s %s\n", ansiYellow, ansiReset, err)
		os.Exit(1)
	}
	fmt.Printf(" %s✓%s %s%s:%d%s\n", ansiGreen, ansiReset, ansiBold, workerHost, cfg.Port, ansiReset)

	st := State{ClusterID: jobID, Host: workerHost}
	saveState(st)
	cleanupSSH(cfg)

	// Auto-start SSH tunnel if running from workstation
	if !isOnCluster() {
		fmt.Printf("  %s▸%s SSH tunnel...", ansiCyan, ansiReset)
		tunnelCmd := exec.Command("ssh", "-fNL",
			fmt.Sprintf("%d:%s:%d", cfg.Port, workerHost, cfg.Port),
			cfg.LoginNode)
		if err := tunnelCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, " %s✗%s %v\n\n", ansiYellow, ansiReset, err)
			printConnect(cfg, st)
		} else {
			fmt.Printf(" %s✓%s\n", ansiGreen, ansiReset)
			fmt.Println()
			fmt.Printf("  %s%s✓ Ready — http://localhost:%d%s\n\n", ansiBold, ansiGreen, cfg.Port, ansiReset)
		}
	} else {
		printConnect(cfg, st)
	}
}

func printConnect(cfg Config, st State) {
	fmt.Println()
	fmt.Printf("  %s%s╭─────────────────────────────────────────╮%s\n", ansiBold, ansiBlue, ansiReset)
	fmt.Printf("  %s%s│         Connection Instructions          │%s\n", ansiBold, ansiBlue, ansiReset)
	fmt.Printf("  %s%s╰─────────────────────────────────────────╯%s\n", ansiBold, ansiBlue, ansiReset)
	fmt.Println()
	fmt.Printf("  %s1.%s From your workstation, run:\n", ansiBold, ansiReset)
	fmt.Printf("     %s%sssh -fNL %d:%s:%d %s%s\n", ansiBold, ansiGreen, cfg.Port, st.Host, cfg.Port, cfg.LoginNode, ansiReset)
	fmt.Println()
	fmt.Printf("  %s2.%s Open in browser:\n", ansiBold, ansiReset)
	fmt.Printf("     %s%shttp://localhost:%d%s\n", ansiBold, ansiCyan, cfg.Port, ansiReset)
	fmt.Println()
}

func runStop() {
	cfg := loadConfig()
	st, ok := loadState()
	if !ok {
		fmt.Println("No running job found.")
		return
	}
	sched := getScheduler(cfg)
	if err := sched.Stop(cfg, st.ClusterID); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	} else {
		fmt.Printf("Removed job %s\n", st.ClusterID)
	}
	os.Remove(statePath())
}

func runStatus() {
	cfg := loadConfig()
	st, ok := loadState()
	if !ok {
		fmt.Println("No job tracked. Run: cx start")
		return
	}
	sched := getScheduler(cfg)
	status, err := sched.Status(cfg, st.ClusterID)
	if err != nil {
		fmt.Printf("Job %s no longer in queue.\n", st.ClusterID)
		os.Remove(statePath())
		return
	}
	fmt.Printf("Job:    %s\nStatus: %s\nNode:   %s\n", st.ClusterID, status.State, st.Host)
	if status.State == "Running" {
		printConnect(cfg, st)
	}
}

func runServer() {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	root := fs.String("root", ".", "Root directory")
	port := fs.Int("port", 8899, "Port")
	host := fs.String("host", "0.0.0.0", "Host")
	thumb := fs.Int("thumb-size", 256, "Thumbnail size")
	cache := fs.String("cache-dir", "", "Cache directory")
	fs.Parse(os.Args[2:])

	rootDir = *root
	thumbSize = *thumb
	cacheDir = *cache
	if cacheDir == "" {
		fmt.Fprintln(os.Stderr, "Error: --cache-dir is required")
		os.Exit(1)
	}
	os.MkdirAll(cacheDir, 0755)

	if abs, err := filepath.Abs(rootDir); err == nil {
		rootDir = abs
	}

	funcMap := template.FuncMap{
		"sub1": func(n int) int { return n - 1 },
		"extUpper": func(name string) string {
			ext := strings.ToLower(filepath.Ext(name))
			if ext == "" {
				return "\u2014"
			}
			return strings.ToUpper(strings.TrimPrefix(ext, "."))
		},
		"extClass": func(name string) string {
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
			switch ext {
			case "py":
				return "py"
			case "sh", "bash", "zsh":
				return "sh"
			case "json":
				return "json"
			case "yaml", "yml":
				return "yaml"
			case "txt", "md", "rst":
				return "txt"
			case "pdf":
				return "pdf"
			case "zip", "tar", "gz", "bz2", "xz", "7z", "rar":
				return "zip"
			case "pt", "pth", "ckpt", "bin", "safetensors":
				return "pt"
			case "npy", "npz":
				return "npy"
			case "csv", "tsv":
				return "csv"
			default:
				return ""
			}
		},
	}
	pageTmpl = template.Must(template.New("page").Funcs(funcMap).Parse(htmlTemplate))

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/browse/", handleBrowse)
	mux.HandleFunc("/thumb/", handleThumb)
	mux.HandleFunc("/raw/", handleRaw)
	mux.HandleFunc("/api/", handleAPI)
	mux.HandleFunc("/refresh/", handleRefresh)

	hostname, _ := os.Hostname()
	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("Serving %s at http://%s:%d", rootDir, hostname, *port)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(`cx - Cluster Explorer: fast web-based file/image browser for HPC clusters

Usage:
  cx config    Configure settings (first-time setup)
  cx start     Submit job to cluster scheduler (HTCondor/Slurm)
  cx stop      Stop the running job
  cx status    Show current job status
  cx server    Run the file browser server directly (no scheduler)`)
		os.Exit(0)
	}

	switch os.Args[1] {
	case "config":
		runConfig()
	case "start":
		runStart()
	case "stop":
		runStop()
	case "status":
		runStatus()
	case "server":
		runServer()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\nRun 'cx' for usage.\n", os.Args[1])
		os.Exit(1)
	}
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.CurrentPath}}</title>
<style>
/* ── Reset & base ─────────────────────────────────────────────────── */
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0d1117;
  --surface:#161b22;
  --surface2:#21262d;
  --surface3:#30363d;
  --border:#30363d;
  --border2:#21262d;
  --text:#e6edf3;
  --text-muted:#8b949e;
  --text-dim:#484f58;
  --accent:#58a6ff;
  --accent-hover:#79c0ff;
  --accent-bg:rgba(88,166,255,.08);
  --green:#3fb950;
  --thumb:220px;
  --radius:8px;
  --radius-lg:12px;
  --shadow:0 1px 3px rgba(0,0,0,.4),0 8px 24px rgba(0,0,0,.3);
  --shadow-hover:0 4px 12px rgba(0,0,0,.5),0 12px 32px rgba(0,0,0,.4);
  --font:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif;
  --font-mono:'SFMono-Regular','Consolas','Liberation Mono',Menlo,monospace;
}
html{scrollbar-color:var(--surface3) var(--bg);scrollbar-width:thin}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.5;min-height:100vh}

/* ── Scrollbar (webkit) ───────────────────────────────────────────── */
::-webkit-scrollbar{width:8px;height:8px}
::-webkit-scrollbar-track{background:var(--bg)}
::-webkit-scrollbar-thumb{background:var(--surface3);border-radius:4px}
::-webkit-scrollbar-thumb:hover{background:var(--border)}

/* ── Header ───────────────────────────────────────────────────────── */
.header{
  position:sticky;top:0;z-index:200;
  background:rgba(13,17,23,.92);
  backdrop-filter:blur(12px);
  -webkit-backdrop-filter:blur(12px);
  border-bottom:1px solid var(--border2);
  padding:0 20px;
}
.header-inner{
  max-width:1800px;margin:0 auto;
  display:flex;flex-direction:column;gap:0;
}
.header-top{
  display:flex;align-items:center;gap:12px;
  padding:10px 0 8px;
  border-bottom:1px solid var(--border2);
}
.app-icon{
  width:28px;height:28px;flex-shrink:0;
  background:linear-gradient(135deg,#1f6feb,#388bfd);
  border-radius:6px;
  display:flex;align-items:center;justify-content:center;
  font-size:14px;line-height:1;
}

/* ── Breadcrumb ───────────────────────────────────────────────────── */
.back-btn{
  background:var(--surface);border:1px solid var(--border);
  border-radius:6px;padding:4px 6px;cursor:pointer;
  color:var(--text-muted);display:flex;align-items:center;
  transition:background .15s,color .15s,border-color .15s;
  flex-shrink:0;
}
.back-btn:hover{background:var(--surface2);color:var(--text);border-color:var(--accent)}
.breadcrumb{
  display:flex;align-items:center;flex-wrap:wrap;gap:2px;
  font-size:13px;min-width:0;flex:1;
}
.breadcrumb a{
  color:var(--text-muted);text-decoration:none;
  padding:2px 6px;border-radius:4px;
  transition:color .15s,background .15s;
  white-space:nowrap;
}
.breadcrumb a:hover{color:var(--text);background:var(--surface2)}
.breadcrumb .crumb-last{color:var(--text);font-weight:500}
.breadcrumb .sep{color:var(--text-dim);padding:0 2px;user-select:none}

/* ── Controls bar ─────────────────────────────────────────────────── */
.controls{
  display:flex;align-items:center;gap:8px;
  padding:8px 0;
  flex-wrap:wrap;
}
.stats-badge{
  color:var(--text-dim);font-size:12px;
  background:var(--surface2);
  padding:3px 10px;border-radius:20px;
  border:1px solid var(--border2);
  white-space:nowrap;
}
.ctrl-spacer{flex:1}
.search-wrap{position:relative;display:flex;align-items:center}
.search-wrap svg{
  position:absolute;left:9px;pointer-events:none;
  color:var(--text-dim);
}
#filterBox{
  background:var(--surface);color:var(--text);
  border:1px solid var(--border);
  padding:5px 10px 5px 30px;
  border-radius:6px;width:220px;font-size:13px;
  transition:border-color .15s,box-shadow .15s;
  font-family:var(--font);
}
#filterBox::placeholder{color:var(--text-dim)}
#filterBox:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(88,166,255,.15)}

.ctrl-group{display:flex;align-items:center;gap:6px}
.ctrl-label{color:var(--text-muted);font-size:12px;white-space:nowrap}
select.ctrl-select{
  background:var(--surface);color:var(--text);
  border:1px solid var(--border);
  padding:5px 28px 5px 10px;border-radius:6px;
  font-size:13px;cursor:pointer;
  appearance:none;-webkit-appearance:none;
  background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='8' viewBox='0 0 12 8'%3E%3Cpath d='M1 1l5 5 5-5' stroke='%238b949e' stroke-width='1.5' fill='none' stroke-linecap='round'/%3E%3C/svg%3E");
  background-repeat:no-repeat;background-position:right 9px center;
  transition:border-color .15s;
  font-family:var(--font);
}
select.ctrl-select:focus{outline:none;border-color:var(--accent)}

.size-btns{display:flex;gap:2px}
.size-btn{
  background:var(--surface);color:var(--text-muted);
  border:1px solid var(--border);
  padding:4px 9px;font-size:11px;cursor:pointer;
  transition:background .15s,color .15s,border-color .15s;
  font-family:var(--font);
}
.size-btn:first-child{border-radius:6px 0 0 6px}
.size-btn:last-child{border-radius:0 6px 6px 0}
.size-btn:not(:last-child){border-right:none}
.size-btn.active,.size-btn:hover{background:var(--surface2);color:var(--text);border-color:var(--accent)}
.size-btn.active{color:var(--accent)}

/* ── Main content ─────────────────────────────────────────────────── */
.container{max-width:1800px;margin:0 auto;padding:20px}

/* ── Section header ───────────────────────────────────────────────── */
.section-head{
  display:flex;align-items:center;gap:8px;
  margin:24px 0 12px;
}
.section-head:first-child{margin-top:0}
.section-icon{color:var(--text-dim);display:flex;align-items:center}
.section-title{
  font-size:11px;font-weight:600;
  text-transform:uppercase;letter-spacing:.08em;
  color:var(--text-muted);
}
.section-count{
  font-size:11px;color:var(--text-dim);
  background:var(--surface2);
  padding:1px 7px;border-radius:10px;
  border:1px solid var(--border2);
}
.section-line{flex:1;height:1px;background:var(--border2)}

/* ── Directory grid ───────────────────────────────────────────────── */
.dirs{
  display:grid;
  grid-template-columns:repeat(auto-fill,minmax(var(--dir-min,180px),1fr));
  gap:8px;margin-bottom:4px;
}
.dir-card{
  display:flex;align-items:center;gap:10px;
  padding:10px 14px;
  background:var(--surface);
  border:1px solid var(--border2);
  border-radius:var(--radius);
  text-decoration:none;color:var(--text);
  transition:background .15s,border-color .15s,transform .1s;
  min-width:0;
  position:relative;overflow:hidden;
}
.dir-card::before{
  content:'';position:absolute;inset:0;
  background:linear-gradient(135deg,var(--accent-bg),transparent);
  opacity:0;transition:opacity .2s;
}
.dir-card:hover{
  background:var(--surface2);
  border-color:var(--border);
  transform:translateY(-1px);
}
.dir-card:hover::before{opacity:1}
.dir-icon{
  font-size:20px;flex-shrink:0;line-height:1;
  filter:drop-shadow(0 1px 2px rgba(0,0,0,.3));
}
.dir-name{
  font-size:13px;font-weight:500;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis;
  min-width:0;
}

/* ── Image grid ───────────────────────────────────────────────────── */
.img-grid{
  display:grid;
  grid-template-columns:repeat(auto-fill,minmax(var(--thumb,220px),1fr));
  gap:10px;
}
.img-card{
  background:var(--surface);
  border:1px solid var(--border2);
  border-radius:var(--radius-lg);
  overflow:hidden;cursor:pointer;
  transition:transform .18s cubic-bezier(.4,0,.2,1),
             box-shadow .18s cubic-bezier(.4,0,.2,1),
             border-color .15s;
  position:relative;
}
.img-card:hover{
  transform:translateY(-3px) scale(1.01);
  box-shadow:var(--shadow-hover);
  border-color:var(--border);
}
.img-card:active{transform:translateY(-1px) scale(1)}

/* skeleton shimmer */
.thumb-wrap{
  width:100%;aspect-ratio:1;
  position:relative;overflow:hidden;
  background:var(--surface2);
}
.thumb-wrap::after{
  content:'';position:absolute;inset:0;
  background:linear-gradient(90deg,transparent 0%,rgba(255,255,255,.04) 50%,transparent 100%);
  animation:shimmer 1.6s infinite;transform:translateX(-100%);
}
@keyframes shimmer{to{transform:translateX(100%)}}
.thumb-wrap img{
  position:absolute;inset:0;
  width:100%;height:100%;object-fit:cover;
  opacity:0;transition:opacity .3s ease;
}
.thumb-wrap img.loaded{opacity:1}
.thumb-wrap img.loaded ~ .thumb-wrap::after{display:none}

.img-label{
  padding:7px 10px 8px;
  font-size:11.5px;font-weight:500;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis;
  color:var(--text-muted);
  background:var(--surface);
  border-top:1px solid var(--border2);
}
.img-label .img-size{
  float:right;color:var(--text-dim);font-size:10.5px;margin-top:1px;
}

/* ── Video grid ───────────────────────────────────────────────────── */
.vid-thumb{
  width:100%;aspect-ratio:16/9;
  display:flex;align-items:center;justify-content:center;
  background:linear-gradient(135deg,#0d1117,#161b22);
  position:relative;overflow:hidden;
}
.vid-play-btn{
  width:48px;height:48px;
  background:rgba(88,166,255,.18);
  border:2px solid rgba(88,166,255,.5);
  border-radius:50%;
  display:flex;align-items:center;justify-content:center;
  transition:background .2s,transform .2s;
}
.img-card:hover .vid-play-btn{
  background:rgba(88,166,255,.3);transform:scale(1.1);
}
.vid-play-icon{
  width:0;height:0;
  border-style:solid;
  border-width:9px 0 9px 16px;
  border-color:transparent transparent transparent rgba(88,166,255,.9);
  margin-left:3px;
}

/* ── Files table ──────────────────────────────────────────────────── */
.files-table{
  width:100%;border-collapse:collapse;
  background:var(--surface);border-radius:var(--radius-lg);
  border:1px solid var(--border2);overflow:hidden;
  font-size:13px;
}
.files-table thead th{
  background:var(--surface2);
  color:var(--text-muted);font-weight:500;font-size:11px;
  text-transform:uppercase;letter-spacing:.06em;
  padding:8px 16px;text-align:left;
  border-bottom:1px solid var(--border2);
}
.files-table tbody tr{
  border-bottom:1px solid var(--border2);
  transition:background .12s;
}
.files-table tbody tr:last-child{border-bottom:none}
.files-table tbody tr:hover{background:var(--surface2)}
.files-table td{padding:9px 16px;vertical-align:middle}
.file-icon-cell{width:36px;padding-right:4px}
.file-ext-badge{
  display:inline-block;
  font-size:9px;font-weight:700;
  font-family:var(--font-mono);
  letter-spacing:.03em;text-transform:uppercase;
  padding:2px 5px;border-radius:3px;
  background:var(--surface3);color:var(--text-muted);
  border:1px solid var(--border);
  min-width:32px;text-align:center;
}
.file-ext-badge.py{background:#1c3048;color:#79c0ff;border-color:#1f4060}
.file-ext-badge.sh{background:#1a2d1a;color:#3fb950;border-color:#1f3d1f}
.file-ext-badge.json{background:#2a1f0a;color:#e3b341;border-color:#3a2a10}
.file-ext-badge.yaml,.file-ext-badge.yml{background:#2a1f0a;color:#e3b341;border-color:#3a2a10}
.file-ext-badge.txt,.file-ext-badge.md{background:#1e1e2e;color:#a5a5c5;border-color:#2e2e4e}
.file-ext-badge.pdf{background:#2d1010;color:#f85149;border-color:#3d1515}
.file-ext-badge.zip,.file-ext-badge.tar,.file-ext-badge.gz{background:#1e1e2e;color:#d2a8ff;border-color:#2e1e4e}
.file-ext-badge.pt,.file-ext-badge.pth,.file-ext-badge.ckpt,.file-ext-badge.bin{background:#0d1f2d;color:#56d364;border-color:#1a3040}
.file-ext-badge.npy,.file-ext-badge.npz{background:#0d2d1e;color:#3fb950;border-color:#1a4030}
.file-ext-badge.csv,.file-ext-badge.tsv{background:#1a2a10;color:#7ee787;border-color:#233a18}
.file-name-link{color:var(--accent);text-decoration:none;font-weight:500}
.file-name-link:hover{color:var(--accent-hover);text-decoration:underline}
.file-size-cell{color:var(--text-dim);font-family:var(--font-mono);font-size:12px;white-space:nowrap;text-align:right}
.file-date-cell{color:var(--text-dim);font-size:12px;white-space:nowrap}

/* ── Empty state ──────────────────────────────────────────────────── */
.empty-state{
  display:flex;flex-direction:column;align-items:center;justify-content:center;
  padding:80px 20px;gap:12px;color:var(--text-dim);
}
.empty-state svg{opacity:.3}
.empty-state p{font-size:14px}

/* ── Lightbox ─────────────────────────────────────────────────────── */
#lightbox{
  display:none;position:fixed;inset:0;z-index:1000;
  background:rgba(1,4,9,.96);
}
#lightbox.open{display:block}

.lb-header{
  position:fixed;top:0;left:0;right:0;
  display:flex;align-items:center;justify-content:space-between;
  padding:14px 20px;
  background:linear-gradient(rgba(1,4,9,.9),transparent);
  z-index:1010;
}
.lb-title{
  color:#c9d1d9;font-size:14px;font-weight:500;
  white-space:nowrap;overflow:hidden;text-overflow:ellipsis;
  max-width:60vw;
}
.lb-counter{
  color:var(--text-dim);font-size:13px;
  background:rgba(22,27,34,.8);
  padding:3px 12px;border-radius:20px;
  border:1px solid var(--border);
  white-space:nowrap;
}
.lb-close{
  width:36px;height:36px;border-radius:50%;
  background:rgba(30,36,44,.8);border:1px solid var(--border);
  color:#e6edf3;cursor:pointer;
  display:flex;align-items:center;justify-content:center;
  font-size:18px;line-height:1;
  transition:background .15s,border-color .15s;
  flex-shrink:0;
}
.lb-close:hover{background:rgba(248,81,73,.15);border-color:#f85149;color:#f85149}

#lbImage{
  position:fixed;
  top:50%;left:50%;
  transform:translate(-50%,-50%);
  max-width:90vw;max-height:85vh;
  object-fit:contain;border-radius:4px;
  box-shadow:0 8px 40px rgba(0,0,0,.8);
  z-index:1005;
}
#lbVideo{
  position:fixed;
  top:50%;left:50%;
  transform:translate(-50%,-50%);
  max-width:90vw;max-height:85vh;
  border-radius:4px;box-shadow:0 8px 40px rgba(0,0,0,.8);
  z-index:1005;
}

.lb-nav{
  position:fixed;top:50%;transform:translateY(-50%);
  width:44px;height:44px;border-radius:50%;
  background:rgba(22,27,34,.7);border:1px solid var(--border);
  color:#e6edf3;cursor:pointer;
  display:flex;align-items:center;justify-content:center;
  transition:background .15s,border-color .15s,transform .15s;
  user-select:none;z-index:1010;
}
.lb-nav:hover{background:rgba(88,166,255,.15);border-color:var(--accent);transform:translateY(-50%) scale(1.08)}
.lb-nav.prev{left:16px}
.lb-nav.next{right:16px}
.lb-nav svg{pointer-events:none}

/* ── No-results message ───────────────────────────────────────────── */
.no-results{
  display:none;text-align:center;padding:32px;
  color:var(--text-dim);font-size:13px;
}
.no-results.show{display:block}
.scroll-sentinel{height:40px;display:flex;align-items:center;justify-content:center}
.scroll-sentinel.loading::after{
  content:'';width:20px;height:20px;border:2px solid var(--surface3);
  border-top-color:var(--accent);border-radius:50%;animation:spin .6s linear infinite;
}
@keyframes spin{to{transform:rotate(360deg)}}

/* ── Responsive ───────────────────────────────────────────────────── */
@media(max-width:640px){
  .header{padding:0 12px}
  .container{padding:12px}
  .controls{gap:6px}
  #filterBox{width:140px}
  .dirs{grid-template-columns:repeat(auto-fill,minmax(140px,1fr))}
  .lb-nav{display:none}
  .lb-body{padding:60px 12px 20px}
}
</style>
</head>
<body>

<!-- ═══ Header ════════════════════════════════════════════════════════ -->
<header class="header">
<div class="header-inner">
  <div class="header-top">
    <button class="back-btn" onclick="history.back()" title="Go back">
      <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M10 3L5 8l5 5"/></svg>
    </button>
    <a class="back-btn" href="/browse/" title="Go to root">
      <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M8.354 1.146a.5.5 0 00-.708 0l-6.5 6.5a.5.5 0 00.146.854l.646.292V13.5A1.5 1.5 0 003.438 15h3.312V11h2.5v4h3.312a1.5 1.5 0 001.5-1.5V8.792l.646-.292a.5.5 0 00.146-.854l-6.5-6.5z"/></svg>
    </a>
    <button class="back-btn" onclick="refreshDir()" title="Refresh cache for this directory">
      <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><path d="M2.5 8a5.5 5.5 0 019.3-3.95M13.5 8a5.5 5.5 0 01-9.3 3.95"/><path d="M12 1v3.5h-3.5M4 15v-3.5h3.5" stroke-linejoin="round"/></svg>
    </button>
    <nav class="breadcrumb" aria-label="Path">
      <a href="/browse/">root</a>
      {{range $i,$c := .Breadcrumbs}}
      <span class="sep" aria-hidden="true">/</span>
      {{if eq $i (sub1 (len $.Breadcrumbs))}}<span class="crumb-last">{{$c.Name}}</span>
      {{else}}<a href="/browse/{{$c.Path}}">{{$c.Name}}</a>{{end}}
      {{end}}
    </nav>
  </div>
  <div class="controls">
    <span class="stats-badge">{{.Stats}}</span>
    <div class="ctrl-spacer"></div>
    <div class="search-wrap">
      <svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8">
        <circle cx="6.5" cy="6.5" r="5.5"/><path d="M11 11l3.5 3.5"/>
      </svg>
      <input id="filterBox" type="text" class="ctrl-select" placeholder="Filter..." oninput="filterItems()" autocomplete="off" spellcheck="false">
    </div>
    <div class="ctrl-group">
      <span class="ctrl-label">Zoom</span>
      <div class="size-btns" id="sizeBtns">
        <button class="size-btn" data-sz="140" onclick="setThumbSize(140)">S</button>
        <button class="size-btn active" data-sz="220" onclick="setThumbSize(220)">M</button>
        <button class="size-btn" data-sz="320" onclick="setThumbSize(320)">L</button>
        <button class="size-btn" data-sz="480" onclick="setThumbSize(480)">XL</button>
      </div>
    </div>
    <div class="ctrl-group">
      <span class="ctrl-label">Sort</span>
      <select class="ctrl-select" id="sortBy" onchange="localStorage.setItem('cx-sort',this.value);window.location.href='?sort='+this.value">
        <option value="name"{{if eq .Sort "name"}} selected{{end}}>Name</option>
        <option value="date"{{if eq .Sort "date"}} selected{{end}}>Newest</option>
        <option value="size"{{if eq .Sort "size"}} selected{{end}}>Size</option>
      </select>
    </div>
  </div>
</div>
</header>

<!-- ═══ Content ═══════════════════════════════════════════════════════ -->
<main class="container">

{{if .Dirs}}
<div class="section-head" data-section="dirs">
  <span class="section-icon">
    <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
      <path d="M1.75 1A1.75 1.75 0 000 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0016 13.25v-8.5A1.75 1.75 0 0014.25 3H7.5a.25.25 0 01-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1H1.75z"/>
    </svg>
  </span>
  <span class="section-title">Folders</span>
  <span class="section-count">{{len .Dirs}}</span>
  <span class="section-line"></span>
</div>
<div class="dirs" id="dirList">
{{range .Dirs}}<a class="dir-card" href="/browse/{{.Path}}" data-name="{{.Name}}">
  <span class="dir-icon">&#128193;</span>
  <span class="dir-name" title="{{.Name}}">{{.Name}}</span>
</a>
{{end}}
</div>
<div class="no-results" id="noResultsDirs">No matching folders</div>
{{end}}

{{if gt .TotalImages 0}}
<div class="section-head" data-section="images">
  <span class="section-icon"><svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path fill-rule="evenodd" d="M1.75 1A1.75 1.75 0 000 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0016 13.25V2.75A1.75 1.75 0 0014.25 1H1.75zM5.5 6a1.5 1.5 0 100-3 1.5 1.5 0 000 3z"/></svg></span>
  <span class="section-title">Images</span>
  <span class="section-count">{{.TotalImages}}</span>
  <span class="section-line"></span>
</div>
<div class="img-grid" id="imageGrid"></div>
<div id="imagesSentinel" class="scroll-sentinel" data-type="images" data-offset="0" data-total="{{.TotalImages}}"></div>
{{end}}

{{if gt .TotalVideos 0}}
<div class="section-head" data-section="videos">
  <span class="section-icon"><svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path fill-rule="evenodd" d="M1.75 2A1.75 1.75 0 000 3.75v8.5C0 13.216.784 14 1.75 14h12.5A1.75 1.75 0 0016 12.25v-8.5A1.75 1.75 0 0014.25 2H1.75zM6 10.37V5.63a.25.25 0 01.374-.218l4.44 2.37a.25.25 0 010 .436l-4.44 2.37A.25.25 0 016 10.37z"/></svg></span>
  <span class="section-title">Videos</span>
  <span class="section-count">{{.TotalVideos}}</span>
  <span class="section-line"></span>
</div>
<div class="img-grid" id="videoGrid"></div>
<div id="videosSentinel" class="scroll-sentinel" data-type="videos" data-offset="0" data-total="{{.TotalVideos}}"></div>
{{end}}

{{if gt .TotalFiles 0}}
<div class="section-head" data-section="files">
  <span class="section-icon"><svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path fill-rule="evenodd" d="M3.75 1.5a.25.25 0 00-.25.25v11.5c0 .138.112.25.25.25h8.5a.25.25 0 00.25-.25V6H9.75A1.75 1.75 0 018 4.25V1.5H3.75zm5.75.56v2.19c0 .138.112.25.25.25h2.19L9.5 2.06zM2 1.75C2 .784 2.784 0 3.75 0h5.086c.464 0 .909.184 1.237.513l3.414 3.414c.329.328.513.773.513 1.237v8.586A1.75 1.75 0 0112.25 15h-8.5A1.75 1.75 0 012 13.25V1.75z"/></svg></span>
  <span class="section-title">Files</span>
  <span class="section-count">{{.TotalFiles}}</span>
  <span class="section-line"></span>
</div>
<table class="files-table" id="fileList"><thead><tr><th class="file-icon-cell"></th><th>Name</th><th style="text-align:right">Size</th></tr></thead><tbody></tbody></table>
<div id="filesSentinel" class="scroll-sentinel" data-type="files" data-offset="0" data-total="{{.TotalFiles}}"></div>
{{end}}

{{if and (not .Dirs) (eq .TotalImages 0) (eq .TotalVideos 0) (eq .TotalFiles 0)}}
<div class="empty-state">
  <svg width="48" height="48" viewBox="0 0 16 16" fill="currentColor">
    <path fill-rule="evenodd" d="M1.75 1.5a.25.25 0 00-.25.25v11.5c0 .138.112.25.25.25H4v-1.25a3.75 3.75 0 117.5 0V13.5h2.25a.25.25 0 00.25-.25V1.75a.25.25 0 00-.25-.25H1.75z"/>
  </svg>
  <p>This directory is empty</p>
</div>
{{end}}

</main>

<!-- ═══ Lightbox ═══════════════════════════════════════════════════════ -->
<div id="lightbox">
  <div class="lb-header">
    <span class="lb-title" id="lbTitle"></span>
    <span class="lb-counter" id="lbCounter"></span>
    <button class="lb-close" onclick="closeLB()">&#10005;</button>
  </div>
  <img id="lbImage" src="">
  <video id="lbVideo" controls style="display:none"></video>
  <button class="lb-nav prev" onclick="navLB(-1)">
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
      <path d="M10 3L5 8l5 5"/>
    </svg>
  </button>
  <button class="lb-nav next" onclick="navLB(1)">
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
      <path d="M6 3l5 5-5 5"/>
    </svg>
  </button>
</div>

<script>
(function(){
  var u=new URL(window.location);
  if(!u.searchParams.has('sort')){
    var s=localStorage.getItem('cx-sort');
    if(s){u.searchParams.set('sort',s);window.location.replace(u);return;}
  }
})();
const images=[];
let ci=0,lbMode='';
const currentPath='{{js .CurrentPath}}'.replace(/^\/|\/$/g,'');

// ── Refresh ─────────────────────────────────────────────────────────
function refreshDir(){
  var btn=event.currentTarget;
  btn.style.opacity='0.5';btn.style.pointerEvents='none';
  fetch('/refresh/'+currentPath).then(function(r){return r.json()}).then(function(){
    window.location.reload();
  });
}

// ── Lightbox ────────────────────────────────────────────────────────
function openLightbox(i){
  ci=i;lbMode='image';
  document.getElementById('lbVideo').pause();
  document.getElementById('lbVideo').style.display='none';
  var img=document.getElementById('lbImage');
  img.style.display='block';
  img.src='/raw/'+images[ci].p;
  document.getElementById('lbTitle').textContent=images[ci].n;
  document.getElementById('lbCounter').textContent=(ci+1)+' / '+images.length;
  document.getElementById('lightbox').className='open';
  document.querySelector('.lb-nav.prev').style.display='';
  document.querySelector('.lb-nav.next').style.display='';
  document.body.style.overflow='hidden';
}
function closeLB(){
  document.getElementById('lightbox').className='';
  document.body.style.overflow='';
  var v=document.getElementById('lbVideo');v.pause();v.src='';v.style.display='none';
  var img=document.getElementById('lbImage');img.src='';img.style.display='none';
  document.querySelector('.lb-nav.prev').style.display='';
  document.querySelector('.lb-nav.next').style.display='';
}
function navLB(d){
  if(lbMode!=='image')return;
  ci=(ci+d+images.length)%images.length;
  var img=document.getElementById('lbImage');
  img.src='/raw/'+images[ci].p;
  document.getElementById('lbTitle').textContent=images[ci].n;
  document.getElementById('lbCounter').textContent=(ci+1)+' / '+images.length;
}
function openVideoLB(path,name){
  lbMode='video';
  document.getElementById('lbImage').style.display='none';
  document.querySelector('.lb-nav.prev').style.display='none';
  document.querySelector('.lb-nav.next').style.display='none';
  var v=document.getElementById('lbVideo');
  v.style.display='block';v.src=path;
  document.getElementById('lbTitle').textContent=name;
  document.getElementById('lbCounter').textContent='';
  document.getElementById('lightbox').className='open';
  document.body.style.overflow='hidden';
  v.play().catch(function(){});
}

// ── Keyboard navigation ─────────────────────────────────────────────
document.addEventListener('keydown',e=>{
  const lb=document.getElementById('lightbox');
  if(!lb.classList.contains('open'))return;
  if(e.key==='Escape')closeLB();
  else if(e.key==='ArrowRight')navLB(1);
  else if(e.key==='ArrowLeft')navLB(-1);
});
// Click backdrop to close
document.getElementById('lightbox').addEventListener('click',function(e){
  if(e.target===document.getElementById('lightbox'))closeLB();
});

// ── Zoom ─────────────────────────────────────────────────────────────
const zoomLevels={140:{dir:140,dirIcon:16,dirFont:12,dirPad:'7px 10px',fileFont:12,filePad:'6px 14px'},220:{dir:180,dirIcon:20,dirFont:13,dirPad:'10px 14px',fileFont:13,filePad:'9px 16px'},320:{dir:240,dirIcon:26,dirFont:15,dirPad:'14px 18px',fileFont:15,filePad:'12px 18px'},480:{dir:320,dirIcon:34,dirFont:18,dirPad:'18px 22px',fileFont:17,filePad:'14px 20px'}};
function setThumbSize(px,btn){
  const s=document.documentElement.style;
  const z=zoomLevels[px]||zoomLevels[220];
  s.setProperty('--thumb',px+'px');
  s.setProperty('--dir-min',z.dir+'px');
  document.querySelectorAll('.dir-card').forEach(el=>{
    el.style.padding=z.dirPad;
    el.style.fontSize=z.dirFont+'px';
  });
  document.querySelectorAll('.dir-icon').forEach(el=>{el.style.fontSize=z.dirIcon+'px'});
  document.querySelectorAll('.files-table td').forEach(el=>{
    el.style.padding=z.filePad;
    el.style.fontSize=z.fileFont+'px';
  });
  document.querySelectorAll('.file-ext-badge').forEach(el=>{
    el.style.fontSize=(z.fileFont-4)+'px';
    el.style.padding=(px>300?'3px 7px':'2px 5px');
  });
  document.querySelectorAll('.size-btn').forEach(b=>b.classList.remove('active'));
  const match=document.querySelector('.size-btn[data-sz="'+px+'"]');
  if(match)match.classList.add('active');
  localStorage.setItem('imgbrowse-zoom',px);
}
// Restore saved zoom on page load
(function(){
  const saved=localStorage.getItem('imgbrowse-zoom');
  if(saved)setThumbSize(parseInt(saved));
})()

// ── Infinite Scroll ──────────────────────────────────────────────────
function fetchItems(sentinel){
  if(sentinel.dataset.done==='1')return;
  sentinel.classList.add('loading');
  const type=sentinel.dataset.type;
  const offset=parseInt(sentinel.dataset.offset);
  const sort=document.getElementById('sortBy').value;
  fetch('/api/'+currentPath+'?type='+type+'&offset='+offset+'&sort='+sort)
    .then(function(r){return r.json()})
    .then(function(data){
      sentinel.classList.remove('loading');
      const grid=document.getElementById(type==='images'?'imageGrid':type==='videos'?'videoGrid':'fileList');
      data.items.forEach(function(item){
        if(type==='images'){
          var idx=images.length;
          images.push({p:item.Path,n:item.Name});
          var div=document.createElement('div');
          div.className='img-card';div.dataset.name=item.Name;
          div.onclick=(function(i){return function(){openLightbox(i)}})(idx);
          div.title=item.Name;
          div.innerHTML='<div class="thumb-wrap"><img src="/thumb/'+item.Path+'" loading="lazy" onload="this.classList.add(\'loaded\')"></div><div class="img-label"><span class="img-size">'+item.Size+'</span>'+item.Name+'</div>';
          grid.appendChild(div);
        } else if(type==='videos'){
          var div=document.createElement('div');
          div.className='img-card';div.dataset.name=item.Name;
          div.onclick=(function(p,n){return function(){openVideoLB('/raw/'+p,n)}})(item.Path,item.Name);
          div.innerHTML='<div class="vid-thumb"><div class="vid-play-btn"><div class="vid-play-icon"></div></div></div><div class="img-label"><span class="img-size">'+item.Size+'</span>'+item.Name+'</div>';
          grid.appendChild(div);
        } else {
          var ext=item.Name.split('.').pop().toLowerCase();
          var cls={py:'py',sh:'sh',json:'json',yaml:'yaml',yml:'yaml',txt:'txt',md:'txt',pdf:'pdf',zip:'zip',tar:'zip',gz:'zip',pt:'pt',pth:'pt',ckpt:'pt',npy:'npy',npz:'npy',csv:'csv'}[ext]||'';
          var tr=document.createElement('tr');
          tr.dataset.name=item.Name;
          tr.innerHTML='<td class="file-icon-cell"><span class="file-ext-badge '+cls+'">'+ext.toUpperCase()+'</span></td><td><a class="file-name-link" href="/raw/'+item.Path+'">'+item.Name+'</a></td><td class="file-size-cell">'+item.Size+'</td>';
          grid.querySelector('tbody').appendChild(tr);
        }
      });
      if(data.hasMore){
        sentinel.dataset.offset=offset+data.items.length;
      } else {
        sentinel.dataset.done='1';
        sentinel.style.display='none';
      }
      // Re-apply zoom to new elements
      var saved=localStorage.getItem('imgbrowse-zoom');
      if(saved)setThumbSize(parseInt(saved));
    });
}

// Set up IntersectionObserver for all sentinels
var scrollObserver=new IntersectionObserver(function(entries){
  entries.forEach(function(e){
    if(e.isIntersecting)fetchItems(e.target);
  });
},{rootMargin:'400px'});
document.querySelectorAll('.scroll-sentinel').forEach(function(s){scrollObserver.observe(s)});

// ── Filter ───────────────────────────────────────────────────────────
function filterItems(){
  var q=document.getElementById('filterBox').value.toLowerCase().trim();
  document.querySelectorAll('#dirList .dir-card').forEach(function(el){
    el.style.display=(!q||el.dataset.name.toLowerCase().includes(q))?'':'none';
  });
  document.querySelectorAll('.img-card').forEach(function(el){
    el.style.display=(!q||el.dataset.name.toLowerCase().includes(q))?'':'none';
  });
  document.querySelectorAll('#fileList tbody tr').forEach(function(el){
    el.style.display=(!q||el.dataset.name.toLowerCase().includes(q))?'':'none';
  });
}

</script>
</body>
</html>`
