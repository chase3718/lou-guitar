package main

import (
	"container/heap"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/gomidi/midi/v2"
	"gitlab.com/gomidi/midi/v2/drivers"
	"gitlab.com/gomidi/midi/v2/drivers/rtmididrv"
)

// -------------------- Logger --------------------

// logger is the package-wide structured logger. Safe to use before initLogger
// is called; defaults to slog.Default().
var logger = slog.Default()

// initLogger configures the shared slog logger and calls slog.SetDefault so
// the stdlib log package also routes through the same handler.
func initLogger(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: debug, // include file:line in debug mode
	})
	logger = slog.New(h)
	slog.SetDefault(logger) // stdlib log.* now routes through slog
}

// -------------------- Pitch helpers --------------------

var noteNames = [12]string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}

func pitchName(pitch int) string {
	if pitch < 0 {
		return fmt.Sprintf("?\"%d\"", pitch)
	}
	return fmt.Sprintf("%s%d", noteNames[pitch%12], (pitch/12)-1)
}

// -------------------- Tunables --------------------

const HOLD_THRESHOLD_MS = 180
const DISPOSABLE_TTL_MS = 10_000
const TICK_MS = 2
const FRET_READY_MS = 8
const MIDI_RESCAN_MS = 1000

const MAX_FRET = 12
const NUM_STRINGS = 6

var OPEN_PITCH = [NUM_STRINGS]int{40, 45, 50, 55, 59, 64} // E2 A2 D3 G3 B3 E4

var PREFERRED_NAME_PATTERNS = []string{"Launchkey", "Novation"}

// Ports matching any of these patterns are never auto-connected (virtual/system ports).
var EXCLUDED_NAME_PATTERNS = []string{"Midi Through", "Through Port", "Dummy"}

// -------------------- Data Model --------------------

type NoteID struct{ ch, pitch int }

type ActiveNote struct {
	id       NoteID
	pitch    int
	velocity int
	noteOnAt time.Time
	isDown   bool
	isHeld   bool
}

type StringSlot struct {
	idx          int
	assignedID   *NoteID
	pitch        *int
	fret         *int
	assignedAt   *time.Time
	lastPluckAt  *time.Time
	lastUsedAt   *time.Time
	isHeldLock   bool
	ttlExpiresAt *time.Time
	busyUntil    *time.Time
}

type CmdType int

const (
	SET_FRET CmdType = iota
	RELEASE
	DAMP
	PLUCK
)

type Cmd struct {
	typ      CmdType
	stringIx int
	fret     *int
	velocity *int
}

type PlannedCmd struct {
	at  time.Time
	cmd Cmd
}

// -------------------- Min-Heap --------------------

type MinHeap []PlannedCmd

func (h MinHeap) Len() int            { return len(h) }
func (h MinHeap) Less(i, j int) bool  { return h[i].at.Before(h[j].at) }
func (h MinHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *MinHeap) Push(x interface{}) { *h = append(*h, x.(PlannedCmd)) }
func (h *MinHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// -------------------- Globals --------------------

var (
	mu          sync.Mutex
	activeNotes = map[NoteID]*ActiveNote{}
	slots       = func() [NUM_STRINGS]StringSlot {
		var s [NUM_STRINGS]StringSlot
		for i := 0; i < NUM_STRINGS; i++ {
			s[i] = StringSlot{idx: i}
		}
		return s
	}()

	cmdQueue MinHeap

	// MIDI state
	midiConnected    bool
	selectedMidiName string
	lastMidiRescanAt time.Time

	// driver + in port handles
	drv    *rtmididrv.Driver
	inPort drivers.In
	stopFn func()
)

// -------------------- Utilities --------------------

func now() time.Time { return time.Now() }

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// -------------------- Guitar helpers --------------------

func inRangeOrRemap(pitch int) (int, bool) {
	minPitch := OPEN_PITCH[0]
	maxPitch := OPEN_PITCH[NUM_STRINGS-1] + MAX_FRET

	if pitch >= minPitch && pitch <= maxPitch {
		logger.Debug("pitch in range", "pitch", pitchName(pitch))
		return pitch, true
	}

	p := pitch
	for p < minPitch {
		p += 12
	}
	for p > maxPitch {
		p -= 12
	}

	if p >= minPitch && p <= maxPitch {
		logger.Debug("pitch remapped", "original", pitchName(pitch), "remapped", pitchName(p))
		return p, true
	}
	logger.Warn("pitch out of range and cannot be remapped", "pitch", pitchName(pitch))
	return 0, false
}

type Pos struct{ s, f int }

func possiblePositions(pitch int) []Pos {
	out := []Pos{}
	for s := 0; s < NUM_STRINGS; s++ {
		f := pitch - OPEN_PITCH[s]
		if f >= 0 && f <= MAX_FRET {
			out = append(out, Pos{s, f})
		}
	}
	// insertion sort: by fret asc, then string asc
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.f > b.f || (a.f == b.f && a.s > b.s) {
				out[j-1], out[j] = out[j], out[j-1]
			} else {
				break
			}
		}
	}
	posStrs := make([]string, len(out))
	for i, p := range out {
		posStrs[i] = fmt.Sprintf("s%d/f%d", p.s, p.f)
	}
	logger.Debug("possible positions", "pitch", pitchName(pitch), "positions", strings.Join(posStrs, " "))
	return out
}

