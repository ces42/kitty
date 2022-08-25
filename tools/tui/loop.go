// License: GPLv3 Copyright: 2022, Kovid Goyal, <kovid at kovidgoyal.net>

package tui

import (
	"bytes"
	"fmt"
	"io"
	"kitty/tools/tty"
	"os"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"kitty/tools/utils"
	"kitty/tools/wcswidth"
)

func read_ignoring_temporary_errors(fd int, buf []byte) (int, error) {
	n, err := unix.Read(fd, buf)
	if err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
		return 0, nil
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, err
}

func write_ignoring_temporary_errors(fd int, buf []byte) (int, error) {
	n, err := unix.Write(fd, buf)
	if err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
		return 0, nil
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, err
}

type ScreenSize struct {
	WidthCells, HeightCells, WidthPx, HeightPx, CellWidth, CellHeight uint
	updated                                                           bool
}

type TimerId uint64
type TimerCallback func(loop *Loop, timer_id TimerId) error

type timer struct {
	interval time.Duration
	deadline time.Time
	repeats  bool
	id       TimerId
	callback TimerCallback
}

func (self *timer) update_deadline(now time.Time) {
	self.deadline = now.Add(self.interval)
}

type Loop struct {
	controlling_term   *tty.Term
	terminal_options   TerminalStateOptions
	screen_size        ScreenSize
	escape_code_parser wcswidth.EscapeCodeParser
	keep_going         bool
	flush_write_buf    bool
	death_signal       Signal
	exit_code          int
	write_buf          []byte
	timers             []*timer
	timer_id_counter   TimerId

	// Callbacks

	// Called when the terminal has been fully setup. Any string returned is sent to
	// the terminal on shutdown
	OnInitialize func(loop *Loop) (string, error)

	// Called when a key event happens
	OnKeyEvent func(loop *Loop, event *KeyEvent) error

	// Called when text is received either from a key event or directly from the terminal
	OnText func(loop *Loop, text string, from_key_event bool, in_bracketed_paste bool) error

	// Called when the terminal is resize
	OnResize func(loop *Loop, old_size ScreenSize, new_size ScreenSize) error

	// Called when writing is done
	OnWriteComplete func(loop *Loop) error

	// Called when a response to an rc command is received
	OnRCResponse func(loop *Loop, data []byte) error

	// Called when any input form tty is received
	OnReceivedData func(loop *Loop, data []byte) error
}

func (self *Loop) update_screen_size() error {
	if self.controlling_term != nil {
		return fmt.Errorf("No controlling terminal cannot update screen size")
	}
	ws, err := self.controlling_term.GetSize()
	if err != nil {
		return err
	}
	s := &self.screen_size
	s.updated = true
	s.HeightCells, s.WidthCells = uint(ws.Row), uint(ws.Col)
	s.HeightPx, s.WidthPx = uint(ws.Ypixel), uint(ws.Xpixel)
	s.CellWidth = s.WidthPx / s.WidthCells
	s.CellHeight = s.HeightPx / s.HeightCells
	return nil
}

func (self *Loop) handle_csi(raw []byte) error {
	csi := string(raw)
	ke := KeyEventFromCSI(csi)
	if ke != nil {
		return self.handle_key_event(ke)
	}
	return nil
}

func (self *Loop) handle_key_event(ev *KeyEvent) error {
	// self.DebugPrintln(ev)
	if self.OnKeyEvent != nil {
		err := self.OnKeyEvent(self, ev)
		if err != nil {
			return err
		}
		if ev.Handled {
			return nil
		}
	}
	if ev.MatchesPressOrRepeat("ctrl+c") {
		ev.Handled = true
		return self.on_SIGINT()
	}
	if ev.MatchesPressOrRepeat("ctrl+z") {
		ev.Handled = true
		return self.on_SIGTSTP()
	}
	if ev.Text != "" && self.OnText != nil {
		return self.OnText(self, ev.Text, true, false)
	}
	return nil
}

func (self *Loop) handle_osc(raw []byte) error {
	return nil
}

func (self *Loop) handle_dcs(raw []byte) error {
	if self.OnRCResponse != nil && bytes.HasPrefix(raw, []byte("@kitty-cmd")) {
		return self.OnRCResponse(self, raw[len("@kitty-cmd"):])
	}
	return nil
}

func (self *Loop) handle_apc(raw []byte) error {
	return nil
}

func (self *Loop) handle_sos(raw []byte) error {
	return nil
}

func (self *Loop) handle_pm(raw []byte) error {
	return nil
}

func (self *Loop) handle_rune(raw rune) error {
	if self.OnText != nil {
		return self.OnText(self, string(raw), false, self.escape_code_parser.InBracketedPaste())
	}
	return nil
}

func (self *Loop) on_SIGINT() error {
	self.death_signal = SIGINT
	self.keep_going = false
	return nil
}

