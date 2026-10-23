package notify

import (
	"strings"
	"testing"
	"time"
)

// A charge at 10-minute reports: illustrative, shaped like rt-quant's real climb (2026-10-23,
// 3858 to 4030 mV) but levelling off below the 86% line. Exactly one message,
// at the point the climb levels off.
func TestChargedFiresOnceWhenTheClimbLevelsOff(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	fired := []int{}
	for i, mv := range []int{3858, 3960, 4000, 4020, 4026, 4028, 4029} {
		if m.EvaluateCharging("rt-q", "Quant", mv, t0.Add(time.Duration(i)*10*time.Minute)) {
			fired = append(fired, mv)
		}
	}
	if len(fired) != 1 || fired[0] != 4028 {
		t.Fatalf("want one message at the second flat report (4028), got %v", fired)
	}
}

func TestChargedNotForARestingBatteryOrAnEarlyPause(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	// Resting: flat for hours, never charging.
	for i := 0; i < 20; i++ {
		if m.EvaluateCharging("rt-r", "Rest", 4020, t0.Add(time.Duration(i)*10*time.Minute)) {
			t.Fatal("a resting battery must never produce a charged message")
		}
	}
	// Resting at a full-cell voltage (just unplugged, or a trickle charger) is not a charge either.
	for i := 0; i < 5; i++ {
		if m.EvaluateCharging("rt-h", "High", 4160, t0.Add(time.Duration(i)*10*time.Minute)) {
			t.Fatal("a flat reading at full voltage was never seen climbing: not a charge to announce")
		}
	}
	// Charging that pauses well below the floor is not done.
	for i, mv := range []int{3700, 3760, 3800, 3804, 3806, 3806} {
		if m.EvaluateCharging("rt-e", "Early", mv, t0.Add(time.Duration(i)*10*time.Minute)) {
			t.Fatalf("paused at %d mV, below the floor: not charged", mv)
		}
	}
}

func TestChargedFiresAt86PercentWhileClimbingAndAgainAfterAnUnplugAndNewCharge(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	at := func(i int, mv int) bool {
		return m.EvaluateCharging("rt-f", "Full", mv, t0.Add(time.Duration(i)*10*time.Minute))
	}
	at(0, 3990)
	if !at(1, 4040) {
		t.Fatal("full-cell voltage while climbing should fire")
	}
	if at(2, 4050) {
		t.Fatal("must not repeat within the same charge")
	}
	at(3, 3960) // unplugged
	at(4, 3964)
	at(5, 3990) // charging again
	if !at(6, 4040) {
		t.Fatal("a new charge should get its own message")
	}
}

func TestChargedForgetsAfterALongSilence(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	m.EvaluateCharging("rt-s", "Silent", 4010, t0)
	m.EvaluateCharging("rt-s", "Silent", 4040, t0.Add(10*time.Minute))
	if m.EvaluateCharging("rt-s", "Silent", 4040, t0.Add(10*time.Minute+2*time.Hour)) {
		t.Fatal("after a long gap the trend restarts; one report is not enough to call it done")
	}
}

func TestChargedWording(t *testing.T) {
	title, text := chargedText("plain", "Quant", "rt-quant", 4064)
	if !strings.Contains(title, "Quant") || !strings.Contains(text, "unplug") || strings.Contains(title+text, "rt-quant") {
		t.Fatalf("plain wording: %q / %q", title, text)
	}
}

// 86% on the firmware's own scale is the line the team chose; just under it must stay quiet.
func TestChargedThresholdIs86PercentOfTheDisplayedScale(t *testing.T) {
	if chargeFullMV != 4031 {
		t.Fatalf("86%% of 3300..4150 mV is 4031, got %d", chargeFullMV)
	}
	m := newTestManager()
	t0 := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	m.EvaluateCharging("rt-86", "Edge", 3990, t0)
	if m.EvaluateCharging("rt-86", "Edge", 4030, t0.Add(10*time.Minute)) {
		t.Fatal("4030 mV is 85%: not yet")
	}
	if !m.EvaluateCharging("rt-86", "Edge", 4032, t0.Add(20*time.Minute)) {
		t.Fatal("4032 mV is 86%: tell them")
	}
}