// -------------------- Slot helpers --------------------

func slotFree(s int, t time.Time) bool {
	sl := slots[s]
	if sl.assignedID != nil {
		return false
	}
	if sl.busyUntil != nil && t.Before(*sl.busyUntil) {
		return false
	}
	return true
}

func canSteal(s int, t time.Time) bool {
	sl := slots[s]
	if sl.assignedID == nil {
		return false
	}
	if sl.busyUntil != nil && t.Before(*sl.busyUntil) {
		return false
	}
	return true
}

func stealScore(s int, t time.Time) int {
	sl := slots[s]
	score := 0
	if sl.isHeldLock {
		score += 1_000_000
	}

	if sl.ttlExpiresAt != nil && !t.Before(*sl.ttlExpiresAt) {
		score -= 500_000
	}

	if sl.lastPluckAt != nil {
		ageMs := int(t.Sub(*sl.lastPluckAt).Milliseconds())
		score -= ageMs * 10
	}

	if sl.fret != nil {
		score -= (*sl.fret) * 50
	}

	logger.Debug("steal score", "string", s, "score", score, "held", sl.isHeldLock)
	return score
}

func pickVictimSlot(t time.Time) (int, bool) {
	bestIx := -1
	bestScore := int(^uint(0) >> 1) // max int
	for s := 0; s < NUM_STRINGS; s++ {
		if canSteal(s, t) {
			sc := stealScore(s, t)
			if sc < bestScore {
				bestScore = sc
				bestIx = s
			}
		}
	}
	if bestIx < 0 {
		logger.Warn("no stealable slot found — dropping note")
		return 0, false
	}
	logger.Debug("victim slot selected", "string", bestIx, "score", bestScore)
	return bestIx, true
}

// -------------------- Command queue helpers --------------------

func cmdTypeName(c CmdType) string {
	switch c {
	case SET_FRET:
		return "SET_FRET"
	case RELEASE:
		return "RELEASE"
	case DAMP:
		return "DAMP"
	case PLUCK:
		return "PLUCK"
	}
	return "UNKNOWN"
}

func enqueue(at time.Time, cmd Cmd) {
	delay := time.Until(at)
	if cmd.fret != nil {
		logger.Debug("enqueue cmd", "type", cmdTypeName(cmd.typ), "string", cmd.stringIx, "fret", *cmd.fret, "delay_ms", delay.Milliseconds())
	} else if cmd.velocity != nil {
		logger.Debug("enqueue cmd", "type", cmdTypeName(cmd.typ), "string", cmd.stringIx, "velocity", *cmd.velocity, "delay_ms", delay.Milliseconds())
	} else {
		logger.Debug("enqueue cmd", "type", cmdTypeName(cmd.typ), "string", cmd.stringIx, "delay_ms", delay.Milliseconds())
	}
	heap.Push(&cmdQueue, PlannedCmd{at: at, cmd: cmd})
}

