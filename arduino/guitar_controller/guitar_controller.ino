// guitar_controller.ino
// Receives bulk frames from the Go host over high-speed serial and drives
// 72 fret actuators (6 strings × 12 frets) through ten 74HC595 shift
// registers via SPI, plus a 6-bit strum mask for future expansion.
//
// Frame wire format (from Go):
//   [0xAA][0x55][LEN][0x10][fret0..5][StrumMask][ProfileID][Duration][Seq][CKS]
//   fret value 255 = open/muted, 0-11 = fret number.

#include <SPI.h>

static constexpr uint8_t NUM_STRINGS    = 6;
static constexpr uint8_t FRETS_PER_STR  = 12;
static constexpr uint8_t NUM_SHIFT_BYTES = 10; // ceil(72 / 8)

// Protocol
static constexpr uint8_t SOF0           = 0xAA;
static constexpr uint8_t SOF1           = 0x55;
static constexpr uint8_t CMD_APPLY_FRAME = 0x10;

// SPI latch (RCLK) for the shift-register chain
static constexpr uint8_t LATCH_PIN = 4;
static constexpr uint32_t BAUD     = 500000;

// -------------------- Shift-register output image --------------------

uint8_t outImage[NUM_SHIFT_BYTES];

static void clearImage() {
  memset(outImage, 0, sizeof(outImage));
}

static void setBit(uint8_t index) {
  if (index < NUM_SHIFT_BYTES * 8)
    outImage[index / 8] |= (1 << (index % 8));
}

static void applyOutputs() {
  digitalWrite(LATCH_PIN, LOW);
  for (int i = NUM_SHIFT_BYTES - 1; i >= 0; i--) {
    SPI.transfer(outImage[i]);
  }
  digitalWrite(LATCH_PIN, HIGH);
}

// -------------------- Frame processing --------------------

// payload layout: [fret0..5][StrumMask][ProfileID][Duration][Seq]
static void processFrame(const uint8_t* payload) {
  clearImage();
  for (uint8_t s = 0; s < NUM_STRINGS; s++) {
    uint8_t fret = payload[s];
    if (fret < FRETS_PER_STR) {
      setBit((uint8_t)(s * FRETS_PER_STR + fret));
    }
    // fret == 255 → open string, leave all bits clear
  }
  applyOutputs();
}

// -------------------- Receiver state machine --------------------

enum RxState { WAIT0, WAIT1, LEN, BODY, CKS };
static RxState rxState = WAIT0;
static uint8_t rxLen   = 0;
static uint8_t rxBuf[32];
static uint8_t rxPos   = 0;
static uint8_t rxCks   = 0;

static void resetRx() {
  rxState = WAIT0;
  rxLen   = 0;
  rxPos   = 0;
  rxCks   = 0;
}

static void serviceSerial() {
  while (Serial.available()) {
    uint8_t b = (uint8_t)Serial.read();
    switch (rxState) {
      case WAIT0:
        if (b == SOF0) rxState = WAIT1;
        break;
      case WAIT1:
        if (b == SOF1) rxState = LEN;
        else           resetRx();
        break;
      case LEN:
        rxLen = b;
        rxCks = b;    // checksum seeds with LEN
        rxPos = 0;
        if (rxLen < 1 || rxLen > (uint8_t)sizeof(rxBuf)) { resetRx(); break; }
        rxState = BODY;
        break;
      case BODY:
        rxBuf[rxPos++] = b;
        rxCks ^= b;
        if (rxPos >= rxLen) rxState = CKS;
        break;
      case CKS:
        if (b == rxCks && rxBuf[0] == CMD_APPLY_FRAME) {
          processFrame(&rxBuf[1]); // payload starts after CMD byte
        }
        resetRx();
        break;
    }
  }
}

// -------------------- Arduino entry points --------------------

void setup() {
  Serial.begin(BAUD);
  SPI.begin();
  pinMode(LATCH_PIN, OUTPUT);
  digitalWrite(LATCH_PIN, HIGH);
  clearImage();
  applyOutputs(); // start with all actuators off
}

void loop() {
  serviceSerial();
}

