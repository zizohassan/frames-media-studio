// framespdf: Gin web app to upload videos → extract frames → PDF, upload images → order → PDF,
// and upload audio → inspect (ffprobe) → convert (ffmpeg).
//
// Prereqs: ffmpeg, ffprobe, ImageMagick (magick or convert) in PATH.
// Run: go run main.go  (then open http://localhost:8080)

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	addr     = ":5060"
	workRoot = "./work"
)

var (
	uploadDir = filepath.Join(workRoot, "uploads")
	framesDir = filepath.Join(workRoot, "frames")
	pdfsDir   = filepath.Join(workRoot, "pdfs")
	audioDir  = filepath.Join(workRoot, "audio")
)

type VideoMeta struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	RelPath   string  `json:"rel_path"`
	AbsPath   string  `json:"-"`
	SizeBytes int64   `json:"size_bytes"`
	DurationS float64 `json:"duration_seconds"`
	Uploaded  string  `json:"uploaded_at"`
}

type ImgMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RelPath   string `json:"rel_path"`
	AbsPath   string `json:"-"`
	SizeBytes int64  `json:"size_bytes"`
	Uploaded  string `json:"uploaded_at"`
	URL       string `json:"url"`
}

type AudioMeta struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	RelPath     string  `json:"rel_path"`
	AbsPath     string  `json:"-"`
	SizeBytes   int64   `json:"size_bytes"`
	Uploaded    string  `json:"uploaded_at"`
	DurationS   float64 `json:"duration_seconds"`
	Codec       string  `json:"codec"`
	Channels    int     `json:"channels"`
	SampleRate  int     `json:"sample_rate"`
	BitrateKbps int     `json:"bitrate_kbps"`
	ProbeJSON   string  `json:"probe_json"`
}

var (
	mu     sync.Mutex
	videos = map[string]*VideoMeta{}
	images = map[string]*ImgMeta{}
	audios = map[string]*AudioMeta{}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	must(os.MkdirAll(uploadDir, 0o755))
	must(os.MkdirAll(framesDir, 0o755))
	must(os.MkdirAll(pdfsDir, 0o755))
	must(os.MkdirAll(audioDir, 0o755))

	// tools
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("ffmpeg not found in PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		log.Fatal("ffprobe not found in PATH")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		if _, err2 := exec.LookPath("convert"); err2 != nil {
			log.Fatal("ImageMagick not found (magick/convert)")
		}
	}

	r := gin.Default()
	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, indexHTML)
	})

	// videos
	r.POST("/upload", handleUploadVideos)
	r.POST("/process", handleProcessVideos)

	// images
	r.POST("/upload_images", handleUploadImages)
	r.POST("/images_pdf", handleImagesPDF)

	// audio
	r.POST("/upload_audio", handleUploadAudio)
	r.POST("/convert_audio", handleConvertAudio)

	// static
	r.StaticFS("/download", http.Dir(pdfsDir))
	r.StaticFS("/uploads", http.Dir(uploadDir))
	r.StaticFS("/audio", http.Dir(audioDir))

	log.Printf("📦 work dir: %s", workRoot)
	log.Printf("🌐 open: http://localhost%s", addr)
	_ = r.Run(addr)
}

// ===== videos =====

type processReq struct {
	Items []struct {
		ID  string  `json:"id"`
		FPS float64 `json:"fps"`
	} `json:"items"`
	JPEGQuality int `json:"jpeg_quality"`
	Density     int `json:"pdf_density"`
	Quality     int `json:"pdf_quality"`
}

type processItem struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	DurationS   float64 `json:"duration_seconds"`
	FPS         float64 `json:"fps"`
	EstFrames   int     `json:"estimated_frames"`
	FramesWrote int     `json:"frames_wrote"`
	PDFURL      string  `json:"pdf_url"`
}

func handleUploadVideos(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 20<<30)
	if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
		c.String(http.StatusBadRequest, "failed to parse form: %v", err)
		return
	}
	files := c.Request.MultipartForm.File["videos"]
	if len(files) == 0 {
		c.String(http.StatusBadRequest, "no files uploaded (field must be 'videos')")
		return
	}
	out := make([]*VideoMeta, 0, len(files))
	for _, fh := range files {
		fr, err := fh.Open()
		if err != nil {
			c.String(http.StatusInternalServerError, "open: %v", err)
			return
		}
		defer fr.Close()
		id := randID(8)
		safe := sanitizeName(fh.Filename)
		rel := filepath.Join(id, safe)
		abs := filepath.Join(uploadDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			c.String(http.StatusInternalServerError, "mkdir: %v", err)
			return
		}
		fw, err := os.Create(abs)
		if err != nil {
			c.String(http.StatusInternalServerError, "create: %v", err)
			return
		}
		wrote, cpErr := ioCopyClose(fw, fr)
		if cpErr != nil {
			c.String(http.StatusInternalServerError, "write: %v", cpErr)
			return
		}
		dur, _ := probeDuration(abs)
		vm := &VideoMeta{ID: id, Name: safe, RelPath: rel, AbsPath: abs, SizeBytes: wrote, DurationS: dur, Uploaded: time.Now().Format(time.RFC3339)}
		mu.Lock()
		videos[id] = vm
		mu.Unlock()
		out = append(out, vm)
	}
	c.JSON(http.StatusOK, gin.H{"videos": out})
}