func flushDueCommands(t time.Time) {
	flushed := 0
	for cmdQueue.Len() > 0 && !t.Before(cmdQueue[0].at) {
		pc := heap.Pop(&cmdQueue).(PlannedCmd)
		sendToMCU(pc.cmd)
		flushed++
	}
	if flushed > 0 {
		logger.Debug("flushed commands", "count", flushed, "remaining", cmdQueue.Len())
	}
}

// -------------------- MCU output --------------------

func sendToMCU(cmd Cmd) {
	switch cmd.typ {
	case SET_FRET:
		if cmd.fret != nil {
			logger.Info("MCU SET_FRET", "string", cmd.stringIx, "fret", *cmd.fret)
		}
	case RELEASE:
		logger.Info("MCU RELEASE", "string", cmd.stringIx)
	case DAMP:
		logger.Info("MCU DAMP", "string", cmd.stringIx)
	case PLUCK:
		if cmd.velocity != nil {
			logger.Info("MCU PLUCK", "string", cmd.stringIx, "velocity", *cmd.velocity)
		}
	}
}

// -------------------- Core: Assign + Play --------------------

func assignAndPlay(note *ActiveNote) {
	t := now()

	logger.Debug("assignAndPlay", "pitch", pitchName(note.pitch), "velocity", note.velocity, "ch", note.id.ch, "isHeld", note.isHeld)

	mapped, ok := inRangeOrRemap(note.pitch)
	if !ok {
		logger.Warn("assignAndPlay: pitch cannot be mapped, dropping note", "pitch", pitchName(note.pitch))
		return
	}

	positions := possiblePositions(mapped)
	if len(positions) == 0 {
		logger.Warn("assignAndPlay: no playable positions found", "pitch", pitchName(mapped))
		return
	}

	chosenS := -1
	chosenF := 0

	// 1) free slot first
	for _, p := range positions {
		if slotFree(p.s, t) {
			chosenS = p.s
			chosenF = p.f
			break
		}
	}

	if chosenS != -1 {
		logger.Debug("free slot assigned", "string", chosenS, "fret", chosenF, "pitch", pitchName(mapped))
	}

	// 2) steal if needed
	if chosenS == -1 {
		logger.Debug("no free slot available, attempting steal", "pitch", pitchName(mapped))
		victim, vok := pickVictimSlot(t)
		if !vok {
			logger.Warn("assignAndPlay: all slots held/busy — note dropped", "pitch", pitchName(mapped))
			return
		}

		// prefer victim's string if it can play the note
		found := false
		for _, p := range positions {
			if p.s == victim {
				chosenS, chosenF = p.s, p.f
				found = true
				break
			}
		}
		if !found {
			chosenS, chosenF = positions[0].s, positions[0].f
		}

		var victimPitch string
		if slots[victim].pitch != nil {
			victimPitch = pitchName(*slots[victim].pitch)
		}
		logger.Info("stealing slot", "victim_string", victim, "victim_pitch", victimPitch, "new_pitch", pitchName(mapped), "new_string", chosenS, "new_fret", chosenF)

		// release victim (optional damp)
		enqueue(t, Cmd{typ: DAMP, stringIx: victim})
		enqueue(t, Cmd{typ: RELEASE, stringIx: victim})

		// clear victim immediately
		slots[victim].assignedID = nil
		slots[victim].pitch = nil
		slots[victim].fret = nil
		slots[victim].isHeldLock = false
		slots[victim].ttlExpiresAt = nil
		bu := t.Add(ms(2))
		slots[victim].busyUntil = &bu
	}

	// 3) reserve chosen slot
	s := chosenS
	id := note.id
	slots[s].assignedID = &id
	mp := mapped
	slots[s].pitch = &mp
	cf := chosenF
	slots[s].fret = &cf
	slots[s].isHeldLock = note.isHeld

	if note.isHeld {
		slots[s].ttlExpiresAt = nil
		logger.Debug("slot reserved (held — no TTL)", "string", s, "fret", cf, "pitch", pitchName(mapped))
	} else {
		exp := t.Add(ms(DISPOSABLE_TTL_MS))
		slots[s].ttlExpiresAt = &exp
		logger.Debug("slot reserved (disposable)", "string", s, "fret", cf, "pitch", pitchName(mapped), "ttl_ms", DISPOSABLE_TTL_MS)
	}

	// 4) schedule fret + pluck
	enqueue(t, Cmd{typ: SET_FRET, stringIx: s, fret: &cf})
	readyAt := t.Add(ms(FRET_READY_MS))
	vel := note.velocity
	enqueue(readyAt, Cmd{typ: PLUCK, stringIx: s, velocity: &vel})

	bu := readyAt.Add(ms(2))
	slots[s].busyUntil = &bu
	slots[s].lastPluckAt = &readyAt

	logger.Info("note scheduled", "pitch", pitchName(mapped), "string", s, "fret", cf, "velocity", vel, "pluck_delay_ms", FRET_READY_MS)

	flushDueCommands(now())
}

