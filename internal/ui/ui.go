package ui

import (
	"fmt"
	"image/color"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"easy-alarms/internal/alarm"
	"easy-alarms/internal/astro"
	"easy-alarms/internal/audio"
	"easy-alarms/internal/humanize"
	"easy-alarms/internal/store"
)

type UI struct {
	app    fyne.App
	win    fyne.Window
	store  *store.Store
	sched  *alarm.Scheduler
	player *audio.Player

	TrayEnabled bool

	settings store.Settings

	list *fyne.Container
	rows []*row

	// header clock (main thread only), with last-rendered caches to skip
	// no-op refreshes — same pattern as the row countdowns.
	clock     *canvas.Text
	clockDate *canvas.Text
	bar       *dayBar
	riseLbl   *canvas.Text
	setLbl    *canvas.Text
	clockLast string
	dateLast  string
	riseLast  string
	setLast   string

	// visible tracks whether the main window is currently shown (as opposed
	// to minimized to tray). refreshLoop skips the header/list repaint work
	// while hidden, since nobody can see it and it would otherwise burn CPU
	// once a second forever in the background. Only ever touched on the
	// Fyne main thread (window callbacks and refreshLoop's fyne.Do), so it
	// needs no locking.
	visible bool

	// ring state (main thread only): open ring dialogs, whether the alarm
	// sound is playing, and the auto-silence cap timer.
	ringDialogs int
	soundOn     bool
	silence     *time.Timer
	// ringing maps an alarm ID to its open ring dialog, so the control API can
	// dismiss/snooze a ringing alarm programmatically. Main thread only.
	ringing map[string]dialog.Dialog

	// tray state, set in setupTray (nil when the tray is disabled)
	desk     desktop.App
	trayMenu *fyne.Menu
	trayNext *fyne.MenuItem
	trayLast string // last rendered "next alarm" label, to skip no-op refreshes
}

type row struct {
	alarm *alarm.Alarm
	when  *canvas.Text
	last  string // last rendered text, to skip no-op refreshes
}

func New(a fyne.App, st *store.Store, sched *alarm.Scheduler, player *audio.Player) *UI {
	return &UI{
		app:     a,
		store:   st,
		sched:   sched,
		player:  player,
		list:    container.NewVBox(),
		ringing: make(map[string]dialog.Dialog),
	}
}

func (u *UI) Run(hidden bool) {
	u.app.SetIcon(appIcon)
	u.win = u.app.NewWindow("Easy Alarms")
	u.win.SetCloseIntercept(u.hideMain) // close button minimizes to tray

	newAlarm := widget.NewButton("⏰  Nueva alarma", func() {
		u.showEditDialog(alarm.New(alarm.KindClock), true)
	})
	newAlarm.Importance = widget.HighImportance
	newTimer := widget.NewButton("⏱  Nuevo timer", func() {
		u.showEditDialog(alarm.New(alarm.KindTimer), true)
	})
	gear := widget.NewButtonWithIcon("", theme.SettingsIcon(), u.showSettings)
	toolbar := container.NewBorder(nil, nil, nil, gear,
		container.NewGridWithColumns(2, newAlarm, newTimer))

	u.settings = store.LoadSettings(store.Settings{
		Lat: astro.Madrid.Lat, Lon: astro.Madrid.Lon, ShowSeconds: true,
	})

	// Header secondary text (date+moon, sunrise/sunset) uses full foreground,
	// not the dimmed/disabled colour: on some monitors the dim grey is barely
	// legible. Hierarchy is carried by the clock's larger, bold size instead.
	fg := theme.Color(theme.ColorNameForeground)
	u.clock = rowText("", 40, true, fg)
	u.clock.Alignment = fyne.TextAlignCenter
	u.clockDate = rowText("", theme.TextSize(), false, fg)
	u.clockDate.Alignment = fyne.TextAlignCenter
	u.bar = newDayBar(u.location(), time.Now())
	u.riseLbl = rowText("", theme.TextSize(), false, fg)
	u.setLbl = rowText("", theme.TextSize(), false, fg)
	u.setLbl.Alignment = fyne.TextAlignTrailing
	sunRow := container.NewBorder(nil, nil, u.riseLbl, u.setLbl)
	u.updateClock(time.Now()) // paint once before Show, so it never flashes empty
	header := container.NewPadded(container.NewVBox(u.clock, u.clockDate, u.bar, sunRow))

	top := container.NewVBox(header, container.NewPadded(toolbar))
	u.win.SetContent(container.NewBorder(
		top, nil, nil, nil,
		container.NewVScroll(u.list),
	))
	// Resize AFTER SetContent (otherwise Fyne keeps the content's min size)
	// and center, so the window lands predictably on a multi-monitor desktop
	// instead of off on a secondary screen.
	u.win.Resize(fyne.NewSize(720, 560))
	u.win.CenterOnScreen()

	if u.TrayEnabled {
		u.setupTray()
	}
	u.rebuildList()
	go u.refreshLoop()

	if !hidden {
		u.showMain()
	}
	u.app.Run()
}

