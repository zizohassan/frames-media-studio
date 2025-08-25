# Frames & Media Studio

A lightweight web application built with Go and Gin that provides comprehensive media processing capabilities.

<img width="1135" height="894" alt="Screenshot 2025-08-25 at 4 11 36 PM" src="https://github.com/user-attachments/assets/f3ca97d2-7a9b-4e86-a6d4-a8e14e4193bc" />

<img width="1118" height="692" alt="Screenshot 2025-08-25 at 4 12 23 PM" src="https://github.com/user-attachments/assets/0449bf66-2149-478c-a96d-d1a006b94af9" />

<img width="1123" height="698" alt="Screenshot 2025-08-25 at 4 13 10 PM" src="https://github.com/user-attachments/assets/1ff04698-989e-4b22-9fac-142163286db5" />

<img width="1122" height="553" alt="Screenshot 2025-08-25 at 4 13 40 PM" src="https://github.com/user-attachments/assets/483fc05a-bd85-4603-a986-070c92c6425b" />

## Features

### 🎥 Videos → Frames → PDF
- Upload multiple video files
- Configure FPS (frames per second) for each video individually
- Extract frames using ffmpeg
- Bundle extracted frames into PDF documents using ImageMagick
- Frame count estimation before processing

### 🖼️ Images → Ordered PDF
- Upload multiple image files
- Set custom ordering using number inputs
- Generate a single PDF with images in the specified order

### 🎵 Audio → Inspect & Convert
- Upload audio files for analysis
- Detailed inspection via ffprobe (codec, channels, sample rate, bitrate, duration)
- Convert audio to multiple formats: MP3, WAV, FLAC, AAC, OGG, Opus
- Per-file conversion settings

## Key Capabilities

- **Multi-file uploads** for videos, images, and audio
- **Per-video FPS selection** with frame count estimation
- **PDF generation** with quality and density controls
- **Image ordering** through intuitive number inputs
- **Audio analysis** with full raw ffprobe JSON output
- **Static download endpoints** for generated PDFs and converted audio
- **No database required** - all processing is file-based

## Tech Stack

- **Backend**: Go with Gin framework
- **Media Processing**: ffmpeg & ffprobe
- **Image Processing**: ImageMagick
- **File Storage**: Local filesystem (`./work/` directory)

## Prerequisites

Ensure the following tools are installed and available in your system PATH:

- **Go 1.20+**
- **ffmpeg** (includes ffprobe)
- **ImageMagick**
  - Newer installations: `magick convert ...`
  - Legacy installations: `convert ...` (auto-detected by the app)
- **Ghostscript** (recommended for optimal PDF generation)

## Installation

### macOS (Homebrew)
```bash
brew install ffmpeg imagemagick ghostscript
```

### Debian/Ubuntu
```bash
sudo apt-get update
sudo apt-get install -y ffmpeg imagemagick ghostscript
```

### Windows
- Download and install Go from [golang.org](https://golang.org)
- Install ffmpeg from [ffmpeg.org](https://ffmpeg.org/download.html)
- Install ImageMagick from [imagemagick.org](https://imagemagick.org/script/download.php)
- Install Ghostscript from [ghostscript.com](https://www.ghostscript.com/download/gsdnld.html)

## Setup & Running

1. **Initialize the project**:
   ```bash
   go mod init framespdf
   go get github.com/gin-gonic/gin
   ```

2. **Start the application**:
   ```bash
   go run main.go
   ```

3. **Access the web interface**:
   Open your browser and navigate to: http://localhost:8080

## File Structure

The application automatically creates and manages the following directory structure:

```
work/
├── uploads/    # Original uploaded files
├── frames/     # Extracted video frames
├── pdfs/       # Generated PDF documents
└── audio/      # Converted audio files
```

## Demo

To see the application in action:

```bash
go run main.go
# Then open http://localhost:8080 in your browser
```

## Architecture

- **Stateless Design**: No database dependencies
- **File-based Processing**: All operations work with local files
- **Concurrent Processing**: Efficient handling of multiple file operations
- **Auto-detection**: Automatically detects ImageMagick version (legacy vs. modern)

## API Endpoints

The application provides RESTful endpoints for:
- File uploads (videos, images, audio)
- Media processing operations
- Static file downloads
- Processing status and results

---

**License**: [MIT]
**Author**: [Abdel Aziz hassan]
**Version**: 1.0.0






