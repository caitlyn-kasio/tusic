// Command musicplayer is a terminal front end for mpv.
//
// The tree on the left browses the music library; the right-hand side shows
// the current track's metadata, the playback queue and the cover art. All
// playback is delegated to mpv over its JSON IPC socket by the player
// package. Selecting a directory anywhere (play or queue) expands it into its
// playable files, in track order.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"kasio.xyz/musicplayer/player"
)

// ipcTimeout bounds every synchronous IPC round trip to mpv. Generous enough
// for a cold socket, short enough that a wedged player can't hang the UI.
const ipcTimeout = 5 * time.Second

// ---------------------------------------------------------------------------
// Queue helpers
// ---------------------------------------------------------------------------

// queueEntry is one item of the player's playlist, as far as we care about it.
type queueEntry struct {
	Path   string
	Title  string
	Artist string
}

// payloadKind tells us which property an observed JSON blob belongs to.
type payloadKind int

const (
	payloadUnknown payloadKind = iota
	payloadPlaylist
	payloadPosition
	payloadMetadata
	payloadBool
)

// classifyPayload sniffs the JSON shape so we can route events without
// knowing the property name. Arrays are the playlist, objects are metadata,
// bare numbers are playlist-pos, true/false is a flag such as pause.
func classifyPayload(raw []byte) payloadKind {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return payloadUnknown
	}
	switch {
	case trimmed[0] == '[':
		return payloadPlaylist
	case trimmed[0] == '{':
		return payloadMetadata
	case trimmed[0] == '-', isASCIIDigit(trimmed[0]):
		return payloadPosition
	case trimmed[0] == 't', trimmed[0] == 'f':
		return payloadBool
	}
	return payloadUnknown
}