// showMain and hideMain wrap Window.Show/Hide to track visibility, so
// refreshLoop knows when it can skip repainting the header and list: nobody
// can see them while minimized to the tray, so doing that work every second
// forever in the background would just burn CPU for nothing.
func (u *UI) showMain() {
	u.visible = true
	u.updateClock(time.Now()) // catch up: the header was frozen while hidden
	for _, r := range u.rows {
		if next := u.rowWhen(r.alarm, time.Now()); next != r.last {
			r.last = next
			r.when.Text = next
			r.when.Refresh()
		}
	}
	u.win.Show()
}

func (u *UI) hideMain() {
	u.visible = false
	u.win.Hide()
}

// commit persists state, re-arms the scheduler and redraws the list. Call
// after any alarm mutation.
func (u *UI) commit() {
	if err := u.store.Save(); err != nil {
		dialog.ShowError(err, u.win)
	}
	u.sched.Reschedule()
	u.rebuildList()
	u.updateTrayNext()
}

func (u *UI) rebuildList() {
	u.list.Objects = nil
	u.rows = nil
	alarms := u.store.List()
	u.sortAlarms(alarms)
	if len(alarms) == 0 {
		empty := widget.NewLabel("🔕  Sin alarmas todavía.\nCrea una con los botones de arriba.")
		empty.Alignment = fyne.TextAlignCenter
		u.list.Add(container.NewPadded(container.NewCenter(empty)))
	}
	for _, a := range alarms {
		u.list.Add(u.newRow(a))
	}
	u.list.Refresh()
}

// sortAlarms orders the list: alarms that will actually ring (active) first,
// then the rest; within each group, the soonest on top. Inactive alarms are
// ordered by the time they *would* fire if running, so the ordering stays
// intuitive ("by time of day").
func (u *UI) sortAlarms(alarms []*alarm.Alarm) {
	now := time.Now()
	group := func(a *alarm.Alarm) (int, time.Time) {
		if next := u.sched.NextFor(a); !next.IsZero() {
			return 0, next
		}
		switch a.Kind {
		case alarm.KindTimer:
			d := a.Duration
			if a.Remaining > 0 {
				d = a.Remaining
			}
			return 1, now.Add(d)
		default:
			c := time.Date(now.Year(), now.Month(), now.Day(), a.Hour, a.Minute, 0, 0, now.Location())
			if !c.After(now) {
				c = c.AddDate(0, 0, 1)
			}
			return 1, c
		}
	}
	sort.SliceStable(alarms, func(i, j int) bool {
		gi, ti := group(alarms[i])
		gj, tj := group(alarms[j])
		if gi != gj {
			return gi < gj
		}
		return ti.Before(tj)
	})
}

func (u *UI) newRow(a *alarm.Alarm) fyne.CanvasObject {
	// Inactive alarms (won't actually ring) are dimmed.
	active := !u.sched.NextFor(a).IsZero()
	fg := theme.Color(theme.ColorNameForeground)
	if !active {
		fg = theme.Color(theme.ColorNameDisabled)
	}

	title := rowText(rowTitle(a), 17, true, fg)

	whenText := u.rowWhen(a, time.Now())
	when := rowText(whenText, theme.TextSize(), false, fg)
	u.rows = append(u.rows, &row{alarm: a, when: when, last: whenText})

	// The sound is deliberately not shown here: it's secondary info that made
	// every row taller, and the edit dialog already displays it.
	left := container.NewVBox(title, when)
	if a.Kind == alarm.KindClock {
		if rep := repeatLabel(a.Repeat); rep != "" {
			left.Add(rowText("📅  "+rep, theme.TextSize(), false, fg))
		}
	}

	dup := widget.NewButtonWithIcon("", theme.ContentCopyIcon(), func() {
		c := alarm.New(a.Kind)
		c.Label, c.Hour, c.Minute, c.Repeat = a.Label, a.Hour, a.Minute, a.Repeat
		c.Duration, c.Sound = a.Duration, a.Sound
		u.showEditDialog(c, true) // cancelling the editor discards the copy
	})
	edit := widget.NewButtonWithIcon("", theme.DocumentCreateIcon(), func() {
		u.showEditDialog(a, false)
	})
	del := widget.NewButtonWithIcon("", theme.DeleteIcon(), func() {
		dialog.ShowConfirm("Eliminar", fmt.Sprintf("¿Eliminar %q?", rowTitle(a)), func(ok bool) {
			if !ok {
				return
			}
			u.store.Remove(a.ID)
			u.commit()
		}, u.win)
	})

	actions := container.NewHBox(u.stateControl(a), dup, edit, del)

	// Double-clicking the info area also opens the editor.
	info := newTappable(left, func() { u.showEditDialog(a, false) })
	body := container.NewBorder(nil, nil, nil, actions, info)
	return widget.NewCard("", "", container.NewPadded(body))
}