func (self *Loop) on_SIGPIPE() error {
	return nil
}

func (self *Loop) on_SIGWINCH() error {
	self.screen_size.updated = false
	if self.OnResize != nil {
		old_size := self.screen_size
		err := self.update_screen_size()
		if err != nil {
			return err
		}
		return self.OnResize(self, old_size, self.screen_size)
	}
	return nil
}

func (self *Loop) on_SIGTERM() error {
	self.death_signal = SIGTERM
	self.keep_going = false
	return nil
}

func (self *Loop) on_SIGTSTP() error {
	return nil
}

func (self *Loop) on_SIGHUP() error {
	self.flush_write_buf = false
	self.death_signal = SIGHUP
	self.keep_going = false
	return nil
}

func CreateLoop() (*Loop, error) {
	l := Loop{controlling_term: nil, timers: make([]*timer, 0)}
	l.terminal_options.alternate_screen = true
	l.escape_code_parser.HandleCSI = l.handle_csi
	l.escape_code_parser.HandleOSC = l.handle_osc
	l.escape_code_parser.HandleDCS = l.handle_dcs
	l.escape_code_parser.HandleAPC = l.handle_apc
	l.escape_code_parser.HandleSOS = l.handle_sos
	l.escape_code_parser.HandlePM = l.handle_pm
	l.escape_code_parser.HandleRune = l.handle_rune
	return &l, nil
}

func (self *Loop) AddTimer(interval time.Duration, repeats bool, callback TimerCallback) TimerId {
	self.timer_id_counter++
	t := timer{interval: interval, repeats: repeats, callback: callback, id: self.timer_id_counter}
	t.update_deadline(time.Now())
	self.timers = append(self.timers, &t)
	self.sort_timers()
	return t.id
}

func (self *Loop) RemoveTimer(id TimerId) bool {
	for i := 0; i < len(self.timers); i++ {
		if self.timers[i].id == id {
			self.timers = append(self.timers[:i], self.timers[i+1:]...)
			return true
		}
	}
	return false
}

func (self *Loop) NoAlternateScreen() {
	self.terminal_options.alternate_screen = false
}

func (self *Loop) MouseTracking(mt MouseTracking) {
	self.terminal_options.mouse_tracking = mt
}

func (self *Loop) DeathSignalName() string {
	if self.death_signal != SIGNULL {
		return self.death_signal.String()
	}
	return ""
}

func (self *Loop) ScreenSize() (ScreenSize, error) {
	if self.screen_size.updated {
		return self.screen_size, nil
	}
	err := self.update_screen_size()
	return self.screen_size, err
}

func kill_self(sig unix.Signal) {
	unix.Kill(os.Getpid(), sig)
	// Give the signal time to be delivered
	time.Sleep(20 * time.Millisecond)
}

func (self *Loop) KillIfSignalled() {
	switch self.death_signal {
	case SIGINT:
		kill_self(unix.SIGINT)
	case SIGTERM:
		kill_self(unix.SIGTERM)
	case SIGHUP:
		kill_self(unix.SIGHUP)
	}
}

func (self *Loop) DebugPrintln(args ...interface{}) {
	if self.controlling_term != nil {
		self.controlling_term.DebugPrintln(args...)
	}
}

