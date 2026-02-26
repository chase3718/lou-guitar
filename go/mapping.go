package main

// openPitch is the MIDI pitch of each open string:
// E2(40)  A2(45)  D3(50)  G3(55)  B3(59)  E4(64)
var openPitch = [NumStrings]int{40, 45, 50, 55, 59, 64}

// GuitarState tracks which MIDI pitches are currently active and maps them to
// a 6-string fret frame.
type GuitarState struct {
	ActiveNotes map[int]bool
}

func NewGuitarState() *GuitarState {
	return &GuitarState{ActiveNotes: make(map[int]bool)}
}

func (g *GuitarState) ApplyNoteOn(pitch int) {
	g.ActiveNotes[pitch] = true
}

func (g *GuitarState) ApplyNoteOff(pitch int) {
	delete(g.ActiveNotes, pitch)
}

// ClearAll releases all active notes (used on MIDI disconnect).
func (g *GuitarState) ClearAll() {
	g.ActiveNotes = make(map[int]bool)
}

// BuildFrame converts the current active-note set into a Frame by assigning
// each pitch to the lowest available fret on the first free string that can
// play it.  One pitch per string maximum.
func (g *GuitarState) BuildFrame(seq byte) Frame {
	f := Frame{}
	for i := 0; i < NumStrings; i++ {
		f.Fret[i] = OpenFret
	}

	for pitch := range g.ActiveNotes {
		assigned := false
		for s := 0; s < NumStrings; s++ {
			fret := pitch - openPitch[s]
			if fret >= 0 && fret <= MaxFret {
				if f.Fret[s] == OpenFret { // string not yet claimed
					f.Fret[s] = byte(fret)
					logger.Debug("mapping: pitch assigned", "pitch", pitch, "string", s, "fret", fret)
					assigned = true
					break
				}
			}
		}
		if !assigned {
			logger.Warn("mapping: pitch unassignable (no free string in range)", "pitch", pitch)
		}
	}

	// Strum any string that has an active fret assignment.
	for s := 0; s < NumStrings; s++ {
		if f.Fret[s] != OpenFret {
			f.StrumMask |= 1 << s
		}
	}

	f.ProfileID = 0
	f.Duration = 20
	f.Seq = seq
	logger.Debug("mapping: frame built",
		"seq", seq,
		"active_notes", len(g.ActiveNotes),
		"strum_mask", f.StrumMask,
	)
	return f
}

// EmptyFrame returns an all-open, no-strum frame (used for panic clear).
func EmptyFrame(seq byte) Frame {
	f := Frame{Seq: seq}
	for i := 0; i < NumStrings; i++ {
		f.Fret[i] = OpenFret
	}
	return f
}
