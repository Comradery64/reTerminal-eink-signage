package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 80% on the firmware's own 3300..4150 mV line, the number the dashboard shows, is 3980 mV.
func TestChargedLineIs80PercentOfTheDisplayedScale(t *testing.T) {
	if chargeDoneMV != 3980 {
		t.Fatalf("80%% of 3300..4150 mV is 3980, got %d", chargeDoneMV)
	}
}

// rt-quant's real climb (2026-10-23, 10-minute reports): it reached 85% under charge. The message
// goes out once, at the first report at or above 80%.
func TestChargedFiresOnceAtTheFirstReportAt80Percent(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)
	fired := []int{}
	for i, mv := range []int{3694, 3842, 3892, 3914, 3944, 3964, 3988, 4010, 4030} {
		if m.EvaluateCharging("rt-q", "Quant", mv, t0.Add(time.Duration(i)*10*time.Minute)) {
			fired = append(fired, mv)
		}
	}
	if len(fired) != 1 || fired[0] != 3988 {
		t.Fatalf("want one message at 3988 mV (80%%), got %v", fired)
	}
}

func TestChargedNotForARestingBatteryEvenWhenFull(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	for _, mv := range []int{3990, 4100, 4160} {
		for i := 0; i < 10; i++ {
			if m.EvaluateCharging(fmt.Sprintf("rt-rest-%d", mv), "Rest", mv, t0.Add(time.Duration(i)*10*time.Minute)) {
				t.Fatalf("a battery resting flat at %d mV was never seen climbing: not a charge to announce", mv)
			}
		}
	}
}

func TestChargedNotBelow80AndOnceAgainAfterAnUnplugAndNewCharge(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	at := func(i, mv int) bool {
		return m.EvaluateCharging("rt-f", "Full", mv, t0.Add(time.Duration(i)*10*time.Minute))
	}
	at(0, 3800)
	if at(1, 3900) || at(2, 3978) {
		t.Fatal("below 80%: not yet")
	}
	if !at(3, 3990) {
		t.Fatal("at 80% while climbing should fire")
	}
	if at(4, 4040) {
		t.Fatal("must not repeat within the same charge")
	}
	at(5, 3940) // unplugged
	at(6, 3944)
	at(7, 3970) // charging again
	if !at(8, 3985) {
		t.Fatal("a new charge should get its own message")
	}
}

func TestChargedForgetsAfterALongSilence(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	m.EvaluateCharging("rt-s", "Silent", 3900, t0)
	m.EvaluateCharging("rt-s", "Silent", 3940, t0.Add(10*time.Minute))
	if m.EvaluateCharging("rt-s", "Silent", 3990, t0.Add(10*time.Minute+2*time.Hour)) {
		t.Fatal("after a long gap the trend restarts; one report is not a charge")
	}
}

func TestChargedWording(t *testing.T) {
	title, text := chargedText("plain", "Quant", "rt-quant", 3988)
	if !strings.Contains(title, "Quant") || !strings.Contains(text, "unplug") || strings.Contains(title+text, "rt-quant") {
		t.Fatalf("plain wording: %q / %q", title, text)
	}
}