func rowText(s string, size float32, bold bool, col color.Color) *canvas.Text {
	t := canvas.NewText(s, col)
	t.TextSize = size
	t.TextStyle = fyne.TextStyle{Bold: bold}
	return t
}

// stateControl is the per-row enable toggle (alarms) or the
// start/pause/resume/stop buttons (timers).
func (u *UI) stateControl(a *alarm.Alarm) fyne.CanvasObject {
	switch a.Kind {
	case alarm.KindTimer:
		stop := widget.NewButtonWithIcon("", theme.MediaStopIcon(), func() {
			a.FiresAt = time.Time{}
			a.Remaining = 0
			u.sched.ClearSnooze(a.ID)
			u.commit()
		})
		switch {
		case !a.FiresAt.IsZero(): // running
			pause := widget.NewButtonWithIcon("", theme.MediaPauseIcon(), func() {
				if left := time.Until(a.FiresAt); left > 0 {
					a.Remaining = left
				}
				a.FiresAt = time.Time{}
				u.commit()
			})
			return container.NewHBox(pause, stop)
		case a.Remaining > 0: // paused
			resume := widget.NewButtonWithIcon("", theme.MediaPlayIcon(), func() {
				a.Enabled = true
				a.FiresAt = time.Now().Add(a.Remaining)
				a.Remaining = 0
				u.commit()
			})
			return container.NewHBox(resume, stop)
		default: // idle
			return widget.NewButtonWithIcon("", theme.MediaPlayIcon(), func() {
				a.Enabled = true
				a.FiresAt = time.Now().Add(a.Duration)
				u.commit()
			})
		}
	default:
		// Set OnChanged AFTER SetChecked: in Fyne SetChecked fires OnChanged,
		// and commit() rebuilds the list, which would recurse infinitely and
		// hang the UI thread. The guard is belt-and-braces.
		check := widget.NewCheck("", nil)
		check.SetChecked(a.Enabled)
		check.OnChanged = func(on bool) {
			if on == a.Enabled {
				return
			}
			a.Enabled = on
			if !on {
				u.sched.ClearSnooze(a.ID) // disabling also silences a pending snooze
			}
			u.commit()
		}
		return check
	}
}

// rowWhen renders a row's status line: the live countdown, or the paused
// state, which has no next trigger but should still show the time left.
func (u *UI) rowWhen(a *alarm.Alarm, now time.Time) string {
	if a.Kind == alarm.KindTimer && a.FiresAt.IsZero() && a.Remaining > 0 {
		if u.sched.NextFor(a).IsZero() { // not snoozed
			return "⏸ En pausa (quedan " + humanize.Compact(a.Remaining) + ")"
		}
	}
	return describeNext(u.sched.NextFor(a), now)
}

func rowTitle(a *alarm.Alarm) string {
	label := a.Label
	if label == "" {
		label = map[alarm.Kind]string{alarm.KindClock: "Alarma", alarm.KindTimer: "Timer"}[a.Kind]
	}
	switch a.Kind {
	case alarm.KindTimer:
		return fmt.Sprintf("⏱  %s · %s", label, humanize.Compact(a.Duration))
	default:
		// No time here: the "when" line below already shows it.
		return "⏰  " + label
	}
}