func (self *Loop) Run() (err error) {
	signal_read_file, signal_write_file, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() {
		signal_read_file.Close()
		signal_write_file.Close()
	}()

	sigchnl := make(chan os.Signal, 256)
	reset_signals := notify_signals(sigchnl, SIGINT, SIGTERM, SIGTSTP, SIGHUP, SIGWINCH, SIGPIPE)
	defer reset_signals()

	go func() {
		for {
			s := <-sigchnl
			if write_signal(signal_write_file, s) != nil {
				break
			}
		}
	}()

	controlling_term, err := tty.OpenControllingTerm()
	if err != nil {
		return err
	}
	tty_fd := controlling_term.Fd()
	self.controlling_term = controlling_term
	defer func() {
		self.controlling_term.RestoreAndClose()
		self.controlling_term = nil
	}()
	err = self.controlling_term.ApplyOperations(tty.TCSANOW, tty.SetRaw)
	if err != nil {
		return nil
	}

	selector := CreateSelect(8)
	selector.RegisterRead(int(signal_read_file.Fd()))
	selector.RegisterRead(tty_fd)

	self.keep_going = true
	self.flush_write_buf = true
	self.queue_write_to_tty(self.terminal_options.SetStateEscapeCodes())
	finalizer := ""
	if self.OnInitialize != nil {
		finalizer, err = self.OnInitialize(self)
		if err != nil {
			return err
		}
	}

	defer func() {
		if self.flush_write_buf {
			self.flush()
		}
		self.write_buf = self.write_buf[:0]
		if finalizer != "" {
			self.queue_write_to_tty([]byte(finalizer))
		}
		self.queue_write_to_tty(self.terminal_options.ResetStateEscapeCodes())
		self.flush()
	}()

	read_buf := make([]byte, utils.DEFAULT_IO_BUFFER_SIZE)
	signal_buf := make([]byte, 256)
	self.death_signal = SIGNULL
	self.escape_code_parser.Reset()
	self.exit_code = 0
	num_ready := 0
	for self.keep_going {
		if len(self.write_buf) > 0 {
			selector.RegisterWrite(tty_fd)
		} else {
			selector.UnRegisterWrite(tty_fd)
		}
		if len(self.timers) > 0 {
			now := time.Now()
			err = self.dispatch_timers(now)
			if err != nil {
				return err
			}
			timeout := self.timers[0].deadline.Sub(now)
			if timeout < 0 {
				timeout = 0
			}
			num_ready, err = selector.Wait(timeout)
		} else {
			num_ready, err = selector.WaitForever()
			if err != nil {
				return fmt.Errorf("Failed to call select() with error: %w", err)
			}
		}
		if num_ready == 0 {
			continue
		}
		if len(self.write_buf) > 0 && selector.IsReadyToWrite(tty_fd) {
			err = self.write_to_tty()
			if err != nil {
				return err
			}
			if self.OnWriteComplete != nil && len(self.write_buf) == 0 {
				err = self.OnWriteComplete(self)
				if err != nil {
					return err
				}
			}
		}
		if selector.IsReadyToRead(tty_fd) {
			read_buf = read_buf[:cap(read_buf)]
			num_read, err := read_ignoring_temporary_errors(tty_fd, read_buf)
			if err != nil {
				return err
			}
			if num_read > 0 {
				if self.OnReceivedData != nil {
					err = self.OnReceivedData(self, read_buf[:num_read])
					if err != nil {
						return err
					}
				}
				err = self.escape_code_parser.Parse(read_buf[:num_read])
				if err != nil {
					return err
				}
			}
		}
		if selector.IsReadyToRead(int(signal_read_file.Fd())) {
			signal_buf = signal_buf[:cap(signal_buf)]
			err = self.read_signals(signal_read_file, signal_buf)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (self *Loop) queue_write_to_tty(data []byte) {
	self.write_buf = append(self.write_buf, data...)
}

func (self *Loop) QueueWriteString(data string) {
	self.queue_write_to_tty([]byte(data))
}

func (self *Loop) QueueWriteBytes(data []byte) {
	self.queue_write_to_tty(data)
}

func (self *Loop) ExitCode() int {
	return self.exit_code
}

func (self *Loop) Beep() {
	self.QueueWriteString("\a")
}

func (self *Loop) Quit(exit_code int) {
	self.exit_code = exit_code
	self.keep_going = false
}

func (self *Loop) write_to_tty() error {
	if len(self.write_buf) == 0 || self.controlling_term == nil {
		return nil
	}
	n, err := write_ignoring_temporary_errors(self.controlling_term.Fd(), self.write_buf)
	if err != nil {
		return err
	}
	if n <= 0 {
		return nil
	}
	remainder := self.write_buf[n:]
	if len(remainder) > 0 {
		self.write_buf = self.write_buf[:len(remainder)]
		copy(self.write_buf, remainder)
	} else {
		self.write_buf = self.write_buf[:0]
	}
	return nil
}

func (self *Loop) flush() error {
	if self.controlling_term == nil {
		return nil
	}
	selector := CreateSelect(1)
	selector.RegisterWrite(self.controlling_term.Fd())
	deadline := time.Now().Add(2 * time.Second)
	for len(self.write_buf) > 0 {
		timeout := deadline.Sub(time.Now())
		if timeout < 0 {
			break
		}
		num_ready, err := selector.Wait(timeout)
		if err != nil {
			return err
		}
		if num_ready > 0 && selector.IsReadyToWrite(self.controlling_term.Fd()) {
			err = self.write_to_tty()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (self *Loop) dispatch_timers(now time.Time) error {
	updated := false
	remove := make(map[TimerId]bool, 0)
	for _, t := range self.timers {
		if now.After(t.deadline) {
			err := t.callback(self, t.id)
			if err != nil {
				return err
			}
			if t.repeats {
				t.update_deadline(now)
				updated = true
			} else {
				remove[t.id] = true
			}
		}
	}
	if len(remove) > 0 {
		timers := make([]*timer, len(self.timers)-len(remove))
		for _, t := range self.timers {
			if !remove[t.id] {
				timers = append(timers, t)
			}
		}
		self.timers = timers
	}
	if updated {
		self.sort_timers()
	}
	return nil
}

func (self *Loop) sort_timers() {
	sort.SliceStable(self.timers, func(a, b int) bool { return self.timers[a].deadline.Before(self.timers[b].deadline) })
}