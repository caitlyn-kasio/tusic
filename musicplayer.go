package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"kasio.xyz/musicplayer/player"

)



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
)

// classifyPayload sniffs the JSON shape so we can route events without
// knowing the property name. Arrays are the playlist, objects are metadata,
// bare numbers are playlist-pos.
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
	case trimmed[0] == '-', trimmed[0] >= '0' && trimmed[0] <= '9':
		return payloadPosition
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
		title := jsonString(e, "title")
		path := jsonString(e, "filename")
		if path == "" {
			path = jsonString(e, "file")
		}
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

// ---------------------------------------------------------------------------

func main() {

	// Initialize variables
	playerPaused := false

	// Queue state. Guarded by queueMu because the player event loop
	// lives on its own goroutine.
	var queueMu sync.Mutex
	var queueEntries []queueEntry
	queuePos := -1 // index of the playing entry, -1 when unknown/empty
	focusQueue := false

	// Initialize music player
	player, err := player.Start("/tmp/tusic.sock")
	if err != nil {
		log.Fatal(err)
	}
	defer player.Close()

	// Initialize Observe ("playlist" is the one we added)
	player.Observe(1, "metadata")
	player.Observe(2, "playlist-pos")
	player.Observe(3, "playlist")

	// Initialize app, create root
	app := tview.NewApplication()
	flexRoot := tview.NewFlex().
		SetDirection(tview.FlexColumn)

	// Browser Handling

	rootDir := "/home/caitlyn/Music/" // TODO: Add args to this!
	rootNode := tview.NewTreeNode(rootDir)

	// Final Construction of browserTree
	browserTree := tview.NewTreeView().
		SetRoot(rootNode).
		SetCurrentNode(rootNode)

	checkIsDir := func(node *tview.TreeNode) bool {
		ref := node.GetReference()
		if ref == nil {
			return false
		}
		_fileRef, ok := ref.(string)
		if !ok {
			return false
		}
		_file, err := os.Open(_fileRef)
		if err != nil {
			return false
		}
		defer _file.Close()
		_fileStat, err := _file.Stat()
		if err != nil {
			return false
		}
		return _fileStat.IsDir()
	}

	fetchMetadata := func(jsonIn []byte, obj *tview.List) {
		obj.Clear()
		var jsonMap map[string]interface{}
		if err := json.Unmarshal(jsonIn, &jsonMap); err != nil {
			return
		}

		resolveMetadata := func(mapin map[string]interface{}, stringin string) string {
			v, err := mapin[stringin]
			if !err {
			} else if v == nil {
			} else if s, err := v.(string); err {
				return s
			}
			return "None"
		}

		_title := resolveMetadata(jsonMap, "title")
		obj.AddItem(_title, "Title", 0, nil).SetSelectedBackgroundColor(tcell.ColorDefault).SetSelectedTextColor(tcell.ColorWhite)
		_artist := resolveMetadata(jsonMap, "artist")
		obj.AddItem(_artist, "Artist", 0, nil)
		_album := resolveMetadata(jsonMap, "album")
		obj.AddItem(_album, "Album", 0, nil)
		_track := resolveMetadata(jsonMap, "track")
		obj.AddItem(_track, "Track #", 0, nil)
	}

	// Thank you to the example tree view for tview!
	browserAdd := func(target *tview.TreeNode, path string) {
		files, err := ioutil.ReadDir(path)
		if err != nil {
			panic(err)
		}

		for _, file := range files {
			node := tview.NewTreeNode(file.Name()).
				SetReference(filepath.Join(path, file.Name())).
				SetSelectable(true)

			if file.IsDir() {
				node.SetColor(tcell.ColorGreen)
			}
			target.AddChild(node)
		}
	}

	browserAdd(rootNode, rootDir)

	browserExpand := func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return // Selecting the root node does nothing.
		}

		children := node.GetChildren()
		if len(children) == 0 {
			// Load and show files in this directory.
			path := reference.(string)
			browserAdd(node, path)
		} else {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		}
	}

	getCur := func() string {
		return browserTree.GetCurrentNode().GetReference().(string)
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
		player.Call(5*time.Second, "set_property", "playlist-pos", index)
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
			queueList.AddItem("(empty — select a file and press 'o')", "", 0, nil)
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

	// Highlight whichever pane owns the keyboard.
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
			// Don't wipe the cover on the transient window where the
			// playlist has updated but playlist-pos hasn't.
			queueMu.Lock()
			empty := len(queueEntries) == 0
			queueMu.Unlock()
			if !empty {
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

		// Keep only the newest request, so holding 'p' doesn't queue up
		// a hundred decodes.
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

	controlDisplay.SetText("WSD - Navigate Menu | TAB - Switch browser/queue | p - Play Selected | o - Queue | x - Remove | u - Pause")
	controlDisplay.SetBorder(true)
	controlDisplay.SetTitle(" Keybindings ")

	metadataList.SetTitle(" Metadata ")
	metadataList.SetBorder(true)

	queueList.SetTitle(" Queue ")
	queueList.SetBorder(true)
	queueList.ShowSecondaryText(false)
	queueList.SetHighlightFullLine(true)
	queueList.SetSelectedFocusOnly(false) // cursor stays visible when browser has focus
	queueList.SetWrapAround(false)

	// Initial paint of the empty queue.
	renderQueue()

	// Observe Msg Handling
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
			}
		}
		app.Draw()
	}()

	// Key Input Handling
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {

		// TAB switches between the browser and the queue.
		if event.Key() == tcell.KeyTab || event.Key() == tcell.KeyBacktab {
			focusQueue = !focusQueue
			if focusQueue {
				app.SetFocus(queueList)
				// put the cursor on whatever is playing
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
				if queueList.GetItemCount() > 0 {
					if i := queueList.GetCurrentItem(); i > 0 {
						queueList.SetCurrentItem(i - 1)
					}
				}
			} else {
				browserTree.Move(-1)
			}
		case 's':
			if focusQueue {
				if n := queueList.GetItemCount(); n > 0 {
					if i := queueList.GetCurrentItem(); i < n-1 {
						queueList.SetCurrentItem(i + 1)
					}
				}
			} else {
				browserTree.Move(1)
			}

		// Expand folder (browser only)
		case 'd':
			if !focusQueue {
				_cur := browserTree.GetCurrentNode()
				if checkIsDir(_cur) {
					browserExpand(_cur)
				}
			}

		// Play
		case 'p':
			if focusQueue {
				idx := queueList.GetCurrentItem()
				if n := queueCount(); n > 0 && idx >= 0 && idx < n {
					jumpTo(idx)
				}
				return nil
			}
			// Play from the browser
			if checkIsDir(browserTree.GetCurrentNode()) {
				// Unimplemented, will eventually have this shortcut to the first
				// item in a folder
			} else {
				player.Call(5*time.Second, "loadfile", getCur())
			}

		// Play/Pause
		case 'u':
			if playerPaused {
				player.SetPause(false)
				playerPaused = false
			} else {
				player.SetPause(true)
				playerPaused = true
			}

		// Seek Backwards
		case 'n':
			player.Seek(-5)

		// Seek Forwards
		case 'm':
			player.Seek(5)

		// Add to queue
		case 'o':
			if focusQueue {
				return nil
			}
			_cur := browserTree.GetCurrentNode()
			if _cur.GetReference() == nil || checkIsDir(_cur) {
				return nil // nothing sensible to append
			}
			_curPath := _cur.GetReference().(string)
			player.Load(_curPath, "append-play")

		// Remove the selected entry from the queue
		case 'x':
			if !focusQueue {
				return nil
			}
			idx := queueList.GetCurrentItem()
			if n := queueCount(); n > 0 && idx >= 0 && idx < n {
				// The playlist-remove event will refresh the display.
				player.Call(5*time.Second, "playlist-remove", idx)
			}
			return nil
		}
		return event
	})

	// Run the app
	if err := app.SetRoot(flexRoot, true).Run(); err != nil {
		panic(err)
	}
}