// updateClock refreshes the header clock, date, moon phase, day bar and
// sunrise/sunset labels, touching each canvas only when its text/state
// actually changed.
func (u *UI) updateClock(now time.Time) {
	layout := "15:04"
	if u.settings.ShowSeconds {
		layout = "15:04:05"
	}
	if t := now.Format(layout); t != u.clockLast {
		u.clockLast = t
		u.clock.Text = t
		u.clock.Refresh()
	}
	moon := astro.MoonPhase(now)
	if d := fmt.Sprintf("%s   %s %.0f%%", longDate(now), moon.Emoji, moon.Illumination*100); d != u.dateLast {
		u.dateLast = d
		u.clockDate.Text = d
		u.clockDate.Refresh()
	}
	u.bar.tick(now)
	rise, set := "—", "—"
	if u.bar.haveSun {
		rise = "☀ " + u.bar.sunrise.Format("15:04")
		set = u.bar.sunset.Format("15:04") + " 🌙"
	}
	if rise != u.riseLast {
		u.riseLast = rise
		u.riseLbl.Text = rise
		u.riseLbl.Refresh()
	}
	if set != u.setLast {
		u.setLast = set
		u.setLbl.Text = set
		u.setLbl.Refresh()
	}
}

// location is the configured location for sun times.
func (u *UI) location() astro.Location {
	return astro.Location{Lat: u.settings.Lat, Lon: u.settings.Lon}
}

// showSettings opens the preferences dialog: location (for sunrise/sunset)
// and whether the clock shows seconds. Changes are persisted and applied live.
func (u *UI) showSettings() {
	lat := widget.NewEntry()
	lat.SetText(strconv.FormatFloat(u.settings.Lat, 'f', -1, 64))
	lon := widget.NewEntry()
	lon.SetText(strconv.FormatFloat(u.settings.Lon, 'f', -1, 64))

	coord := func(name string, min, max float64) fyne.StringValidator {
		return func(s string) error {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return fmt.Errorf("%s no es un número válido", name)
			}
			if v < min || v > max {
				return fmt.Errorf("%s fuera de rango (%g a %g)", name, min, max)
			}
			return nil
		}
	}
	lat.Validator = coord("La latitud", -90, 90)
	lon.Validator = coord("La longitud", -180, 180)

	seconds := widget.NewCheck("", nil)
	seconds.SetChecked(u.settings.ShowSeconds)

	form := dialog.NewForm("Ajustes", "Guardar", "Cancelar", []*widget.FormItem{
		{Text: "Latitud", Widget: lat, HintText: "Norte positivo (Madrid ≈ 40.42)"},
		{Text: "Longitud", Widget: lon, HintText: "Este positivo (Madrid ≈ -3.70)"},
		{Text: "Mostrar segundos", Widget: seconds},
	}, func(ok bool) {
		if !ok {
			return
		}
		u.settings.Lat, _ = strconv.ParseFloat(strings.TrimSpace(lat.Text), 64)
		u.settings.Lon, _ = strconv.ParseFloat(strings.TrimSpace(lon.Text), 64)
		u.settings.ShowSeconds = seconds.Checked
		if err := store.SaveSettings(u.settings); err != nil {
			dialog.ShowError(err, u.win)
		}
		u.applySettings()
	}, u.win)
	form.Resize(fyne.NewSize(420, 260))
	form.Show()
}

// applySettings re-renders the header after a settings change: relocates the
// sun, and forces a full clock repaint (the second/no-second format may have
// flipped) by clearing the render caches.
func (u *UI) applySettings() {
	now := time.Now()
	u.bar.setLocation(u.location(), now)
	u.clockLast, u.dateLast, u.riseLast, u.setLast = "", "", "", ""
	u.updateClock(now)
}

// refreshLoop keeps the header clock and the "rings in..." labels ticking.
// It only touches labels whose text actually changed, so an idle window does
// no rendering work, and skips the header/list work entirely while the
// window is hidden (minimized to tray): nobody can see it, so recomputing
// the clock, moon phase and every row's countdown once a second would just
// waste CPU in the background, forever. The tray label is cheap and stays
// live either way, since that's the whole point of the tray icon.
func (u *UI) refreshLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		fyne.Do(func() {
			now := time.Now()
			if u.visible {
				u.updateClock(now)
				for _, r := range u.rows {
					next := u.rowWhen(r.alarm, now)
					if next != r.last {
						r.last = next
						r.when.Text = next
						r.when.Refresh()
					}
				}
			}
			u.updateTrayNext()
		})
	}
}
