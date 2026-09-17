package ui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"

	"easy-alarms/internal/alarm"
	"easy-alarms/internal/autostart"
	"easy-alarms/internal/humanize"
)

// setupTray installs the StatusNotifierItem icon. Note GNOME's appindicator
// extension routes every click to the menu, so "open window" must be a menu
// item rather than a primary-click action.
func (u *UI) setupTray() {
	desk, ok := u.app.(desktop.App)
	if !ok {
		return
	}
	u.desk = desk

	// Informational, non-clickable header showing the soonest alarm.
	u.trayNext = fyne.NewMenuItem("", nil)
	u.trayNext.Disabled = true

	auto := fyne.NewMenuItem("Arrancar al iniciar sesión", nil)
	auto.Checked = autostart.Enabled()

	// One-click timers straight from the tray, no dialog involved.
	quick := fyne.NewMenuItem("⏱ Timer rápido", nil)
	var quickItems []*fyne.MenuItem
	for _, d := range []time.Duration{5 * time.Minute, 10 * time.Minute, 25 * time.Minute, time.Hour} {
		quickItems = append(quickItems, fyne.NewMenuItem(humanize.Compact(d), func() {
			fyne.Do(func() { u.startQuickTimer(d) })
		}))
	}
	quick.ChildMenu = fyne.NewMenu("", quickItems...)

	u.trayMenu = fyne.NewMenu("Easy Alarms",
		u.trayNext,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Abrir Easy Alarms", func() {
			u.showMain()
			u.win.RequestFocus()
		}),
		quick,
		fyne.NewMenuItemSeparator(),
		auto,
	)

	auto.Action = func() {
		var err error
		if auto.Checked {
			err = autostart.Disable()
		} else {
			err = autostart.Enable()
		}
		if err != nil {
			dialog.ShowError(err, u.win)
			return
		}
		auto.Checked = !auto.Checked
		desk.SetSystemTrayMenu(u.trayMenu)
	}

	u.updateTrayNext()
	desk.SetSystemTrayMenu(u.trayMenu)
	desk.SetSystemTrayIcon(appIcon)
}

// updateTrayNext refreshes the tray header with the soonest upcoming alarm.
// It only re-pushes the menu when the text changes, so the once-per-second
// caller is cheap. Safe to call when the tray is disabled.
func (u *UI) updateTrayNext() {
	if u.desk == nil {
		return
	}
	text := "🔕 Sin alarmas activas"
	if a, next := u.soonest(); a != nil {
		text = "⏰ Próxima: " + describeNextCoarse(next, time.Now())
	}
	if text == u.trayLast {
		return
	}
	u.trayLast = text
	u.trayNext.Label = text
	u.desk.SetSystemTrayMenu(u.trayMenu)
}

// startQuickTimer creates and starts an unlabeled timer in one go.
func (u *UI) startQuickTimer(d time.Duration) {
	a := alarm.New(alarm.KindTimer)
	a.Duration = d
	a.FiresAt = time.Now().Add(d)
	u.store.Add(a)
	u.commit()
}

// soonest returns the alarm with the earliest upcoming trigger and that time,
// or (nil, zero) if nothing is scheduled.
func (u *UI) soonest() (*alarm.Alarm, time.Time) {
	var best *alarm.Alarm
	var bestAt time.Time
	for _, a := range u.store.List() {
		next := u.sched.NextFor(a)
		if next.IsZero() {
			continue
		}
		if best == nil || next.Before(bestAt) {
			best, bestAt = a, next
		}
	}
	return best, bestAt
}