// -------------------- MIDI event handlers --------------------

func onNoteOn(ch, pitch, vel int) {
	t := now()
	id := NoteID{ch: ch, pitch: pitch}

	logger.Info("NOTE ON", "pitch", pitchName(pitch), "midi_pitch", pitch, "velocity", vel, "channel", ch)

	n := &ActiveNote{
		id:       id,
		pitch:    pitch,
		velocity: vel,
		noteOnAt: t,
		isDown:   true,
		isHeld:   false,
	}
	activeNotes[id] = n
	logger.Debug("active notes", "count", len(activeNotes))
	assignAndPlay(n)
}

func onNoteOff(ch, pitch int) {
	t := now()
	id := NoteID{ch: ch, pitch: pitch}

	logger.Info("NOTE OFF", "pitch", pitchName(pitch), "midi_pitch", pitch, "channel", ch)

	n := activeNotes[id]
	if n == nil {
		logger.Warn("NOTE OFF for unknown note — ignoring", "pitch", pitchName(pitch), "channel", ch)
		return
	}
	n.isDown = false

	heldDuration := t.Sub(n.noteOnAt)
	logger.Debug("note off details", "pitch", pitchName(pitch), "was_held", n.isHeld, "held_duration_ms", heldDuration.Milliseconds())

	// find slot playing it
	for s := 0; s < NUM_STRINGS; s++ {
		if slots[s].assignedID != nil && *slots[s].assignedID == id {
			if n.isHeld {
				logger.Info("releasing held note", "pitch", pitchName(pitch), "string", s)
				enqueue(t, Cmd{typ: RELEASE, stringIx: s})
				// optional: enqueue(t, Cmd{typ: DAMP, stringIx: s})
				slots[s].assignedID = nil
				slots[s].pitch = nil
				slots[s].fret = nil
				slots[s].isHeldLock = false
				slots[s].ttlExpiresAt = nil
			} else {
				// tapped note: keep latched until TTL
				exp := t.Add(ms(DISPOSABLE_TTL_MS))
				slots[s].ttlExpiresAt = &exp
				logger.Debug("tapped note latched until TTL", "pitch", pitchName(pitch), "string", s, "ttl_ms", DISPOSABLE_TTL_MS)
			}
			delete(activeNotes, id)
			logger.Debug("active notes", "count", len(activeNotes))
			break
		}
	}

	flushDueCommands(now())
}

// -------------------- Scheduler tick --------------------

func schedulerTick() {
	t := now()

	// 1) promote to HELD
	for _, n := range activeNotes {
		if n.isDown && !n.isHeld && t.Sub(n.noteOnAt) >= ms(HOLD_THRESHOLD_MS) {
			n.isHeld = true
			logger.Info("note promoted to HELD", "pitch", pitchName(n.pitch), "held_ms", t.Sub(n.noteOnAt).Milliseconds())
			// propagate lock to its slot
			for s := 0; s < NUM_STRINGS; s++ {
				if slots[s].assignedID != nil && *slots[s].assignedID == n.id {
					slots[s].isHeldLock = true
					slots[s].ttlExpiresAt = nil
					logger.Debug("slot hold-locked", "string", s, "pitch", pitchName(n.pitch))
				}
			}
		}
	}

	// 2) expire disposables
	for s := 0; s < NUM_STRINGS; s++ {
		if slots[s].assignedID != nil && !slots[s].isHeldLock &&
			slots[s].ttlExpiresAt != nil && !t.Before(*slots[s].ttlExpiresAt) {

			var expiredPitch string
			if slots[s].pitch != nil {
				expiredPitch = pitchName(*slots[s].pitch)
			}
			logger.Info("TTL expired, releasing slot", "string", s, "pitch", expiredPitch)
			enqueue(t, Cmd{typ: RELEASE, stringIx: s})
			// optional: enqueue(t, Cmd{typ: DAMP, stringIx: s})
			slots[s].assignedID = nil
			slots[s].pitch = nil
			slots[s].fret = nil
			slots[s].ttlExpiresAt = nil
		}
	}

	flushDueCommands(t)
}

