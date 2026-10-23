package notify

import (
	"fmt"
	"time"
)

// Charge detection works from telemetry alone: the unit has no charger-status line the broker can
// read, but a charging battery's voltage climbs report after report, which a resting one never
// does. The firmware's percent is a straight line over voltage and over-reads while charging (a
// unit showing 80% under charge rested at 65% minutes after it was unplugged), so the "charged"
// call is made from the shape of the climb, not from the percent.
const (
	chargeRiseMV     = 15               // a rise of at least this between reports means it is charging
	chargeFlatMV     = 10               // a rise below this, while charging, counts as levelling off
	chargeUnplugMV   = 40               // a drop of at least this ends the charge (unplugged)
	chargeStallsDone = 2                // consecutive levelling-off reports that mean "done"
	chargeFloorMV    = 4000             // never call it done below this; an early pause is not full
	chargeFullMV     = 4150             // at/above this it is done whatever the trend
	chargeMaxGap     = 45 * time.Minute // a longer silence breaks the trend; start again
)

// EvaluateCharging is called on every telemetry report. It sends one "charged, you can unplug it"
// message per charge: when the voltage has stopped climbing near the top, or reaches the full-cell
// voltage. A new charge starts after an unplug or a long silence. State is in memory, so a broker
// restart in the middle of a charge forgets it and the message for that charge may not be sent.
// Returns whether a message was fired.
func (m *Manager) EvaluateCharging(deviceID, room string, mv int, now time.Time) bool {
	if mv <= 0 {
		return false
	}
	m.mu.Lock()
	st := m.state[deviceID]
	if st == nil {
		st = &devState{}
		m.state[deviceID] = st
	}
	prev, prevAt := st.lastMV, st.lastMVAt
	st.lastMV, st.lastMVAt = mv, now

	if prev == 0 || now.Sub(prevAt) > chargeMaxGap {
		st.charging, st.stalls, st.chargedSent = false, 0, false
		m.mu.Unlock()
		return false
	}
	delta := mv - prev
	var fire bool
	switch {
	case delta >= chargeRiseMV:
		if !st.charging {
			st.chargedSent = false // a new charge
		}
		st.charging, st.stalls = true, 0
	case delta <= -chargeUnplugMV:
		st.charging, st.stalls, st.chargedSent = false, 0, false
	case st.charging && delta < chargeFlatMV:
		st.stalls++
	}
	if st.charging && !st.chargedSent &&
		((st.stalls >= chargeStallsDone && mv >= chargeFloorMV) || mv >= chargeFullMV) {
		st.chargedSent = true
		fire = true
	}
	cfg := m.current()
	m.mu.Unlock()

	if !fire {
		return false
	}
	name := room
	if name == "" {
		name = deviceID
	}
	title, text := chargedText(cfg.Style, name, deviceID, mv)
	m.log.Info("charged alert", "device", deviceID, "room", room, "mv", mv)
	m.dispatch(Message{Title: title, Text: text, DeviceID: deviceID, Room: room})
	return true
}

func chargedText(style, name, deviceID string, mv int) (title, text string) {
	switch NormalizeStyle(style) {
	case StyleMinimal:
		return fmt.Sprintf("🔌 %s: charged", name), ""
	case StyleDetailed:
		return fmt.Sprintf("🔌 Charged: %s", name),
			fmt.Sprintf("Display %q (device %s) has stopped charging up at %d mV. You can unplug it.", name, deviceID, mv)
	}
	return fmt.Sprintf("🔌 Charged: %s", name), "It has stopped charging up. You can unplug it now."
}