func handleProcessVideos(c *gin.Context) {
	var req processReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if len(req.Items) == 0 {
		c.String(http.StatusBadRequest, "no items provided")
		return
	}
	if req.JPEGQuality == 0 {
		req.JPEGQuality = 2
	}
	if req.Density == 0 {
		req.Density = 150
	}
	if req.Quality == 0 {
		req.Quality = 92
	}
	results := make([]processItem, 0, len(req.Items))
	for _, it := range req.Items {
		mu.Lock()
		vm := videos[it.ID]
		mu.Unlock()
		if vm == nil {
			c.String(http.StatusBadRequest, "unknown video id: %s", it.ID)
			return
		}
		fps := it.FPS
		if !(fps > 0) {
			fps = 1
		}
		frameDir := filepath.Join(framesDir, vm.ID)
		_ = os.MkdirAll(frameDir, 0o755)
		pattern := filepath.Join(frameDir, "frame_%05d.jpg")
		wrote, err := extractFrames(vm.AbsPath, pattern, fps, req.JPEGQuality)
		if err != nil {
			c.String(http.StatusInternalServerError, "ffmpeg extraction failed for %s: %v", vm.Name, err)
			return
		}
		imgs, _ := filepath.Glob(filepath.Join(frameDir, "frame_*.jpg"))
		sort.Strings(imgs)
		if len(imgs) == 0 {
			c.String(http.StatusInternalServerError, "no frames extracted")
			return
		}
		pdfPath := filepath.Join(pdfsDir, vm.ID+"_"+stripExt(vm.Name)+".pdf")
		if err := imagesToPDF(imgs, pdfPath, req.Density, req.Quality); err != nil {
			c.String(http.StatusInternalServerError, "pdf build failed: %v", err)
			return
		}
		results = append(results, processItem{
			ID:          vm.ID,
			Name:        vm.Name,
			DurationS:   vm.DurationS,
			FPS:         fps,
			EstFrames:   int(math.Ceil(vm.DurationS * fps)),
			FramesWrote: wrote,
			PDFURL:      "/download/" + filepath.Base(pdfPath),
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": results})
}

// ===== images =====

type imagesUploadResp struct {
	Images []*ImgMeta `json:"images"`
}

type imagesPDFReq struct {
	Items []struct {
		ID    string `json:"id"`
		Order int    `json:"order"`
	} `json:"items"`
	Density int    `json:"pdf_density"`
	Quality int    `json:"pdf_quality"`
	OutName string `json:"out_name"`
}

func handleUploadImages(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 5<<30)
	if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
		c.String(http.StatusBadRequest, "failed to parse form: %v", err)
		return
	}
	files := c.Request.MultipartForm.File["images"]
	if len(files) == 0 {
		c.String(http.StatusBadRequest, "no files uploaded (field must be 'images')")
		return
	}
	out := make([]*ImgMeta, 0, len(files))
	for _, fh := range files {
		fr, err := fh.Open()
		if err != nil {
			c.String(http.StatusInternalServerError, "open: %v", err)
			return
		}
		defer fr.Close()
		id := randID(8)
		safe := sanitizeName(fh.Filename)
		rel := filepath.Join(id, safe)
		abs := filepath.Join(uploadDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			c.String(http.StatusInternalServerError, "mkdir: %v", err)
			return
		}
		fw, err := os.Create(abs)
		if err != nil {
			c.String(http.StatusInternalServerError, "create: %v", err)
			return
		}
		wrote, cpErr := ioCopyClose(fw, fr)
		if cpErr != nil {
			c.String(http.StatusInternalServerError, "write: %v", cpErr)
			return
		}
		im := &ImgMeta{ID: id, Name: safe, RelPath: rel, AbsPath: abs, SizeBytes: wrote, Uploaded: time.Now().Format(time.RFC3339), URL: "/uploads/" + rel}
		mu.Lock()
		images[id] = im
		mu.Unlock()
		out = append(out, im)
	}
	c.JSON(http.StatusOK, imagesUploadResp{Images: out})
}

func handleImagesPDF(c *gin.Context) {
	var req imagesPDFReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if len(req.Items) == 0 {
		c.String(http.StatusBadRequest, "no items provided")
		return
	}
	if req.Density == 0 {
		req.Density = 150
	}
	if req.Quality == 0 {
		req.Quality = 92
	}
	sort.SliceStable(req.Items, func(i, j int) bool { return req.Items[i].Order < req.Items[j].Order })
	paths := make([]string, 0, len(req.Items))
	for _, it := range req.Items {
		mu.Lock()
		im := images[it.ID]
		mu.Unlock()
		if im == nil {
			c.String(http.StatusBadRequest, "unknown image id: %s", it.ID)
			return
		}
		paths = append(paths, im.AbsPath)
	}
	if len(paths) == 0 {
		c.String(http.StatusBadRequest, "no valid images")
		return
	}
	name := sanitizeName(req.OutName)
	if name == "" {
		name = "images_" + time.Now().Format("20060102_150405") + "_" + randID(4) + ".pdf"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
		name += ".pdf"
	}
	pdfPath := filepath.Join(pdfsDir, name)
	if err := imagesToPDF(paths, pdfPath, req.Density, req.Quality); err != nil {
		c.String(http.StatusInternalServerError, "pdf build failed: %v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"pdf_url": "/download/" + filepath.Base(pdfPath), "count": len(paths)})
}

// ===== audio =====

type audioUploadResp struct {
	Audios []*AudioMeta `json:"audios"`
}

type convertAudioReq struct {
	Items []struct {
		ID          string `json:"id"`
		Format      string `json:"format"`
		BitrateKbps int    `json:"bitrate_kbps"`
		SampleRate  int    `json:"sample_rate"`
		Channels    int    `json:"channels"`
	} `json:"items"`
}

type convertAudioItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
	OutURL string `json:"out_url"`
}

func handleUploadAudio(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 5<<30)
	if err := c.Request.ParseMultipartForm(64 << 20); err != nil {
		c.String(http.StatusBadRequest, "failed to parse form: %v", err)
		return
	}
	files := c.Request.MultipartForm.File["audios"]
	if len(files) == 0 {
		c.String(http.StatusBadRequest, "no files uploaded (field must be 'audios')")
		return
	}
	out := make([]*AudioMeta, 0, len(files))
	for _, fh := range files {
		fr, err := fh.Open()
		if err != nil {
			c.String(http.StatusInternalServerError, "open: %v", err)
			return
		}
		defer fr.Close()
		id := randID(8)
		safe := sanitizeName(fh.Filename)
		rel := filepath.Join(id, safe)
		abs := filepath.Join(uploadDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			c.String(http.StatusInternalServerError, "mkdir: %v", err)
			return
		}
		fw, err := os.Create(abs)
		if err != nil {
			c.String(http.StatusInternalServerError, "create: %v", err)
			return
		}
		wrote, cpErr := ioCopyClose(fw, fr)
		if cpErr != nil {
			c.String(http.StatusInternalServerError, "write: %v", cpErr)
			return
		}
		dur, codec, ch, sr, br, raw, _ := probeAudioJSON(abs)
		am := &AudioMeta{ID: id, Name: safe, RelPath: rel, AbsPath: abs, SizeBytes: wrote, Uploaded: time.Now().Format(time.RFC3339), DurationS: dur, Codec: codec, Channels: ch, SampleRate: sr, BitrateKbps: br, ProbeJSON: raw}
		mu.Lock()
		audios[id] = am
		mu.Unlock()
		out = append(out, am)
	}
	c.JSON(http.StatusOK, audioUploadResp{Audios: out})
}

func handleConvertAudio(c *gin.Context) {
	var req convertAudioReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "bad json: %v", err)
		return
	}
	if len(req.Items) == 0 {
		c.String(http.StatusBadRequest, "no items provided")
		return
	}
	res := make([]convertAudioItem, 0, len(req.Items))
	for _, it := range req.Items {
		mu.Lock()
		am := audios[it.ID]
		mu.Unlock()
		if am == nil {
			c.String(http.StatusBadRequest, "unknown audio id: %s", it.ID)
			return
		}
		outPath, err := convertAudio(am.AbsPath, am.Name, it.Format, it.BitrateKbps, it.SampleRate, it.Channels)
		if err != nil {
			c.String(http.StatusInternalServerError, "convert failed for %s: %v", am.Name, err)
			return
		}
		res = append(res, convertAudioItem{ID: am.ID, Name: am.Name, Format: strings.ToUpper(it.Format), OutURL: "/audio/" + filepath.Base(outPath)})
	}
	c.JSON(http.StatusOK, gin.H{"results": res})
}

