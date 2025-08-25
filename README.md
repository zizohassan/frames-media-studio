Frames & Media Studio (Go + Gin)

A tiny web app that lets you:

Videos → Frames → PDF
Upload multiple videos, pick FPS per video, extract frames with ffmpeg, and bundle each video’s frames into a PDF with ImageMagick.

Images → Ordered PDF
Upload multiple images, set their order with inputs, and create a single PDF in that order.

Audio → Inspect & Convert
Upload audio, inspect details via ffprobe (codec, channels, sample rate, bitrate, duration), and convert to mp3/wav/flac/aac/ogg/opus with ffmpeg.

Backend is Go (Gin). No database. Files saved under ./work/.

Demo (local)
go run main.go
# open http://localhost:8080

Features

Multi-file uploads for videos/images/audio

Per-video FPS selection & frame count estimation

PDF building with quality/density controls

Image ordering (number inputs)

Audio probe (full raw ffprobe JSON) + per-file conversion settings

Static download endpoints for generated PDFs/audio

Tech stack

Go (Gin)

ffmpeg / ffprobe

ImageMagick (magick convert or legacy convert)

Requirements

Install these first (they must be in PATH):

Go 1.20+

ffmpeg (includes ffprobe)

ImageMagick

Newer builds use magick convert …

Older builds use convert … (the app auto-detects)

(Recommended for PDF writing) Ghostscript

Quick install examples

macOS (Homebrew)

brew install ffmpeg imagemagick ghostscript


Debian/Ubuntu

sudo apt-get update
sudo apt-get install -y ffmpeg imagemagick ghostscript

Setup
go mod init framespdf
go get github.com/gin-gonic/gin
go run main.go


Open: http://localhost:8080

The app creates and uses:

work/
  uploads/   # original uploads
  frames/    # extracted frames from videos
  pdfs/      # generated PDFs
  audio/     # converted audio files
