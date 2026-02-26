package main

const (
	OpenFret      = 255
	NumStrings    = 6
	MaxFret       = 11
	CmdApplyFrame = 0x10
	SOF0          = 0xAA
	SOF1          = 0x55
)

// Frame is a full-state snapshot of all 6 strings sent to the Arduino in one
// bulk transfer.  Every field is serialised into the 10-byte payload.
type Frame struct {
	Fret      [NumStrings]byte // 0-11 = fret number, 255 = open/muted
	StrumMask byte             // bit N set = strum string N
	ProfileID byte
	Duration  byte
	Seq       byte
}

// Encode builds the on-wire representation:
//
//	[SOF0][SOF1][LEN][CMD][fret0..5][StrumMask][ProfileID][Duration][Seq][CKS]
func (f *Frame) Encode() []byte {
	payload := make([]byte, 0, 10)
	for i := 0; i < NumStrings; i++ {
		payload = append(payload, f.Fret[i])
	}
	payload = append(payload, f.StrumMask, f.ProfileID, f.Duration, f.Seq)

	length := byte(len(payload) + 1) // +1 for CMD byte
	cks := length ^ CmdApplyFrame
	for _, b := range payload {
		cks ^= b
	}

	out := []byte{SOF0, SOF1, length, CmdApplyFrame}
	out = append(out, payload...)
	out = append(out, cks)
	return out
}