// -------------------- MIDI hot-plug watcher --------------------

func listMidiInputsViaDriver() ([]string, error) {
	ins, err := drv.Ins()
	if err != nil {
		logger.Error("failed to list MIDI inputs", "err", err)
		return nil, err
	}
	names := []string{}
	for _, in := range ins {
		name := in.String()
		excluded := false
		for _, pat := range EXCLUDED_NAME_PATTERNS {
			if containsCI(name, pat) {
				excluded = true
				break
			}
		}
		if excluded {
			logger.Debug("MIDI input excluded (virtual/system port)", "device", name)
			continue
		}
		names = append(names, name)
	}
	logger.Debug("MIDI inputs enumerated (after exclusions)", "count", len(names), "inputs", strings.Join(names, ", "))
	return names, nil
}

func pickPreferred(inputs []string) (string, bool) {
	for _, pat := range PREFERRED_NAME_PATTERNS {
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

func closeMidi() {
	logger.Info("closing MIDI connection", "device", selectedMidiName)
	if stopFn != nil {
		stopFn()
		stopFn = nil
	}
	if inPort != nil {
		_ = inPort.Close()
		inPort = nil
	}
	midiConnected = false
	selectedMidiName = ""
	logger.Info("MIDI connection closed")
}

func openMidiByName(name string) error {
	logger.Info("opening MIDI input", "device", name)
	ins, err := drv.Ins()
	if err != nil {
		logger.Error("failed to list MIDI inputs when opening", "err", err)
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
		err := fmt.Errorf("MIDI input %q not found", name)
		logger.Error("MIDI input not found", "device", name)
		return err
	}
	if err := found.Open(); err != nil {
		logger.Error("failed to open MIDI port", "device", name, "err", err)
		return err
	}
	stop, err := midi.ListenTo(found, func(msg midi.Message, timestampms int32) {
		mu.Lock()
		defer mu.Unlock()
		onMidiMessage(msg)
	}, midi.HandleError(func(listenErr error) {
		logger.Warn("MIDI listener error — device likely disconnected", "device", name, "err", listenErr)
		// Run in a new goroutine: we must not call closeMidi (which calls stopFn)
		// from within the listener goroutine itself.
		go func() {
			mu.Lock()
			defer mu.Unlock()
			if midiConnected && selectedMidiName == name {
				closeMidi()
				onMidiDisconnectPanicRelease()
				// Reset rescan timer so watcher reconnects on next tick.
				lastMidiRescanAt = time.Time{}
			}
		}()
	}))
	if err != nil {
		logger.Error("failed to start MIDI listener", "device", name, "err", err)
		_ = found.Close()
		return err
	}
	inPort = found
	stopFn = stop
	midiConnected = true
	selectedMidiName = name
	logger.Info("MIDI input connected", "device", name)
	return nil
}

func onMidiDisconnectPanicRelease() {
	logger.Warn("MIDI disconnect — panic releasing all strings")
	t := now()
	for s := 0; s < NUM_STRINGS; s++ {
		enqueue(t, Cmd{typ: RELEASE, stringIx: s})
		// enqueue(t, Cmd{typ: DAMP, stringIx: s})
		slots[s].assignedID = nil
		slots[s].pitch = nil
		slots[s].fret = nil
		slots[s].isHeldLock = false
		slots[s].ttlExpiresAt = nil
	}
	flushDueCommands(now())
	logger.Info("panic release complete — all strings cleared")
}

func midiWatcherTick() {
	t := now()
	if !lastMidiRescanAt.IsZero() && t.Sub(lastMidiRescanAt) < ms(MIDI_RESCAN_MS) {
		return
	}
	lastMidiRescanAt = t

	logger.Debug("MIDI watcher scanning")

	inputs, err := listMidiInputsViaDriver()
	if err != nil {
		logger.Error("MIDI watcher: failed to enumerate inputs", "err", err)
		return
	}

	// if connected, verify selected still exists
	if midiConnected && selectedMidiName != "" {
		stillThere := false
		for _, n := range inputs {
			if n == selectedMidiName {
				stillThere = true
				break
			}
		}
		if stillThere {
			logger.Debug("MIDI device still present", "device", selectedMidiName)
			return
		}

		// disappeared
		logger.Warn("MIDI device disappeared", "device", selectedMidiName)
		closeMidi()
		onMidiDisconnectPanicRelease()
		// Reset rescan timer so we attempt reconnect on next tick.
		lastMidiRescanAt = time.Time{}
	}

	// if not connected, attempt connect
	if !midiConnected {
		if len(inputs) == 0 {
			logger.Debug("no MIDI inputs available")
			return
		}
		cand, ok := pickPreferred(inputs)
		if !ok {
			logger.Debug("no preferred MIDI device found", "available", strings.Join(inputs, ", "))
			return
		}
		logger.Info("attempting to connect to MIDI device", "device", cand)
		if err := openMidiByName(cand); err != nil {
			logger.Error("failed to connect to MIDI device", "device", cand, "err", err)
			// Back off: let the rescan interval elapse before retrying.
		} else {
			// Connected — reset timer so we don't immediately re-check.
			lastMidiRescanAt = t
		}
	}
}

// -------------------- MIDI message parsing --------------------

func onMidiMessage(msg midi.Message) {
	var ch, key, vel uint8
	if msg.GetNoteStart(&ch, &key, &vel) {
		logger.Debug("raw MIDI note start", "ch", ch, "key", key, "vel", vel)
		onNoteOn(int(ch), int(key), int(vel))
		return
	}
	if msg.GetNoteEnd(&ch, &key) {
		logger.Debug("raw MIDI note end", "ch", ch, "key", key)
		onNoteOff(int(ch), int(key))
		return
	}
	logger.Debug("unhandled MIDI message", "msg", msg.String())
}

// -------------------- Main --------------------

func main() {
	debug := flag.Bool("debug", false, "enable debug logging (adds source location)")
	serialDev := flag.String("serial", "/dev/ttyACM0", "serial port device")
	baud := flag.Int("baud", 500000, "serial baud rate")
	flag.Parse()

	initLogger(*debug)
	logger.Info("lou-guitar starting",
		"serial", *serialDev,
		"baud", *baud,
		"debug", *debug,
		"hold_threshold_ms", HOLD_THRESHOLD_MS,
		"disposable_ttl_ms", DISPOSABLE_TTL_MS,
		"fret_ready_ms", FRET_READY_MS,
		"num_strings", NUM_STRINGS,
		"max_fret", MAX_FRET,
	)

	sp := OpenSerial(*serialDev, *baud)
	defer sp.Close()

	state := NewGuitarState()
	var seq byte
	var stateMu sync.Mutex

	// onNote is called from the MIDI listener goroutine.
	onNote := func(on bool, pitch int) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if on {
			state.ApplyNoteOn(pitch)
		} else {
			state.ApplyNoteOff(pitch)
		}
		frame := state.BuildFrame(seq)
		sp.SendFrame(frame)
		seq++
	}

	// onDisconnect: panic-release all strings immediately.
	onDisconnect := func() {
		logger.Warn("midi: disconnect – panic-releasing all strings")
		stateMu.Lock()
		defer stateMu.Unlock()
		state.ClearAll()
		sp.SendFrame(EmptyFrame(seq))
		seq++
		logger.Info("panic release complete")
	}

	watcher, err := NewMIDIWatcher(onNote, onDisconnect)
	if err != nil {
		logger.Error("midi watcher init failed", "err", err)
		os.Exit(1)
	}
	defer watcher.Close()

	logger.Info("running – waiting for MIDI device")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		watcher.Tick()
	}
}
