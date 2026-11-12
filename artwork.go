package main

import (
	"bytes"
	"image"
	_ "image/jpeg" // registers the decoder for APIC/covr payloads
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dhowden/tag"
	xdraw "golang.org/x/image/draw"
)

const (
	// Cover art is decoded at full size then shrunk to this bounding box.
	// tview re-rasterises to the widget size on every layout change, and that
	// pass is O(source pixels), so a 3000x3000 scan would make the UI stutter.
	artMaxPx = 512

	// How many decoded covers to keep. Overshoot just resets the whole map.
	artCacheMax = 48
)

var artCache = struct {
	sync.Mutex
	entries map[string]image.Image
}{entries: make(map[string]image.Image)}

func artFromCache(key string) (image.Image, bool) {
	artCache.Lock()
	defer artCache.Unlock()
	img, ok := artCache.entries[key]
	return img, ok
}

func artToCache(key string, img image.Image) {
	artCache.Lock()
	defer artCache.Unlock()
	if len(artCache.entries) >= artCacheMax {
		artCache.entries = make(map[string]image.Image, artCacheMax)
	}
	artCache.entries[key] = img
}

// loadArtwork returns the cover for the track at path, or nil. Results
// (including "there is none") are cached, so repeated calls per track are free.
func loadArtwork(path string) image.Image {
	if path == "" {
		return nil
	}
	if img, ok := artFromCache(path); ok {
		return img
	}

	img := embeddedCover(path)
	if img == nil {
		img = folderCover(path)
	}

	artToCache(path, img) // negative results cached too
	return img
}

// embeddedCover pulls the attached picture out of the file's metadata.
func embeddedCover(path string) image.Image {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	m, err := tag.ReadFrom(f) // handles MP3/ID3, FLAC, OGG, MP4/M4A
	if err != nil {
		return nil
	}

	pic := m.Picture()
	if pic == nil || len(pic.Data) == 0 {
		return nil
	}
	return decodeArt(pic.Data)
}

var coverBaseNames = []string{
	"cover", "folder", "front", "album", "albumart", "albumartsmall", "artwork",
}

// folderCover looks for the usual sidecar images next to the track.
func folderCover(trackPath string) image.Image {
	dir := filepath.Dir(trackPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var fallback string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch strings.ToLower(filepath.Ext(name)) {
		case ".jpg", ".jpeg", ".png":
		default:
			continue
		}

		full := filepath.Join(dir, name)
		if fallback == "" {
			fallback = full // first image seen, used if no name matches
		}

		base := strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
		for _, want := range coverBaseNames {
			if base == want {
				if img := decodeArtFile(full); img != nil {
					return img
				}
			}
		}
	}

	if fallback != "" {
		return decodeArtFile(fallback)
	}
	return nil
}

func decodeArtFile(path string) image.Image {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return decodeArt(data)
}

func decodeArt(data []byte) image.Image {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return fitArt(img, artMaxPx)
}

// fitArt shrinks src to fit inside a maxPx square, preserving aspect ratio.
// Images already that small are returned untouched.
func fitArt(src image.Image, maxPx int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	if w <= maxPx && h <= maxPx {
		return src
	}

	ratio := float64(maxPx) / float64(w)
	if r := float64(maxPx) / float64(h); r < ratio {
		ratio = r
	}
	nw := int(float64(w)*ratio + 0.5)
	nh := int(float64(h)*ratio + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Src, nil)
	return dst
}