// ===== helpers / exec =====

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func ioCopyClose(dst *os.File, src io.Reader) (int64, error) {
	wrote, cpErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if cpErr != nil {
		return wrote, cpErr
	}
	return wrote, closeErr
}

func sanitizeName(s string) string {
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.TrimSpace(s)
	if s == "" {
		s = "file"
	}
	return s
}

func stripExt(s string) string {
	ext := filepath.Ext(s)
	return strings.TrimSuffix(s, ext)
}

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func probeDuration(file string) (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", file)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "N/A" || s == "" {
		return 0, errors.New("no duration")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

func extractFrames(inPath, outPattern string, fps float64, jpegQ int) (int, error) {
	filter := fmt.Sprintf("fps=%g:round=up:start_time=0", fps)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-fflags", "+genpts",
		"-i", inPath,
		"-map", "0:v:0",
		"-vsync", "vfr",
		"-vf", filter,
		"-q:v", strconv.Itoa(jpegQ),
		outPattern,
	}
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	files, _ := filepath.Glob(strings.ReplaceAll(outPattern, "%05d", "*"))
	return len(files), nil
}

func imagesToPDF(imgs []string, outPDF string, density int, quality int) error {
	bin := "magick"
	if _, err := exec.LookPath(bin); err != nil {
		bin = "convert"
	}
	args := []string{}
	for _, img := range imgs {
		args = append(args, img, "-auto-orient")
	}
	args = append(args, "-density", strconv.Itoa(density), "-quality", strconv.Itoa(quality), outPDF)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func probeAudioJSON(file string) (duration float64, codec string, channels int, sampleRate int, bitrateKbps int, rawJSON string, err error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", file)
	out, e := cmd.Output()
	if e != nil {
		err = e
		return
	}
	rawJSON = string(out)
	var pr struct {
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
			BitRate    string `json:"bit_rate"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(out, &pr)
	if pr.Format.Duration != "" {
		f, _ := strconv.ParseFloat(pr.Format.Duration, 64)
		if f > 0 {
			duration = f
		}
	}
	if pr.Format.BitRate != "" {
		b, _ := strconv.Atoi(pr.Format.BitRate)
		if b > 0 {
			bitrateKbps = b / 1000
		}
	}
	for _, s := range pr.Streams {
		if s.CodecType == "audio" {
			codec = s.CodecName
			if s.Channels > 0 {
				channels = s.Channels
			}
			if s.SampleRate != "" {
				v, _ := strconv.Atoi(s.SampleRate)
				if v > 0 {
					sampleRate = v
				}
			}
			if s.BitRate != "" {
				b2, _ := strconv.Atoi(s.BitRate)
				if b2 > 0 {
					bitrateKbps = b2 / 1000
				}
			}
			break
		}
	}
	return
}

func convertAudio(inAbs string, inName string, format string, bitrateKbps, sampleRate, channels int) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "mp3"
	}
	base := stripExt(inName)
	ext := "." + format
	out := filepath.Join(audioDir, base+ext)

	codec := ""
	switch format {
	case "mp3":
		codec = "libmp3lame"
	case "wav":
		codec = "pcm_s16le"
	case "flac":
		codec = "flac"
	case "aac":
		codec = "aac"
	case "ogg":
		codec = "libvorbis"
	case "opus":
		codec = "libopus"
	default:
		return "", fmt.Errorf("unsupported format: %s", format)
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", inAbs, "-vn", "-c:a", codec}
	if sampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(sampleRate))
	}
	if channels == 1 || channels == 2 {
		args = append(args, "-ac", strconv.Itoa(channels))
	}
	if bitrateKbps > 0 {
		if format == "mp3" || format == "aac" || format == "ogg" || format == "opus" {
			args = append(args, "-b:a", fmt.Sprintf("%dk", bitrateKbps))
		}
	}
	args = append(args, out)
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

// ===== HTML =====

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Frames & PDFs</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <script>
    tailwind.config = {
      theme: {
        extend: {
          fontFamily: {
            'mono': ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace']
          }
        }
      }
    }
  </script>
</head>
<body class="bg-gray-50 font-sans text-gray-900 p-6 max-w-6xl mx-auto">
  <div class="mb-8">
    <h1 class="text-3xl font-bold text-gray-900 mb-2">Video → Frames → PDF</h1>
    <p class="text-gray-600">Convert videos to frames and generate PDFs with advanced processing options</p>
  </div>

  <div class="bg-white rounded-xl shadow-sm border border-gray-200 p-6 mb-6">
    <form id="upForm" class="space-y-4">
      <div>
        <label class="block text-sm font-semibold text-gray-700 mb-2">Select videos</label>
        <p class="text-sm text-gray-500 mb-3">You can pick multiple files</p>
        <div class="flex items-center gap-3">
          <input id="videos" name="videos" type="file" accept="video/*" multiple 
                 class="block w-full text-sm text-gray-500 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:text-sm file:font-medium file:bg-blue-50 file:text-blue-700 hover:file:bg-blue-100 file:cursor-pointer" />
          <button type="submit" class="px-6 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 transition-colors font-medium">
            Upload
          </button>
        </div>
      </div>
    </form>
    <div class="mt-4 p-3 bg-amber-50 border border-amber-200 rounded-lg">
      <p class="text-sm text-amber-800">
        <span class="font-medium">Requirements:</span> Requires ffmpeg & ImageMagick on the server. 
        PDFs will be available under <span class="font-mono bg-amber-100 px-1 rounded">/download/…</span>
      </p>
    </div>
  </div>

  <div id="list" class="bg-white rounded-xl shadow-sm border border-gray-200 p-6 mb-6" style="display:none;">
    <div class="grid grid-cols-5 gap-4 items-center pb-3 border-b border-gray-200 mb-4">
      <div class="font-semibold text-gray-700">File</div>
      <div class="font-semibold text-gray-700">Duration</div>
      <div class="font-semibold text-gray-700">FPS</div>
      <div class="font-semibold text-gray-700">Est. Frames</div>
      <div class="font-semibold text-gray-700">Info</div>
    </div>
    <div id="rows" class="space-y-3"></div>
    <div class="mt-6 pt-6 border-t border-gray-200">
      <div class="flex flex-wrap items-center gap-4">
        <div class="flex items-center gap-2">
          <label class="text-sm font-medium text-gray-700">JPEG quality:</label>
          <input id="jpegq" type="number" min="2" max="31" step="1" value="2" 
                 class="w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div class="flex items-center gap-2">
          <label class="text-sm font-medium text-gray-700">PDF density:</label>
          <input id="density" type="number" min="72" step="1" value="150" 
                 class="w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <div class="flex items-center gap-2">
          <label class="text-sm font-medium text-gray-700">PDF quality:</label>
          <input id="pdfq" type="number" min="1" max="100" step="1" value="92" 
                 class="w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-blue-500 focus:border-blue-500" />
        </div>
        <button id="goBtn" class="px-6 py-2 bg-green-600 text-white rounded-lg hover:bg-green-700 transition-colors font-medium">
          Process → PDF
        </button>
      </div>
    </div>
  </div>

  <div id="results" class="bg-white rounded-xl shadow-sm border border-gray-200 p-6 mb-8" style="display:none;"></div>

  <div class="mb-8">
    <h2 class="text-3xl font-bold text-gray-900 mb-2">Images → PDF</h2>
    <p class="text-gray-600">Combine multiple images into a single PDF document</p>
  </div>

  <div class="bg-white rounded-xl shadow-sm border border-gray-200 p-6 mb-6">
    <form id="imgForm" class="space-y-4">
      <div>
        <label class="block text-sm font-semibold text-gray-700 mb-2">Select images</label>
        <p class="text-sm text-gray-500 mb-3">You can pick multiple files in any order</p>
        <div class="flex items-center gap-3">
          <input id="imgs" name="images" type="file" accept="image/*" multiple 
                 class="block w-full text-sm text-gray-500 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:text-sm file:font-medium file:bg-purple-50 file:text-purple-700 hover:file:bg-purple-100 file:cursor-pointer" />
          <button type="submit" class="px-6 py-2 bg-purple-600 text-white rounded-lg hover:bg-purple-700 transition-colors font-medium">
            Upload
          </button>
        </div>
      </div>
    </form>
    
    <div id="imgList" class="mt-6" style="display:none;">
      <div class="p-3 bg-blue-50 border border-blue-200 rounded-lg mb-4">
        <p class="text-sm text-blue-800">
          <span class="font-medium">Tip:</span> Set the <strong>Order</strong> for each image (1..N). Lower numbers appear first. You can leave gaps—ordering is sorted ascending.
        </p>
      </div>
      <div id="thumbs" class="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 gap-4 mb-6"></div>
      <div class="pt-6 border-t border-gray-200">
        <div class="flex flex-wrap items-center gap-4">
          <div class="flex items-center gap-2">
            <label class="text-sm font-medium text-gray-700">PDF density:</label>
            <input id="idensity" type="number" min="72" step="1" value="150" 
                   class="w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-purple-500 focus:border-purple-500" />
          </div>
          <div class="flex items-center gap-2">
            <label class="text-sm font-medium text-gray-700">PDF quality:</label>
            <input id="iquality" type="number" min="1" max="100" step="1" value="92" 
                   class="w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-purple-500 focus:border-purple-500" />
          </div>
          <div class="flex items-center gap-2">
            <label class="text-sm font-medium text-gray-700">Output name:</label>
            <input id="iname" type="text" placeholder="optional e.g. album.pdf" 
                   class="w-40 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-purple-500 focus:border-purple-500" />
          </div>
          <button id="imgGo" type="button" class="px-6 py-2 bg-purple-600 text-white rounded-lg hover:bg-purple-700 transition-colors font-medium">
            Build Images → PDF
          </button>
        </div>
      </div>
    </div>
    <div id="imgResult" class="mt-6" style="display:none;"></div>
  </div>

  <div class="mb-8">
    <h2 class="text-3xl font-bold text-gray-900 mb-2">Audio → Inspect & Convert</h2>
    <p class="text-gray-600">Analyze audio files and convert between different formats</p>
  </div>

  <div class="bg-white rounded-xl shadow-sm border border-gray-200 p-6 mb-6">
    <form id="audForm" class="space-y-4">
      <div>
        <label class="block text-sm font-semibold text-gray-700 mb-2">Select audio</label>
        <p class="text-sm text-gray-500 mb-3">You can pick multiple files</p>
        <div class="flex items-center gap-3">
          <input id="audios" name="audios" type="file" accept="audio/*" multiple 
                 class="block w-full text-sm text-gray-500 file:mr-4 file:py-2 file:px-4 file:rounded-lg file:border-0 file:text-sm file:font-medium file:bg-emerald-50 file:text-emerald-700 hover:file:bg-emerald-100 file:cursor-pointer" />
          <button type="submit" class="px-6 py-2 bg-emerald-600 text-white rounded-lg hover:bg-emerald-700 transition-colors font-medium">
            Upload
          </button>
        </div>
      </div>
    </form>

    <div id="audList" class="mt-6" style="display:none;">
      <div class="grid grid-cols-11 gap-2 items-center pb-3 border-b border-gray-200 mb-4 text-sm font-semibold text-gray-700">
        <div>File</div><div>Dur</div><div>Codec</div><div>Ch</div><div>Rate</div><div>Bitrate</div><div>Format</div><div>BR kbps</div><div>SR Hz</div><div>Ch</div><div>Details</div>
      </div>
      <div id="audRows" class="space-y-3"></div>
      <div class="mt-6 pt-6 border-t border-gray-200">
        <button id="audGo" type="button" class="px-6 py-2 bg-emerald-600 text-white rounded-lg hover:bg-emerald-700 transition-colors font-medium">
          Convert Selected
        </button>
      </div>
    </div>
    <div id="audResults" class="mt-6" style="display:none;"></div>
  </div>

<script>
// ----- Videos -----
const rowsDiv = document.getElementById('rows');
const listDiv = document.getElementById('list');
const resultsDiv = document.getElementById('results');
const upForm = document.getElementById('upForm');
const goBtn = document.getElementById('goBtn');
let uploads = [];

upForm.addEventListener('submit', async function(e) {
  e.preventDefault();
  const files = document.getElementById('videos').files;
  if (!files || files.length === 0) { alert('Pick at least one video'); return; }
  const fd = new FormData();
  for (const f of files) fd.append('videos', f, f.name);
  const res = await fetch('/upload', { method: 'POST', body: fd });
  if (!res.ok) { alert('Upload failed: ' + await res.text()); return; }
  const data = await res.json();
  uploads = data.videos || [];
  renderList();
});

function renderList() {
  rowsDiv.innerHTML = '';
  if (uploads.length === 0) { listDiv.style.display='none'; return; }
  listDiv.style.display = 'block';
  for (const v of uploads) {
    const row = document.createElement('div'); 
    row.className = 'grid grid-cols-5 gap-4 items-center py-3 border-b border-gray-100 last:border-b-0';
    const dur = v.duration_seconds || 0; const hms = toHMS(dur);
    const fpsInput = document.createElement('input'); 
    fpsInput.type = 'number'; fpsInput.min = '0.1'; fpsInput.step = '0.1'; fpsInput.value = '1';
    fpsInput.className = 'w-20 px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-blue-500 focus:border-blue-500';
    const estSpan = document.createElement('div'); 
    estSpan.className = 'font-mono text-sm text-gray-600'; 
    estSpan.textContent = Math.ceil(1 * dur);
    fpsInput.oninput = function(){ estSpan.textContent = Math.ceil((Number(fpsInput.value)||0) * dur); };
    
    const fileDiv = document.createElement('div');
    fileDiv.innerHTML = '<span class="font-mono text-sm text-gray-900">'+escapeHTML(v.name)+'</span>';
    
    const durDiv = document.createElement('div');
    durDiv.innerHTML = '<span class="font-mono text-sm text-gray-600">'+hms+'</span>';
    
    const info = document.createElement('div'); 
    info.className = 'text-xs text-gray-500'; 
    info.textContent = 'id=' + v.id;
    
    row.appendChild(fileDiv);
    row.appendChild(durDiv);
    row.appendChild(fpsInput); 
    row.appendChild(estSpan);
    row.appendChild(info);
    row.dataset.id = v.id; row.dataset.duration = dur; rowsDiv.appendChild(row);
  }
}

goBtn?.addEventListener('click', async function(){
  const items = []; const jpegq = Number(document.getElementById('jpegq').value || '2'); const density = Number(document.getElementById('density').value || '150'); const pdfq = Number(document.getElementById('pdfq').value || '92');
  for (const row of rowsDiv.children) { const id = row.dataset.id; const fps = Number(row.querySelector('input[type=number]').value || '1'); items.push({ id: id, fps: fps }); }
  const payload = { items: items, jpeg_quality: jpegq, pdf_density: density, pdf_quality: pdfq };
  resultsDiv.style.display = 'block'; resultsDiv.innerHTML = '<div class="text-gray-500 text-center py-4">Processing…</div>';
  const res = await fetch('/process', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(payload) });
  if (!res.ok) { resultsDiv.innerHTML = '<div class="text-red-600 p-4 bg-red-50 border border-red-200 rounded-lg">'+escapeHTML(await res.text())+'</div>'; return; }
  const data = await res.json();
  const headerRow = '<div class="grid grid-cols-5 gap-4 items-center pb-3 border-b border-gray-200 mb-4 font-semibold text-gray-700"><div>File</div><div>Duration</div><div>FPS</div><div>Frames</div><div>PDF</div></div>';
  const rows = (data.results||[]).map(function(r){ 
    return '<div class="grid grid-cols-5 gap-4 items-center py-3 border-b border-gray-100 last:border-b-0">' + 
           '<div><span class="font-mono text-sm text-gray-900">'+escapeHTML(r.name)+'</span></div>' + 
           '<div><span class="font-mono text-sm text-gray-600">'+toHMS(r.duration_seconds)+'</span></div>' + 
           '<div><span class="font-mono text-sm text-gray-600">'+r.fps+'</span></div>' + 
           '<div><span class="font-mono text-sm text-gray-600">'+r.frames_wrote+' (est '+r.estimated_frames+')</span></div>' + 
           '<div><a href="'+r.pdf_url+'" download class="inline-flex items-center px-3 py-1.5 bg-blue-600 text-white text-sm rounded-lg hover:bg-blue-700 transition-colors">Download PDF</a></div>' + 
           '</div>'; 
  }).join('');
  resultsDiv.innerHTML = headerRow + rows;
});

// ----- Images -----
const imgForm = document.getElementById('imgForm');
const thumbsDiv = document.getElementById('thumbs');
const imgList = document.getElementById('imgList');
const imgResult = document.getElementById('imgResult');
let imgUploads = [];

imgForm.addEventListener('submit', async function(e){
  e.preventDefault();
  const files = document.getElementById('imgs').files;
  if (!files || files.length === 0) { alert('Pick at least one image'); return; }
  const fd = new FormData(); for (const f of files) fd.append('images', f, f.name);
  const res = await fetch('/upload_images', { method: 'POST', body: fd });
  if (!res.ok) { alert('Upload failed: ' + await res.text()); return; }
  const data = await res.json(); imgUploads = data.images || []; renderThumbs();
});

function renderThumbs(){
  thumbsDiv.innerHTML = ''; if (imgUploads.length === 0) { imgList.style.display = 'none'; return; } imgList.style.display = 'block';
  for (let i=0;i<imgUploads.length;i++){
    const it = imgUploads[i];
    const wrap = document.createElement('div'); 
    wrap.className = 'bg-white border border-gray-200 rounded-lg p-4 text-center hover:shadow-md transition-shadow';
    const im = document.createElement('img'); 
    im.src = it.url; 
    im.className = 'w-full h-32 object-contain mx-auto mb-3 rounded';
    wrap.appendChild(im);
    const caption = document.createElement('div'); 
    caption.className = 'text-xs font-mono text-gray-600 mb-2 truncate'; 
    caption.textContent = it.name; 
    wrap.appendChild(caption);
    const lab = document.createElement('label'); 
    lab.className='text-xs font-medium text-gray-700 block mb-1'; 
    lab.textContent = 'Order:'; 
    wrap.appendChild(lab);
    const order = document.createElement('input'); 
    order.type='number'; order.step='1'; order.min='1'; order.value = String(i+1); 
    order.className='orderInput w-full px-2 py-1 border border-gray-300 rounded text-sm focus:ring-2 focus:ring-purple-500 focus:border-purple-500'; 
    wrap.appendChild(order);
    wrap.dataset.id = it.id; thumbsDiv.appendChild(wrap);
  }
}

document.getElementById('imgGo').addEventListener('click', async function(){
  const density = Number(document.getElementById('idensity').value || '150'); const quality = Number(document.getElementById('iquality').value || '92'); const outName = document.getElementById('iname').value || '';
  const items = []; const cards = thumbsDiv.children; for (let i=0;i<cards.length;i++){ const id = cards[i].dataset.id; const ord = Number(cards[i].querySelector('input.orderInput').value || (i+1)); items.push({ id: id, order: ord }); }
  imgResult.style.display='block'; imgResult.innerHTML = '<div class="text-gray-500 text-center py-4">Building PDF…</div>';
  const payload = { items: items, pdf_density: density, pdf_quality: quality, out_name: outName };
  const res = await fetch('/images_pdf', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(payload) });
  if (!res.ok) { imgResult.innerHTML = '<div class="text-red-600 p-4 bg-red-50 border border-red-200 rounded-lg">'+escapeHTML(await res.text())+'</div>'; return; }
  const dat = await res.json(); 
  imgResult.innerHTML = '<div class="p-4 bg-green-50 border border-green-200 rounded-lg"><a href="'+dat.pdf_url+'" download class="inline-flex items-center px-4 py-2 bg-green-600 text-white rounded-lg hover:bg-green-700 transition-colors font-medium">Download Images PDF</a> <span class="ml-3 text-green-700">('+dat.count+' pages)</span></div>';
});

// ----- Audio -----
const audForm = document.getElementById('audForm');
const audList = document.getElementById('audList');
const audRows = document.getElementById('audRows');
const audGo = document.getElementById('audGo');
const audResults = document.getElementById('audResults');
let audUploads = [];

audForm.addEventListener('submit', async function(e){
  e.preventDefault();
  const files = document.getElementById('audios').files;
  if (!files || files.length === 0) { alert('Pick at least one audio'); return; }
  const fd = new FormData(); for (const f of files) fd.append('audios', f, f.name);
  const res = await fetch('/upload_audio', { method: 'POST', body: fd });
  if (!res.ok) { alert('Upload failed: ' + await res.text()); return; }
  const data = await res.json(); audUploads = data.audios || []; renderAud();
});

function renderAud(){
  audRows.innerHTML=''; if (audUploads.length===0){audList.style.display='none'; return;} audList.style.display='block';
  for (let i=0;i<audUploads.length;i++){
    const a = audUploads[i];
    const row = document.createElement('div'); 
    row.className='grid grid-cols-11 gap-2 items-center py-3 border-b border-gray-100 last:border-b-0 text-sm';
    const dur = toHMS(a.duration_seconds||0);
    const br = (a.bitrate_kbps||0) ? (a.bitrate_kbps+' kbps') : '-';
    
    const fileDiv = document.createElement('div');
    fileDiv.innerHTML = '<span class="font-mono text-gray-900 text-xs truncate block">'+escapeHTML(a.name)+'</span>';
    
    row.appendChild(fileDiv);
    row.innerHTML += '<div class="font-mono text-gray-600">'+dur+'</div>'+
      '<div class="font-mono text-gray-600">'+(a.codec||'-')+'</div>'+
      '<div class="font-mono text-gray-600">'+(a.channels||'-')+'</div>'+
      '<div class="font-mono text-gray-600">'+(a.sample_rate||'-')+'</div>'+
      '<div class="font-mono text-gray-600">'+br+'</div>';
    
    const fmt = document.createElement('select');
    fmt.className = 'px-2 py-1 border border-gray-300 rounded text-xs focus:ring-2 focus:ring-emerald-500 focus:border-emerald-500';
    ;['mp3','wav','flac','aac','ogg','opus'].forEach(function(opt){ const o=document.createElement('option'); o.value=opt; o.textContent=opt; if(opt==='mp3') o.selected=true; fmt.appendChild(o); });
    
    const brI = document.createElement('input'); 
    brI.type='number'; brI.min='32'; brI.max='512'; brI.step='16'; brI.value= String(a.bitrate_kbps||192);
    brI.className = 'w-16 px-2 py-1 border border-gray-300 rounded text-xs focus:ring-2 focus:ring-emerald-500 focus:border-emerald-500';
    
    const srI = document.createElement('input'); 
    srI.type='number'; srI.min='8000'; srI.max='192000'; srI.step='1000'; srI.value= String(a.sample_rate||44100);
    srI.className = 'w-16 px-2 py-1 border border-gray-300 rounded text-xs focus:ring-2 focus:ring-emerald-500 focus:border-emerald-500';
    
    const chI = document.createElement('input'); 
    chI.type='number'; chI.min='1'; chI.max='2'; chI.step='1'; chI.value= String(a.channels||2);
    chI.className = 'w-12 px-2 py-1 border border-gray-300 rounded text-xs focus:ring-2 focus:ring-emerald-500 focus:border-emerald-500';
    
    const det = document.createElement('button'); 
    det.type='button'; det.textContent='Details';
    det.className = 'px-2 py-1 bg-gray-100 text-gray-700 rounded text-xs hover:bg-gray-200 transition-colors';
    
    const pre = document.createElement('pre'); 
    pre.className='bg-gray-50 p-3 rounded-lg text-xs overflow-auto max-h-64 mt-2 border border-gray-200 col-span-11'; 
    pre.style.display='none'; 
    pre.textContent = a.probe_json||'';
    det.onclick = function(){ pre.style.display = (pre.style.display==='none'?'block':'none'); };

    row.appendChild(fmt); row.appendChild(brI); row.appendChild(srI); row.appendChild(chI); row.appendChild(det);
    audRows.appendChild(row); audRows.appendChild(pre);

    row.dataset.id = a.id;
  }
}

audGo.addEventListener('click', async function(){
  const items = []; const children = audRows.children;
  for (let i=0;i<children.length;i+=2){
    const row = children[i]; if (!row || !row.classList.contains('grid')) continue;
    const id = row.dataset.id; const selects = row.getElementsByTagName('select'); const inputs = row.getElementsByTagName('input');
    const fmt = selects[0].value; const br = Number(inputs[0].value||'192'); const sr = Number(inputs[1].value||'44100'); const ch = Number(inputs[2].value||'2');
    items.push({ id: id, format: fmt, bitrate_kbps: br, sample_rate: sr, channels: ch });
  }
  audResults.style.display='block'; audResults.innerHTML='<div class="text-gray-500 text-center py-4">Converting…</div>';
  const res = await fetch('/convert_audio', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({ items: items }) });
  if (!res.ok) { audResults.innerHTML = '<div class="text-red-600 p-4 bg-red-50 border border-red-200 rounded-lg">'+escapeHTML(await res.text())+'</div>'; return; }
  const data = await res.json();
  const rows = (data.results||[]).map(function(r){ 
    return '<div class="p-3 bg-gray-50 border border-gray-200 rounded-lg mb-2"><a href="'+r.out_url+'" download class="inline-flex items-center px-3 py-1.5 bg-emerald-600 text-white text-sm rounded-lg hover:bg-emerald-700 transition-colors">'+escapeHTML(r.name)+' → '+escapeHTML(r.format)+'</a></div>'; 
  }).join('');
  audResults.innerHTML = rows || '<div class="text-gray-500 text-center py-4">No results</div>';
});

function toHMS(sec) { sec = Number(sec||0); const h = Math.floor(sec/3600); const m = Math.floor((sec%3600)/60); const s = (sec - h*3600 - m*60).toFixed(3); return pad(h)+":"+pad(m)+":"+s.padStart(6,'0'); }
function pad(n){ return String(n).padStart(2,'0'); }
function escapeHTML(s){ return (s||'').replace(/[&<>"']/g, function(c){ return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]; }); }
</script>
</body>
</html>`
