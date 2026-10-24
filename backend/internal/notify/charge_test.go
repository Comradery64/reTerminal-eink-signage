package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 94% on the firmware's own 3300..4150 mV line, the number the dashboard shows, is 4099 mV.
func TestChargedLineIs94PercentOfTheDisplayedScale(t *testing.T) {
	if chargeDoneMV != 4099 {
		t.Fatalf("94%% of 3300..4150 mV is 4099, got %d", chargeDoneMV)
	}
}

// rt-quant's real second charge (2026-10-23, 10-minute reports): 4156 mV under charge rested at
// 4042 mV (87%). The message goes out once, at the first report at or above 94%.
func TestChargedFiresOnceAtTheFirstReportAt94Percent(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)
	fired := []int{}
	for i, mv := range []int{3932, 4072, 4108, 4128, 4146, 4156, 4170} {
		if m.EvaluateCharging("rt-q", "Quant", mv, t0.Add(time.Duration(i)*10*time.Minute)) {
			fired = append(fired, mv)
		}
	}
	if len(fired) != 1 || fired[0] != 4108 {
		t.Fatalf("want one message at 4108 mV (95%%), got %v", fired)
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

func TestChargedNotBelow94AndOnceAgainAfterAnUnplugAndNewCharge(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	at := func(i, mv int) bool {
		return m.EvaluateCharging("rt-f", "Full", mv, t0.Add(time.Duration(i)*10*time.Minute))
	}
	at(0, 3800)
	if at(1, 3990) || at(2, 4098) {
		t.Fatal("below 94%: not yet")
	}
	if !at(3, 4110) {
		t.Fatal("at 94% while climbing should fire")
	}
	if at(4, 4150) {
		t.Fatal("must not repeat within the same charge")
	}
	at(5, 4040) // unplugged
	at(6, 4044)
	at(7, 4080) // charging again
	if !at(8, 4105) {
		t.Fatal("a new charge should get its own message")
	}
}

func TestChargedForgetsAfterALongSilence(t *testing.T) {
	m := newTestManager()
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	m.EvaluateCharging("rt-s", "Silent", 4040, t0)
	m.EvaluateCharging("rt-s", "Silent", 4070, t0.Add(10*time.Minute))
	if m.EvaluateCharging("rt-s", "Silent", 4110, t0.Add(10*time.Minute+2*time.Hour)) {
		t.Fatal("after a long gap the trend restarts; one report is not a charge")
	}
}

func TestChargedWording(t *testing.T) {
	title, text := chargedText("plain", "Quant", "rt-quant", 4108)
	if !strings.Contains(title, "Quant") || !strings.Contains(text, "unplug") || strings.Contains(title+text, "rt-quant") {
		t.Fatalf("plain wording: %q / %q", title, text)
	}
}
