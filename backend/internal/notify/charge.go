package notify

import (
	"fmt"
	"time"
)

// Charge detection works from telemetry alone: the unit has no charger-status line the broker can
// read, but a charging battery's voltage climbs report after report, which a resting one never
// does. The firmware's percent is a straight line over voltage, so the "charged" line is a percent
// on that same scale: the one the dashboard shows. It over-reads while charging (rt-quant read 85%
// under charge and rested at 76% minutes after it was unplugged), so the percent here is what the
// display says during the charge, not the resting charge.
const (
	chargeRiseMV   = 15                                   // a rise of at least this between reports means it is charging
	chargeUnplugMV = 40                                   // a drop of at least this ends the charge (unplugged)
	chargeDonePct  = 80                                   // tell them at this displayed percent while charging
	chargeDoneMV   = 3300 + chargeDonePct*(4150-3300)/100 // on the firmware's own 3300..4150 mV line: 3980 mV
	chargeMaxGap   = 45 * time.Minute                     // a longer silence breaks the trend; start again
)

// EvaluateCharging is called on every telemetry report. It sends one "charged, you can unplug it"
// message per charge, when a unit that has been seen climbing reaches chargeDoneMV. A new charge
// starts after an unplug or a long silence. State is in memory, so a broker restart in the middle
// of a charge forgets it and the message for that charge may not be sent.
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
		st.charging, st.chargedSent = false, false
		m.mu.Unlock()
		return false
	}
	delta := mv - prev
	switch {
	case delta >= chargeRiseMV:
		if !st.charging {
			st.chargedSent = false // a new charge
		}
		st.charging = true
	case delta <= -chargeUnplugMV:
		st.charging, st.chargedSent = false, false
	}
	fire := st.charging && !st.chargedSent && mv >= chargeDoneMV
	if fire {
		st.chargedSent = true
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
			fmt.Sprintf("Display %q (device %s) has charged to %d mV (%d%% on the dashboard). You can unplug it.", name, deviceID, mv, chargeDonePct)
	}
	return fmt.Sprintf("🔌 Charged: %s", name), "It has reached 80%. You can unplug it now."
}
