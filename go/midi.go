package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"gitlab.com/gomidi/midi/v2"
	"gitlab.com/gomidi/midi/v2/drivers"
	"gitlab.com/gomidi/midi/v2/drivers/rtmididrv"
)

// -------------------- Hot-swap config --------------------

// PREFERRED_PATTERNS: devices matching any of these are picked first.
var PREFERRED_PATTERNS = []string{"Launchkey", "Novation"}

// EXCLUDED_PATTERNS: virtual/system ports that are never auto-connected.
var EXCLUDED_PATTERNS = []string{"Midi Through", "Through Port", "Dummy"}

const midiRescanInterval = 1000 * time.Millisecond

// -------------------- MIDIWatcher --------------------

// MIDIWatcher monitors available MIDI inputs and maintains a connection to the
// preferred device.  It handles hot-plug (new device appears) and hot-unplug
// (device disappears) transparently.
//
// onNote is called for every NoteOn / NoteOff while a device is connected.
// onDisconnect is called (from a goroutine) when the active device is lost;
// callers should use it to release all actuators immediately.
type MIDIWatcher struct {
	mu           sync.Mutex
	drv          *rtmididrv.Driver
	inPort       drivers.In
	stopFn       func()
	connected    bool
	selectedName string
	lastRescanAt time.Time

	onNote       func(on bool, pitch int)
	onDisconnect func()
}

// NewMIDIWatcher creates a watcher and initialises the underlying rtmidi
// driver.  Call Close() when done.
func NewMIDIWatcher(onNote func(on bool, pitch int), onDisconnect func()) (*MIDIWatcher, error) {
	drv, err := rtmididrv.New()
	if err != nil {
		return nil, fmt.Errorf("rtmididrv: %w", err)
	}
	return &MIDIWatcher{
		drv:          drv,
		onNote:       onNote,
		onDisconnect: onDisconnect,
	}, nil
}

// Close shuts down the active MIDI connection and the rtmidi driver.
func (m *MIDIWatcher) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeConn()
	m.drv.Close()
}

// Tick should be called on a regular interval (e.g. every second) from the
// main loop.  It scans for devices, auto-connects to a preferred one, and
// detects disappearances.
func (m *MIDIWatcher) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if !m.lastRescanAt.IsZero() && now.Sub(m.lastRescanAt) < midiRescanInterval {
		return
	}
	m.lastRescanAt = now

	inputs := m.listInputs()

	if m.connected {
		// Verify the selected device is still present.
		for _, n := range inputs {
			if n == m.selectedName {
				return // still there, nothing to do
			}
		}
		// Device disappeared.
		logger.Warn("midi: device disappeared", "device", m.selectedName)
		m.closeConn()
		m.lastRescanAt = time.Time{} // rescan immediately next tick
		if m.onDisconnect != nil {
			go m.onDisconnect()
		}
		return
	}

	// Not connected – try to connect.
	if len(inputs) == 0 {
		return
	}
	cand, ok := m.pickPreferred(inputs)
	if !ok {
		return
	}
	if err := m.openByName(cand); err != nil {
		logger.Error("midi: connect failed", "device", cand, "err", err)
	}
}

// -------------------- internal --------------------

func (m *MIDIWatcher) listInputs() []string {
	ins, err := m.drv.Ins()
	if err != nil {
		logger.Error("midi: list inputs failed", "err", err)
		return nil
	}
	var names []string
	for _, in := range ins {
		name := in.String()
		excluded := false
		for _, pat := range EXCLUDED_PATTERNS {
			if containsCI(name, pat) {
				excluded = true
				break
			}
		}
		if excluded {
			logger.Debug("midi: input excluded", "device", name)
		} else {
			names = append(names, name)
		}
	}
	logger.Debug("midi: inputs found", "count", len(names), "devices", strings.Join(names, ", "))
	return names
}

func (m *MIDIWatcher) pickPreferred(inputs []string) (string, bool) {
	for _, pat := range PREFERRED_PATTERNS {
		for _, name := range inputs {
			if containsCI(name, pat) {
				return name, true
			}
		}
	}
	if len(inputs) == 1 {
		return inputs[0], true
	}
	return "", false
}

func (m *MIDIWatcher) closeConn() {
	if m.stopFn != nil {
		m.stopFn()
		m.stopFn = nil
	}
	if m.inPort != nil {
		_ = m.inPort.Close()
		m.inPort = nil
	}
	m.connected = false
	m.selectedName = ""
}

func (m *MIDIWatcher) openByName(name string) error {
	ins, err := m.drv.Ins()
	if err != nil {
		return err
	}
	var found drivers.In
	for _, in := range ins {
		if in.String() == name {
			found = in
			break
		}
	}
	if found == nil {
		return fmt.Errorf("input %q not found", name)
	}
	if err := found.Open(); err != nil {
		return fmt.Errorf("open %q: %w", name, err)
	}

	stop, err := midi.ListenTo(found, func(msg midi.Message, _ int32) {
		var ch, key, vel uint8
		if msg.GetNoteStart(&ch, &key, &vel) {
			logger.Info("midi: note on", "ch", ch, "key", key, "vel", vel)
			m.onNote(true, int(key))
		} else if msg.GetNoteEnd(&ch, &key) {
			logger.Info("midi: note off", "ch", ch, "key", key)
			m.onNote(false, int(key))
		} else {
			logger.Debug("midi: unhandled message", "msg", msg.String())
		}
	}, midi.HandleError(func(listenErr error) {
		logger.Warn("midi: listener error", "device", name, "err", listenErr)
		// Must not call closeConn from within the listener goroutine, so
		// we dispatch to a new goroutine and re-acquire the mutex.
		go func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.connected && m.selectedName == name {
				m.closeConn()
				m.lastRescanAt = time.Time{} // trigger immediate rescan
				if m.onDisconnect != nil {
					go m.onDisconnect()
				}
			}
		}()
	}))
	if err != nil {
		_ = found.Close()
		return fmt.Errorf("listen %q: %w", name, err)
	}

	m.inPort = found
	m.stopFn = stop
	m.connected = true
	m.selectedName = name
	logger.Info("midi: connected", "device", name)
	return nil
}

// -------------------- utility --------------------

func containsCI(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
