package player

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// This is all vibecoded. Incredible.

type message struct {
	Error     string          `json:"error,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	RequestID int64           `json:"request_id,omitempty"`
	Event     string          `json:"event,omitempty"`
	Name      string          `json:"name,omitempty"`
}

type Player struct {
	cmd     *exec.Cmd
	conn    net.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	waiters map[int64]chan message

	Events chan message // property changes, end-file, etc.
	done   chan struct{}
}

func Start(socketPath string) (*Player, error) {
	os.Remove(socketPath) // clear a stale socket from a previous crash

	cmd := exec.Command("mpv",
		"--idle=yes",
		"--no-video",
		"--no-terminal",
		"--really-quiet",
		"--gapless-audio=yes",
		"--input-ipc-server="+socketPath,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mpv: %w", err)
	}

	// mpv needs a beat to create the socket, so poll for it.
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if conn, err = net.Dial("unix", socketPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("connect: %w", err)
	}

	p := &Player{
		cmd:     cmd,
		conn:    conn,
		waiters: map[int64]chan message{},
		Events:  make(chan message, 64),
		done:    make(chan struct{}),
	}
	go p.readLoop()
	return p, nil
}

func (p *Player) readLoop() {
	defer close(p.Events)
	defer close(p.done)

	sc := bufio.NewScanner(p.conn)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for sc.Scan() {
		var msg message
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue // mpv can emit invalid UTF-8; ignore that line
		}
		// Events and replies arrive interleaved on the same stream.
		// Events carry "event"; replies carry "error" + our request_id.
		if msg.Event != "" {
			select {
			case p.Events <- msg:
			default: // buffer full, drop it
			}
			continue
		}
		p.mu.Lock()
		ch, ok := p.waiters[msg.RequestID]
		delete(p.waiters, msg.RequestID)
		p.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// Call runs one mpv command and waits for its reply.
func (p *Player) Call(timeout time.Duration, args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan message, 1)
	p.waiters[id] = ch
	p.mu.Unlock()

	req, _ := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{args, id})

	p.writeMu.Lock()
	_, err := p.conn.Write(append(req, '\n')) // one message per line
	p.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case msg := <-ch:
		if msg.Error != "success" {
			return msg.Data, fmt.Errorf("mpv: %s", msg.Error)
		}
		return msg.Data, nil
	case <-time.After(timeout):
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
		return nil, errors.New("mpv: reply timeout")
	case <-p.done:
		return nil, errors.New("mpv: connection closed")
	}
}

// ---- thin helpers over common commands ----

// mode: "replace", "append" or "append-play"
func (p *Player) Load(path, mode string) error {
	_, err := p.Call(5*time.Second, "loadfile", path, mode)
	return err
}

func (p *Player) SetPause(paused bool) error {
	_, err := p.Call(2*time.Second, "set_property", "pause", paused)
	return err
}

func (p *Player) Seek(seconds float64) error {
	_, err := p.Call(2*time.Second, "seek", seconds, "relative")
	return err
}

func (p *Player) Next() error {
	_, err := p.Call(2*time.Second, "playlist-next", "weak")
	return err
}

// Observe makes mpv push property-change events with this id.
func (p *Player) Observe(id int64, prop string) error {
	_, err := p.Call(2*time.Second, "observe_property", id, prop)
	return err
}

func (p *Player) Close() {
	p.Call(time.Second, "quit")
	p.conn.Close()
	p.cmd.Wait()
}