// rawJSON normalises whatever the player package hands us into bytes.
// (Data may be json.RawMessage, []byte, a string, or a Stringer.)
func rawJSON(v interface{}) []byte {
	switch d := v.(type) {
	case json.RawMessage:
		return d
	case []byte:
		return d
	case string:
		return []byte(d)
	case fmt.Stringer:
		return []byte(d.String())
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// parsePlaylist turns the "playlist" property into a slice of queueEntry.
func parsePlaylist(raw []byte) []queueEntry {
	var entries []map[string]interface{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		log.Printf("queue: cannot parse playlist: %v", err)
		return nil
	}

	out := make([]queueEntry, 0, len(entries))
	for _, e := range entries {
		path := jsonString(e, "filename")
		if path == "" {
			path = jsonString(e, "file")
		}
		title := jsonString(e, "title")
		if title == "" {
			// mpv lazily fills in "title"; fall back to the file name.
			title = filepath.Base(path)
		}
		out = append(out, queueEntry{
			Path:   path,
			Title:  title,
			Artist: jsonString(e, "artist"),
		})
	}
	return out
}

func jsonString(m map[string]interface{}, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// metadataValue renders one metadata field for display. mpv sends strings for
// text tags but numbers for things like track, so both are handled.
func metadataValue(m map[string]interface{}, key string) string {
	switch v := m[key].(type) {
	case string:
		if v != "" {
			return v
		}
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return "None"
}

// ---------------------------------------------------------------------------
// Album (directory) helpers
// ---------------------------------------------------------------------------

// audioExts is the set of extensions we hand to the player. mpv itself is more
// liberal, but an allow-list keeps stray cover.jpg / rip.log files out of the
// queue when an album folder mixes them in with the tracks.
var audioExts = map[string]bool{
	".aac": true, ".aif": true, ".aiff": true, ".alac": true,
	".ape": true, ".dff": true, ".dsf": true, ".flac": true,
	".m4a": true, ".mka": true, ".mp3": true, ".mp4": true,
	".mpc": true, ".oga": true, ".ogg": true, ".opus": true,
	".wav": true, ".wma": true, ".wv": true,
}

func isAudioFile(path string) bool {
	return audioExts[strings.ToLower(filepath.Ext(path))]
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// collectAlbum lists every playable file under dir, sorted into track order.
// It walks recursively, so "Artist/Album/CD2/01.flac" still follows
// "Artist/Album/CD1/10.flac", and it sorts by natural comparison of the full
// path so numbers compare by value rather than digit by digit.
func collectAlbum(dir string) []string {
	var files []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subdirectory: skip it instead of aborting the album.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") || !isAudioFile(path) {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		log.Printf("album: walking %s: %v", dir, err)
	}

	sort.SliceStable(files, func(i, j int) bool {
		return naturalLess(files[i], files[j])
	})
	return files
}

// naturalLess orders strings the way a person reads a track list: runs of
// digits compare by numeric value, everything else byte by byte.
func naturalLess(a, b string) bool {
	for len(a) > 0 && len(b) > 0 {
		if isASCIIDigit(a[0]) && isASCIIDigit(b[0]) {
			ai, bi := 0, 0
			for ai < len(a) && isASCIIDigit(a[ai]) {
				ai++
			}
			for bi < len(b) && isASCIIDigit(b[bi]) {
				bi++
			}
			na := strings.TrimLeft(a[:ai], "0")
			nb := strings.TrimLeft(b[:bi], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb) // more digits == bigger number
			}
			if na != nb {
				return na < nb
			}
			// Same value ("07" vs "7"): keep comparing what follows.
			a, b = a[ai:], b[bi:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b) // shorter is a prefix, therefore first
}

// ---------------------------------------------------------------------------

func main() {
	// Pause state mirror. Written by the event goroutine, read by the input
	// handler, hence the mutex.
	var (
		pauseMu sync.Mutex
		paused  bool
	)
	setPaused := func(v bool) {
		pauseMu.Lock()
		paused = v
		pauseMu.Unlock()
	}
	isPaused := func() bool {
		pauseMu.Lock()
		defer pauseMu.Unlock()
		return paused
	}

	// Queue state. Guarded by queueMu because the player event loop lives on
	// its own goroutine.
	var (
		queueMu      sync.Mutex
		queueEntries []queueEntry
		queuePos     = -1 // index of the playing entry, -1 when unknown/empty
		batchMu      sync.Mutex
		focusQueue   bool
	)

	// Initialize music player
	player, err := player.Start("/tmp/tusic.sock")
	if err != nil {
		log.Fatal(err)
	}
	defer player.Close()

	// Subscribe to the properties we consume. Request ids are int64 in the
	// player package (they map onto mpv's request_id), so the conversion is
	// explicit.
	for i, name := range []string{"metadata", "playlist-pos", "playlist", "pause"} {
		player.Observe(int64(i+1), name)
	}


	// Initialize app, create root
	app := tview.NewApplication()
	flexRoot := tview.NewFlex().
		SetDirection(tview.FlexColumn)

	// -----------------------------------------------------------------------
	// Browser
	// -----------------------------------------------------------------------

	rootDir := "/home/caitlyn/Music/" // TODO: take this from the command line
	rootNode := tview.NewTreeNode(rootDir)

	browserTree := tview.NewTreeView().
		SetRoot(rootNode).
		SetCurrentNode(rootNode)

	// checkIsDir reports whether the node's reference points at a directory.
	checkIsDir := func(node *tview.TreeNode) bool {
		ref, ok := node.GetReference().(string)
		if !ok {
			return false
		}
		info, err := os.Stat(ref)
		if err != nil {
			return false
		}
		return info.IsDir()
	}

	// browserAdd fills target with the entries of path. Sorted by name, which
	// is what ReadDir guarantees.
	// Thanks to the example tree view for tview!
	browserAdd := func(target *tview.TreeNode, path string) {
		entries, err := os.ReadDir(path)
		if err != nil {
			log.Printf("browser: reading %s: %v", path, err)
			return
		}
		for _, entry := range entries {
			node := tview.NewTreeNode(entry.Name()).
				SetReference(filepath.Join(path, entry.Name())).
				SetSelectable(true)
			if entry.IsDir() {
				node.SetColor(tcell.ColorGreen)
			}
			target.AddChild(node)
		}
	}

	browserAdd(rootNode, rootDir)

	browserExpand := func(node *tview.TreeNode) {
		ref, ok := node.GetReference().(string)
		if !ok {
			return // the root node carries no reference; nothing to do
		}
		if len(node.GetChildren()) == 0 {
			browserAdd(node, ref) // load and show this directory
			return
		}
		node.SetExpanded(!node.IsExpanded()) // collapse if visible, expand if not
	}

	// currentPath is the reference of the selected node, "" when it has none
	// (the root) or the reference isn't a path.
	currentPath := func() string {
		node := browserTree.GetCurrentNode()
		if node == nil {
			return ""
		}
		path, _ := node.GetReference().(string)
		return path
	}

	fetchMetadata := func(raw []byte, obj *tview.List) {
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			return
		}
		obj.Clear()
		for _, field := range []struct{ key, label string }{
			{"title", "Title"},
			{"artist", "Artist"},
			{"album", "Album"},
			{"track", "Track #"},
		} {
			obj.AddItem(metadataValue(m, field.key), field.label, 0, nil)
		}
	}

	// Initialize primitives before adding
	imageDisplay := tview.NewImage()
	controlDisplay := tview.NewTextView()
	metadataList := tview.NewList()
	queueList := tview.NewList()

	// -----------------------------------------------------------------------
	// Queue: commands, rendering, navigation
	// -----------------------------------------------------------------------

	queueCount := func() int {
		queueMu.Lock()
		defer queueMu.Unlock()
		return len(queueEntries)
	}

	// jumpTo asks the player to switch to the playlist entry at index.
	jumpTo := func(index int) {
		player.Call(ipcTimeout, "set_property", "playlist-pos", index)
	}

	// enqueueBatch feeds files to the player in order, one IPC call each.
	// Blocking by design: callers run it on their own goroutine so the UI
	// never waits on a 30-track album.
	enqueueBatch := func(paths []string, replace bool) {
		if len(paths) == 0 {
			return
		}

		// One batch at a time, so hammering 'o' can't interleave two albums.
		batchMu.Lock()
		defer batchMu.Unlock()

		if replace {
			// Exactly what single-file play does: loadfile with no flags is
			// mpv's "replace", which restarts the playlist at track one.
			player.Call(ipcTimeout, "loadfile", paths[0])
			paths = paths[1:]
		}

		for _, p := range paths {
			// "append" grows the playlist without disturbing playback, so the
			// files land in precisely the order we send them.
			player.Load(p, "append")
		}
	}

	// playAlbum resolves dir to its tracks and hands them to the player.
	// Runs off the UI goroutine; the playlist events do the repainting.
	playAlbum := func(dir string, replace bool) {
		files := collectAlbum(dir)
		if len(files) == 0 {
			log.Printf("album: no playable files under %s", dir)
			return
		}
		enqueueBatch(files, replace)
	}

	// renderQueue redraws queueList from the cached playlist state.
	// Must be called from the main (event) goroutine.
	renderQueue := func() {
		queueMu.Lock()
		entries := make([]queueEntry, len(queueEntries))
		copy(entries, queueEntries)
		pos := queuePos
		queueMu.Unlock()

		sel := queueList.GetCurrentItem()
		queueList.Clear()

		if len(entries) == 0 {
			queueList.AddItem("(empty — select a file or album and press 'o')", "", 0, nil)
			queueList.SetTitle(" Queue ")
			return
		}

		for i, e := range entries {
			label := e.Title
			if label == "" {
				label = filepath.Base(e.Path)
			}
			if e.Artist != "" && e.Title != "" {
				label = e.Artist + " — " + label
			}

			marker := "  "
			if i == pos {
				marker = "▶ " // currently playing
			}

			idx := i
			queueList.AddItem(fmt.Sprintf("%s%2d. %s", marker, i+1, label), "", 0, func() {
				jumpTo(idx)
			})
		}

		// Keep the cursor where the user left it.
		if sel < 0 || sel >= len(entries) {
			sel = 0
		}
		queueList.SetCurrentItem(sel)

		if pos >= 0 && pos < len(entries) {
			queueList.SetTitle(fmt.Sprintf(" Queue (%d/%d) ", pos+1, len(entries)))
		} else {
			queueList.SetTitle(fmt.Sprintf(" Queue (%d) ", len(entries)))
		}
	}

	// updateBorders highlights whichever pane owns the keyboard.
	updateBorders := func() {
		if focusQueue {
			browserTree.SetBorderColor(tcell.ColorGray)
			queueList.SetBorderColor(tcell.ColorYellow)
		} else {
			browserTree.SetBorderColor(tcell.ColorWhite)
			queueList.SetBorderColor(tcell.ColorWhite)
		}
	}

	// -----------------------------------------------------------------------
	// Album art: the event handler says *what* is playing, a worker decodes
	// it off the UI goroutine.
	// -----------------------------------------------------------------------

	var (
		artMu   sync.Mutex
		wantArt string // track whose cover we currently want
	)
	artRequests := make(chan string, 1)

	currentTrackPath := func() string {
		queueMu.Lock()
		defer queueMu.Unlock()
		if queuePos < 0 || queuePos >= len(queueEntries) {
			return ""
		}
		return queueEntries[queuePos].Path
	}

	requestArt := func(path string) {
		if path == "" {
			// Don't wipe the cover in the transient window where the playlist
			// has updated but playlist-pos hasn't.
			if queueCount() != 0 {
				return
			}
		}

		artMu.Lock()
		if wantArt == path {
			artMu.Unlock()
			return
		}
		wantArt = path
		artMu.Unlock()

		// Keep only the newest request, so holding 'p' doesn't queue up a
		// hundred decodes.
		select {
		case artRequests <- path:
		default:
			select {
			case <-artRequests:
			default:
			}
			artRequests <- path
		}
	}

	go func() {
		for path := range artRequests {
			img := loadArtwork(path) // nil when there's nothing to show

			artMu.Lock()
			stale := wantArt != path
			artMu.Unlock()
			if stale {
				continue
			}

			app.QueueUpdateDraw(func() {
				imageDisplay.SetImage(img)
			})
		}
	}()

	// -----------------------------------------------------------------------
	// Layout
	// -----------------------------------------------------------------------

	mqFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(metadataList, 0, 1, false).
		AddItem(queueList, 0, 2, false)

	interiorFlexBox := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(mqFlex, 0, 1, false).
		AddItem(imageDisplay, 0, 3, false)

	// We set up nesting in reverse, which is confusing
	rightFlexBox := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(interiorFlexBox, 0, 5, false).
		AddItem(controlDisplay, 0, 1, false)

	// Main layout construction
	flexRoot.AddItem(browserTree, 0, 1, false).
		AddItem(rightFlexBox, 0, 3, false)

	browserTree.SetTitle(" Browser ")
	browserTree.SetBorder(true)
	browserTree.SetTopLevel(1)

	imageDisplay.SetTitle(" Image ")
	imageDisplay.SetBorder(true)

	controlDisplay.SetText("WSD - Navigate Menu | TAB - Switch browser/queue | p - Play (folder = album) | o - Queue (folder = album) | x - Remove | u - Pause | n/m - Seek")
	controlDisplay.SetBorder(true)
	controlDisplay.SetTitle(" Keybindings ")

	metadataList.SetTitle(" Metadata ")
	metadataList.SetBorder(true)
	metadataList.SetSelectedFocusOnly(false)

	queueList.SetTitle(" Queue ")
	queueList.SetBorder(true)
	queueList.ShowSecondaryText(false)
	queueList.SetHighlightFullLine(true)
	queueList.SetSelectedFocusOnly(false) // cursor stays visible when browser has focus
	queueList.SetWrapAround(false)

	// Initial paint of the empty queue.
	renderQueue()

	// -----------------------------------------------------------------------
	// Player events
	// -----------------------------------------------------------------------

	go func() {
		for msg := range player.Events {
			if msg.Event != "property-change" {
				continue
			}

			raw := rawJSON(msg.Data)

			switch classifyPayload(raw) {
			case payloadPlaylist:
				entries := parsePlaylist(raw)
				queueMu.Lock()
				queueEntries = entries
				queueMu.Unlock()
				app.QueueUpdateDraw(renderQueue)
				requestArt(currentTrackPath())

			case payloadPosition:
				var pos int
				if err := json.Unmarshal(raw, &pos); err != nil {
					continue
				}
				queueMu.Lock()
				queuePos = pos
				queueMu.Unlock()
				app.QueueUpdateDraw(renderQueue)
				requestArt(currentTrackPath())

			case payloadMetadata:
				app.QueueUpdateDraw(func() {
					fetchMetadata(raw, metadataList)
				})

			case payloadBool:
				// Currently only "pause", so track the mirror and nothing else.
				var v bool
				if err := json.Unmarshal(raw, &v); err != nil {
					continue
				}
				setPaused(v)
			}
		}
		app.Draw()
	}()

	// -----------------------------------------------------------------------
	// Key input
	// -----------------------------------------------------------------------

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		// TAB switches between the browser and the queue.
		if event.Key() == tcell.KeyTab || event.Key() == tcell.KeyBacktab {
			focusQueue = !focusQueue
			if focusQueue {
				app.SetFocus(queueList)
				// Put the cursor on whatever is playing.
				queueMu.Lock()
				pos, n := queuePos, len(queueEntries)
				queueMu.Unlock()
				if pos >= 0 && pos < n {
					queueList.SetCurrentItem(pos)
				}
			} else {
				app.SetFocus(browserTree)
			}
			updateBorders()
			return nil // consume, so List doesn't also handle it
		}

		switch event.Rune() {
		// Close program
		case 'q':
			app.Stop()

		// Navigation (whichever pane has focus)
		case 'w':
			if focusQueue {
				if queueCount() > 0 {
					if i := queueList.GetCurrentItem(); i > 0 {
						queueList.SetCurrentItem(i - 1)
					}
				}
			} else {
				browserTree.Move(-1)
			}
		case 's':
			if focusQueue {
				if n := queueList.GetItemCount(); queueCount() > 0 && n > 0 {
					if i := queueList.GetCurrentItem(); i < n-1 {
						queueList.SetCurrentItem(i + 1)
					}
				}
			} else {
				browserTree.Move(1)
			}

		// Expand or collapse folder (browser only)
		case 'd':
			if !focusQueue {
				node := browserTree.GetCurrentNode()
				if node != nil && checkIsDir(node) {
					browserExpand(node)
				}
			}

		// Play. In the queue: jump to that entry. In the browser: play the
		// file, or the whole album when the selection is a folder.
		case 'p':
			if focusQueue {
				idx := queueList.GetCurrentItem()
				if n := queueCount(); n > 0 && idx >= 0 && idx < n {
					jumpTo(idx)
				}
				return nil
			}

			node := browserTree.GetCurrentNode()
			path := currentPath()
			if path == "" {
				return nil // reference-less node (the root): nothing to play
			}
			if checkIsDir(node) {
				go playAlbum(path, true)
				return nil
			}
			player.Call(ipcTimeout, "loadfile", path)

		// Play/Pause
		case 'u':
			want := !isPaused()
			setPaused(want) // optimistic; the pause event confirms
			player.SetPause(want)

		// Seek backwards
		case 'n':
			player.Seek(-5)

		// Seek forwards
		case 'm':
			player.Seek(5)

		// Add to queue: the file, or the whole album when the selection is a
		// folder.
		case 'o':
			if focusQueue {
				return nil
			}

			node := browserTree.GetCurrentNode()
			path := currentPath()
			if path == "" {
				return nil // nothing sensible to append
			}
			if checkIsDir(node) {
				go playAlbum(path, false)
				return nil
			}
			player.Load(path, "append-play")

		// Remove the selected entry from the queue
		case 'x':
			if !focusQueue {
				return nil
			}
			idx := queueList.GetCurrentItem()
			if n := queueCount(); n > 0 && idx >= 0 && idx < n {
				// The playlist event will refresh the display.
				player.Call(ipcTimeout, "playlist-remove", idx)
			}
			return nil

		default:
			return event // not ours; let tview have it
		}
		return nil
	})

	// Run the app
	if err := app.SetRoot(flexRoot, true).Run(); err != nil {
		panic(err)
	}
}
