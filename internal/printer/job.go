package printer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrDisconnected = errors.New("printer not connected")
	ErrJobActive    = errors.New("a job is already active")
	ErrNoJob        = errors.New("no active job")
	ErrNotPrinting  = errors.New("the job is not printing")
	// ErrInvalidMode is returned (wrapped) by Upload for an unknown mode.
	ErrInvalidMode = errors.New("unknown upload mode")
)

// sdStartGrace is how long an SD job may report "Not SD printing" after
// M23/M24 before forge gives up on it (M27 S2 reports every 2s).
const sdStartGrace = 30 * time.Second

// cancelFlushTimeout bounds how long Cancel waits for Marlin to confirm an
// SD abort (M524) before it would park; M27 S2 reports every 2s.
const cancelFlushTimeout = 10 * time.Second

// autoReportPeriod is one M27 S2 period plus margin. A "Not SD printing"
// cached before M524 went out only counts once this much time has passed.
const autoReportPeriod = 2500 * time.Millisecond

// emergencyUploadWait bounds how long Emergency waits for an SD upload to
// close the file (abortUpload waits up to 15s for its M29) before M112.
const emergencyUploadWait = 20 * time.Second

// eventQueue is the per-driver event buffer. Events are rare; if it is
// ever full the event is dropped and logged rather than block the reader.
const eventQueue = 16

type driverEvent struct {
	ev   Event
	line string
}

// lcdJobName labels an SD print forge did not start (LCD or console).
const lcdJobName = "(started on printer)"

// Manager tracks the current job (upload/print/stream) on top of whichever
// Driver the Link currently has attached, and is the type Server talks to.
// It serializes job control so only one upload/print/stream runs at a time.
//
// Lock order: seq before mu. Nothing holding mu calls into a Driver method
// that can block, and nothing holding mu takes seq.
type Manager struct {
	Names *NameMap

	seq sync.Mutex // one command sequence at a time (SEC-8)

	mu        sync.Mutex
	drv       *Driver // nil while disconnected
	job       *Job
	cancel    context.CancelFunc
	worker    chan struct{} // closed when the upload/stream goroutine exits
	resume    chan struct{} // non-nil while a stream job is paused
	state     State
	keptState State // state of an SD job kept across a link loss
	lastErr   string
	sdSeen    bool // M27 reported "SD printing" for the current SD job
	sdIgnore  bool // a stale "SD printing" may linger; wait for "Not SD printing"
	reconcile bool // SD job kept across a link loss; waiting for the first M27
}

func NewManager(names *NameMap) *Manager {
	return &Manager{Names: names, state: StateDisconnected}
}

// Attach makes d the current driver. The Link calls it after Handshake and
// before d.Start so no event is missed.
func (m *Manager) Attach(d *Driver) {
	// One goroutine per driver applies its events in arrival order (R-24).
	evs := make(chan driverEvent, eventQueue)
	d.OnEvent(func(ev Event, line string) {
		// Called on the driver's read goroutine: never block it.
		select {
		case evs <- driverEvent{ev, line}:
		default:
			log.Printf("forge: printer event queue full, dropped event %d: %s", ev, line)
		}
	})
	go func() {
		for {
			select {
			case e := <-evs:
				m.handleEvent(d, e.ev, e.line)
			case <-d.Done():
				return
			}
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drv = d
	m.sdIgnore = false
	switch {
	case m.job != nil && m.reconcile:
		// Wait for the first M27 report to say whether it still runs.
		m.state = m.keptState
	case m.job == nil:
		m.state = StateIdle
	}
}

// Detach drops the current driver after the link failed. Transfers that
// need the link are aborted; an SD print is kept because the printer runs
// it on its own and may still be printing.
func (m *Manager) Detach(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drv = nil
	job := m.job
	switch {
	case job == nil:
		m.state = StateDisconnected
	case job.Mode == ModeSDUpload && (m.state == StatePrinting || m.state == StatePaused):
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.keptState = m.state
		m.reconcile = true
		m.sdSeen = false
		job.Note = "printer link lost; the SD print may still be running"
		m.state = StateDisconnected
	case m.state == StateUploading:
		m.failLocked("connection lost during upload: " + reason)
	default:
		m.failLocked("connection lost during print, stream aborted: " + reason)
	}
}

// Connected reports whether a driver is attached.
func (m *Manager) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drv != nil
}

func (m *Manager) driver() *Driver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drv
}

func (m *Manager) State() State {
	m.sync()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Snapshot builds the current point-in-time view for the JSON API.
func (m *Manager) Snapshot() Snapshot {
	m.sync()
	m.mu.Lock()
	drv := m.drv
	state := m.state
	lastErr := m.lastErr
	var job *Job
	if m.job != nil {
		j := *m.job
		job = &j
	}
	m.mu.Unlock()

	if job != nil {
		job.ElapsedSec = time.Since(job.StartedAt).Seconds()
		if job.Progress > 0 && job.Progress < 100 && (state == StatePrinting || state == StateUploading) {
			job.ETASec = job.ElapsedSec/job.Progress*100 - job.ElapsedSec
		}
	}
	snap := Snapshot{State: state, Connected: drv != nil, Job: job, LastError: lastErr}
	if drv != nil {
		snap.Temps = drv.Temps()
		for k, v := range drv.Capabilities() {
			if v {
				snap.Capabilities = append(snap.Capabilities, k)
			}
		}
		sort.Strings(snap.Capabilities)
	}
	return snap
}

// sync folds the driver's cached M27 status into the job state. It runs
// lazily from Snapshot and the job-control calls; the Driver already caches
// the M27 S2 autoreport, so this costs no serial traffic.
func (m *Manager) sync() {
	drv := m.driver()
	if drv == nil {
		return
	}
	st := drv.SDStatus()
	m.mu.Lock()
	var lcd *Job
	if m.drv == drv {
		lcd = m.refreshLocked(st)
	}
	m.mu.Unlock()
	if lcd == nil || st.Total <= 0 {
		return
	}
	// Name the print if exactly one card file has the reported size.
	name := ""
	for _, f := range drv.SDFiles() {
		if f.Bytes == st.Total {
			if name != "" {
				return
			}
			name = f.Long
			if name == "" {
				name = f.Short
			}
		}
	}
	if name != "" {
		m.mu.Lock()
		if m.job == lcd {
			lcd.Filename = name
		}
		m.mu.Unlock()
	}
}

// refreshLocked applies one M27 status. It returns the job it created when
// it found an SD print forge did not start.
func (m *Manager) refreshLocked(st SDStatus) *Job {
	if m.job == nil {
		if st.NotSD {
			m.sdIgnore = false
			return nil
		}
		// StateError too: an error must not hide a print started on the
		// printer, or the next upload's M28 would abort it (R-09).
		if !st.Printing || m.sdIgnore || m.worker != nil || (m.state != StateIdle && m.state != StateError) {
			return nil
		}
		j := &Job{Filename: lcdJobName, Mode: ModeSDUpload, StartedAt: time.Now()}
		setSDProgress(j, st)
		m.startJobLocked(j, nil)
		m.sdSeen = true
		m.state = StatePrinting
		return j
	}
	j := m.job
	if j.Mode != ModeSDUpload || (m.state != StatePrinting && m.state != StatePaused) {
		return nil
	}
	switch {
	case st.Printing:
		m.sdSeen = true
		if m.reconcile {
			m.reconcile = false
			j.Note = ""
		}
		setSDProgress(j, st)
	case st.NotSD:
		switch {
		case m.reconcile:
			m.endJobLocked(StateIdle, "the SD print was no longer running after the printer reconnected")
		case m.sdSeen:
			m.endJobLocked(StateIdle, "")
		case time.Since(j.StartedAt) > sdStartGrace:
			m.failLocked(fmt.Sprintf("SD print of %s did not start", j.Filename))
		}
	}
	return nil
}

func setSDProgress(j *Job, st SDStatus) {
	j.SentBytes = st.Current
	j.TotalBytes = st.Total
	if st.Total > 0 {
		j.Progress = float64(st.Current) / float64(st.Total) * 100
	}
}

func (m *Manager) handleEvent(d *Driver, ev Event, line string) {
	line = strings.TrimSpace(line)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drv != d {
		return // late event from a driver that is gone
	}
	if ev == EventPrinterReset && m.job != nil && m.job.Mode == ModeStream {
		m.streamResetLocked(d)
		return
	}
	sdPrinting := m.job != nil && m.job.Mode == ModeSDUpload &&
		(m.state == StatePrinting || m.state == StatePaused)
	switch ev {
	case EventDonePrinting:
		if sdPrinting {
			m.endJobLocked(StateIdle, "")
		}
		m.sdIgnore = true
	case EventOpenFailed:
		if sdPrinting {
			m.failLocked(fmt.Sprintf("printer could not open %s on the SD card (%s)", m.job.Filename, line))
		} else {
			m.lastErr = "printer: " + line
		}
	case EventHalted:
		if m.job != nil {
			m.failLocked("printer halted, job aborted: " + line)
		} else {
			m.state = StateError
			m.lastErr = "printer halted: " + line
		}
	case EventPrinterReset:
		if m.job != nil {
			m.failLocked("printer reset unexpectedly, job aborted")
		} else if m.state == StateError {
			m.state = StateIdle
		}
	}
}

// streamResetLocked fails a stream job after the printer rebooted. The
// Driver does not fail SendCtx callers on a reset, so the feeder is stopped
// here by cancelling its context. A line may still have slipped through
// between the reset and this cancel (R-07), so once the feeder has exited
// the heaters and fan are switched off. No park: the printer is unhomed.
func (m *Manager) streamResetLocked(d *Driver) {
	wait := m.worker
	m.failLocked("printer reset unexpectedly during the stream; stream stopped, heaters switched off")
	go func() {
		if wait != nil {
			t := time.NewTimer(10 * time.Second)
			select {
			case <-wait:
			case <-t.C:
			}
			t.Stop()
		}
		m.seq.Lock()
		defer m.seq.Unlock()
		if m.driver() != d {
			return
		}
		if err := cooldown(d); err != nil {
			log.Printf("forge: cooldown after printer reset: %v", err)
		}
	}()
}

// startJobLocked publishes a new job with a clean slate.
func (m *Manager) startJobLocked(j *Job, cancel context.CancelFunc) {
	m.job = j
	m.cancel = cancel
	m.resume = nil
	m.lastErr = ""
	m.sdSeen = false
	m.reconcile = false
}

// endJobLocked clears the job, always cancelling its context and waking a
// paused feeder so no goroutine is left behind (SEC-13).
func (m *Manager) endJobLocked(s State, errMsg string) {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.resume != nil {
		close(m.resume)
		m.resume = nil
	}
	if m.job != nil && m.job.Mode == ModeSDUpload {
		// The last M27 may still say "SD printing" for up to 2s.
		m.sdIgnore = true
	}
	m.job = nil
	m.sdSeen = false
	m.reconcile = false
	m.state = s
	if errMsg != "" {
		m.lastErr = errMsg
	}
}

func (m *Manager) failLocked(msg string) {
	log.Printf("forge: job error: %s", msg)
	m.endJobLocked(StateError, msg)
}

// failJob ends job with an error if it is still the current job. It
// reports whether it did, so a job already ended by Cancel/Detach stays
// quiet.
func (m *Manager) failJob(job *Job, msg string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != job {
		return false
	}
	m.failLocked(msg)
	return true
}

func (m *Manager) finishJob(job *Job) {
	m.mu.Lock()
	if m.job == job {
		m.endJobLocked(StateIdle, "")
	}
	m.mu.Unlock()
}

// Upload starts an upload (sd) or stream (stream) of localPath and returns
// as soon as it is validated; the transfer runs in its own goroutine and
// its outcome shows up in Snapshot. Upload owns localPath from here on and
// removes it in every case.
func (m *Manager) Upload(localPath, longName string, mode UploadMode, startPrint bool) error {
	if mode != ModeSDUpload && mode != ModeStream {
		os.Remove(localPath)
		return fmt.Errorf("%w %q", ErrInvalidMode, mode)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		os.Remove(localPath)
		return err
	}
	m.sync()
	m.mu.Lock()
	drv := m.drv
	switch {
	case drv == nil:
		err = ErrDisconnected
	case m.job != nil || m.worker != nil:
		err = ErrJobActive
	}
	if err != nil {
		m.mu.Unlock()
		os.Remove(localPath)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{Filename: longName, Mode: mode, TotalBytes: info.Size(), StartedAt: time.Now()}
	m.startJobLocked(job, cancel)
	done := make(chan struct{})
	m.worker = done
	if mode == ModeStream {
		m.state = StatePrinting
	} else {
		m.state = StateUploading
	}
	m.mu.Unlock()

	go func() {
		defer m.workerExit(done, localPath)
		if mode == ModeStream {
			m.runStream(ctx, drv, job, localPath)
		} else {
			m.runSD(ctx, drv, job, localPath, longName, startPrint)
		}
	}()
	return nil
}

func (m *Manager) workerExit(done chan struct{}, localPath string) {
	os.Remove(localPath) // /tmp is precious on this box; always clean up
	m.mu.Lock()
	if m.worker == done {
		m.worker = nil
	}
	m.mu.Unlock()
	close(done)
}

func (m *Manager) runSD(ctx context.Context, drv *Driver, job *Job, localPath, longName string, startPrint bool) {
	short, err := drv.UploadToSD(ctx, localPath, longName, m.Names, func(sent, total int64) {
		m.mu.Lock()
		if m.job == job {
			job.SentBytes = sent
			job.TotalBytes = total
			if total > 0 {
				job.Progress = float64(sent) / float64(total) * 100
			}
		}
		m.mu.Unlock()
	})
	if err != nil {
		m.failJob(job, "upload failed: "+err.Error())
		return
	}
	if err := m.Names.Save(); err != nil {
		// Non-fatal: the file is on the card, only the long name is lost.
		log.Printf("forge: saving name map: %v", err)
	}
	if !startPrint {
		m.finishJob(job)
		_ = drv.RefreshFiles()
		return
	}

	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	if m.job != job || m.drv != drv {
		m.mu.Unlock()
		return
	}
	job.SentBytes, job.TotalBytes, job.Progress = 0, 0, 0
	job.StartedAt = time.Now()
	m.sdSeen = false
	m.state = StatePrinting
	m.mu.Unlock()
	if err := startSD(drv, short); err != nil {
		m.failJob(job, "starting the SD print failed: "+err.Error())
	}
}

func startSD(drv *Driver, short string) error {
	if err := drv.Send("M23 " + short); err != nil {
		return err
	}
	return drv.Send("M24")
}

// runStream feeds localPath line by line. SendCtx blocks while Marlin's
// window is full, so the file is never buffered in RAM.
func (m *Manager) runStream(ctx context.Context, drv *Driver, job *Job, localPath string) {
	f, err := os.Open(localPath)
	if err != nil {
		m.failJob(job, "stream failed: "+err.Error())
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var sent int64
	lastPct := -1
	for scanner.Scan() {
		if !m.waitIfPaused(ctx) {
			return
		}
		line := scanner.Text()
		sent += int64(len(line)) + 1
		if clean := stripComment(line); clean != "" {
			if err := drv.SendCtx(ctx, clean); err != nil {
				m.streamFailed(ctx, drv, job, err)
				return
			}
		}
		m.mu.Lock()
		if m.job != job {
			m.mu.Unlock()
			return
		}
		job.SentBytes = sent
		if job.TotalBytes > 0 {
			job.Progress = float64(sent) / float64(job.TotalBytes) * 100
		}
		pct := int(job.Progress)
		m.mu.Unlock()
		if pct != lastPct {
			lastPct = pct
			if err := drv.SendCtx(ctx, fmt.Sprintf("M73 P%d", pct)); err != nil {
				m.streamFailed(ctx, drv, job, err)
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		m.streamFailed(ctx, drv, job, err)
		return
	}
	// Every line has been handed to SendCtx, but Marlin may still be
	// printing what is queued or outstanding in the window: wait for it to
	// drain before the job reads idle (SEC-13; Cancel/Pause must still work
	// until the printer has actually caught up).
	if err := drv.WaitDrained(ctx); err != nil {
		if ctx.Err() != nil {
			return // job cancelled/failed elsewhere; already handled
		}
		m.streamFailed(ctx, drv, job, err)
		return
	}
	m.finishJob(job)
}

// streamFailed ends a stream that broke mid-print (SEC-18) and cools the
// printer down, unless the job was cancelled or the link is gone.
func (m *Manager) streamFailed(ctx context.Context, drv *Driver, job *Job, err error) {
	if ctx.Err() != nil {
		return
	}
	if m.failJob(job, "stream failed: "+err.Error()) && !errors.Is(err, ErrClosed) {
		m.seq.Lock()
		_ = cooldownAndPark(drv)
		m.seq.Unlock()
	}
}

// waitIfPaused blocks while a stream job is paused. It returns false once
// ctx is cancelled.
func (m *Manager) waitIfPaused(ctx context.Context) bool {
	for {
		m.mu.Lock()
		ch := m.resume
		m.mu.Unlock()
		if ch == nil {
			return ctx.Err() == nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// StartSDPrint selects and starts a file already on the SD card as a job.
func (m *Manager) StartSDPrint(short string) error {
	short = strings.ToUpper(strings.TrimSpace(short))
	if !valid83(short) {
		return ErrInvalidCommand
	}
	long := m.Names.LongFor(short)
	m.sync()
	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	drv := m.drv
	if drv == nil {
		m.mu.Unlock()
		return ErrDisconnected
	}
	if m.job != nil || m.worker != nil {
		m.mu.Unlock()
		return ErrJobActive
	}
	job := &Job{Filename: long, Mode: ModeSDUpload, StartedAt: time.Now()}
	m.startJobLocked(job, nil)
	m.state = StatePrinting
	m.mu.Unlock()
	if err := startSD(drv, short); err != nil {
		m.failJob(job, "starting the SD print failed: "+err.Error())
		return err
	}
	return nil
}

// Pause pauses the job the way its own mode needs (SEC-7): M25 for an SD
// print, holding back the feeder for a stream.
func (m *Manager) Pause() error {
	m.sync()
	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	job, drv := m.job, m.drv
	switch {
	case job == nil:
		m.mu.Unlock()
		return ErrNoJob
	case drv == nil:
		m.mu.Unlock()
		return ErrDisconnected
	case m.state == StatePaused:
		m.mu.Unlock()
		return nil
	case m.state != StatePrinting:
		m.mu.Unlock()
		return ErrNotPrinting
	}
	m.state = StatePaused
	if job.Mode == ModeStream {
		m.resume = make(chan struct{})
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := drv.Send("M25"); err != nil {
		m.mu.Lock()
		if m.job == job && m.state == StatePaused {
			m.state = StatePrinting
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

// Resume undoes Pause for the job's own mode.
func (m *Manager) Resume() error {
	m.sync()
	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	job, drv := m.job, m.drv
	switch {
	case job == nil:
		m.mu.Unlock()
		return ErrNoJob
	case drv == nil:
		m.mu.Unlock()
		return ErrDisconnected
	case m.state == StatePrinting:
		m.mu.Unlock()
		return nil
	case m.state != StatePaused:
		m.mu.Unlock()
		return ErrNotPrinting
	}
	if job.Mode == ModeStream {
		if m.resume != nil {
			close(m.resume)
			m.resume = nil
		}
		m.state = StatePrinting
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := drv.Send("M24"); err != nil {
		return err
	}
	m.mu.Lock()
	if m.job == job && m.state == StatePaused {
		m.state = StatePrinting
	}
	m.mu.Unlock()
	return nil
}

// Cancel stops the job. An SD upload only has its transfer cancelled (the
// driver closes the file with M29); nothing is printing, so no M524 and no
// cooldown go out, which would otherwise be written into the file. An SD
// print gets M524, a stream stops its feeder; both then cool down and park.
func (m *Manager) Cancel() error {
	m.sync()
	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	job, drv, state := m.job, m.drv, m.state
	if job == nil {
		m.mu.Unlock()
		return ErrNoJob
	}
	if drv == nil {
		m.mu.Unlock()
		return ErrDisconnected
	}
	sdPrint := job.Mode == ModeSDUpload && (state == StatePrinting || state == StatePaused)
	stream := job.Mode == ModeStream
	m.endJobLocked(StateIdle, "")
	m.mu.Unlock()

	switch {
	case sdPrint:
		return m.cancelSD(drv)
	case stream:
		return cooldownAndPark(drv)
	}
	return nil
}

// cancelSD aborts an SD print. Marlin's emergency parser acts on M524 at
// receipt and the abort later runs queue.clear() at an arbitrary point, so
// nothing may follow M524 until the abort is confirmed: a flush landing
// between G91 and G1 would run "G1 Z10" absolute (R-04). The confirmation
// is M27 reporting "Not SD printing", which Marlin only prints once
// abortFilePrintNow and queue.clear have both run. Marlin's abort already
// switches heaters and fans off; forge repeats that and then parks. If the
// abort is not confirmed in time the park is skipped. Called with seq held.
func (m *Manager) cancelSD(drv *Driver) error {
	wasPrinting := drv.SDStatus().Printing
	if err := drv.Send("M524"); err != nil {
		return err
	}
	if !waitSDAborted(drv, time.Now(), wasPrinting) {
		msg := "cancel: the printer did not confirm the SD abort; Z was not lifted"
		log.Printf("forge: %s", msg)
		m.mu.Lock()
		if m.job == nil {
			m.lastErr = msg
		}
		m.mu.Unlock()
		return cooldown(drv)
	}
	return cooldownAndPark(drv)
}

// waitSDAborted polls the cached M27 status until it says "Not SD
// printing". A status that already said so before M524 went out may be
// stale, so it only counts after one autoreport period.
func waitSDAborted(drv *Driver, sent time.Time, wasPrinting bool) bool {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if drv.SDStatus().NotSD && (wasPrinting || time.Since(sent) >= autoReportPeriod) {
			return true
		}
		if time.Since(sent) >= cancelFlushTimeout {
			return false
		}
		select {
		case <-t.C:
		case <-drv.Done():
			return false
		}
	}
}

// cooldown switches heaters and fan off. Every command is tried even if
// one fails; the first error is returned.
func cooldown(drv *Driver) error {
	return sendAll(drv, "M104 S0", "M140 S0", "M107")
}

// cooldownAndPark cools down and lifts Z by 10mm. The park is
// self-contained (G91, move, G90 back to back) so it never depends on a
// positioning mode set earlier.
func cooldownAndPark(drv *Driver) error {
	return sendAll(drv, "M104 S0", "M140 S0", "M107", "G91", "G1 Z10 F600", "G90")
}

func sendAll(drv *Driver, cmds ...string) error {
	var first error
	for _, c := range cmds {
		if err := drv.Send(c); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Emergency aborts any transfer and sends M112. During an SD upload Marlin
// is saving to the file with its emergency parser off, so the transfer is
// cancelled first and given a moment to close the file with M29.
func (m *Manager) Emergency() error {
	m.mu.Lock()
	drv := m.drv
	if drv == nil {
		m.mu.Unlock()
		return ErrDisconnected
	}
	var wait chan struct{}
	if m.state == StateUploading {
		wait = m.worker
	}
	m.endJobLocked(StateError, "emergency stop (M112) sent; reset the printer")
	m.mu.Unlock()
	if wait != nil {
		// Marlin ignores M112 while saving to a file (R-10), so wait until
		// the transfer has closed it with M29.
		t := time.NewTimer(emergencyUploadWait)
		select {
		case <-wait:
		case <-t.C:
		}
		t.Stop()
	}
	if err := drv.SendEmergency("M112"); err != nil {
		return err
	}
	if drv.Uploading() {
		return errors.New("M112 sent while the printer was still saving an upload and may have been ignored; use the printer's reset button")
	}
	return nil
}

// Send queues one console command.
func (m *Manager) Send(cmd string) error {
	return m.SendSequence([]string{cmd})
}

// SendSequence sends cmds in order, one caller at a time, so a jog's
// G91/G1/G90 can never interleave with another request (SEC-8). It stops
// at the first error.
//
// Motion, positioning-mode and EEPROM commands are refused with
// ErrJobActive while a job runs (R-05): a stray Home or jog mid-print drives
// the nozzle into the part, and M500 stalls the MCU on its flash write.
func (m *Manager) SendSequence(cmds []string) error {
	m.sync()
	m.seq.Lock()
	defer m.seq.Unlock()
	m.mu.Lock()
	drv, busy := m.drv, m.job != nil || m.worker != nil
	m.mu.Unlock()
	if drv == nil {
		return ErrDisconnected
	}
	if busy {
		for _, c := range cmds {
			if unsafeDuringJob(c) {
				return ErrJobActive
			}
		}
	}
	for _, c := range cmds {
		if err := drv.Send(c); err != nil {
			return err
		}
	}
	return nil
}

// unsafeDuringJob reports whether cmd moves axes, changes positioning or
// writes/resets the stored settings.
func unsafeDuringJob(cmd string) bool {
	f := strings.Fields(strings.ToUpper(cmd))
	if len(f) == 0 || len(f[0]) < 2 {
		return false
	}
	num, err := strconv.Atoi(f[0][1:])
	if err != nil {
		return false
	}
	switch f[0][0] {
	case 'G':
		switch num {
		case 0, 1, 2, 3, 5, 28, 29, 90, 91, 92:
			return true
		}
	case 'M':
		switch num {
		case 500, 502:
			return true
		}
	}
	return false
}

// Babystep moves Z by deltaMM (M290), limited to 1mm per call (SEC-35).
func (m *Manager) Babystep(deltaMM float64) error {
	if math.IsNaN(deltaMM) || math.Abs(deltaMM) > 1.0 {
		return ErrInvalidCommand
	}
	s := formatMM(deltaMM)
	if s == "0" {
		return nil
	}
	if err := m.Send("M290 Z" + s); err != nil {
		return err
	}
	v, _ := strconv.ParseFloat(s, 64)
	m.mu.Lock()
	if m.job != nil {
		m.job.BabystepMM += v
	}
	m.mu.Unlock()
	return nil
}

// formatMM renders v with at most 3 decimals and no float noise:
// 0.05 -> "0.05", -0.01 -> "-0.01", 1 -> "1".
func formatMM(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "-0" || s == "" {
		s = "0"
	}
	return s
}

// SaveSettings stores Marlin's settings (including babystep Z) with M500.
func (m *Manager) SaveSettings() error {
	return m.Send("M500")
}

// Console returns the recent printer output, empty while disconnected.
func (m *Manager) Console() []string {
	if drv := m.driver(); drv != nil {
		return drv.Console()
	}
	return []string{}
}

// SDFiles returns the last SD listing, empty while disconnected.
func (m *Manager) SDFiles() []SDFile {
	if drv := m.driver(); drv != nil {
		return drv.SDFiles()
	}
	return []SDFile{}
}

// activeDriver returns the driver when no job runs.
func (m *Manager) activeDriver() (*Driver, error) {
	m.sync()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drv == nil {
		return nil, ErrDisconnected
	}
	if m.job != nil || m.worker != nil {
		return nil, ErrJobActive
	}
	return m.drv, nil
}

// RefreshFiles asks for a new M20 listing. Refused while a job runs:
// walking the card during a print or upload costs the printer time.
func (m *Manager) RefreshFiles() error {
	drv, err := m.activeDriver()
	if err != nil {
		return err
	}
	return drv.RefreshFiles()
}

// DeleteFile removes short from the SD card and forgets its long name.
func (m *Manager) DeleteFile(short string) error {
	short = strings.ToUpper(strings.TrimSpace(short))
	if !valid83(short) {
		return ErrInvalidCommand
	}
	// seq before the job check, so StartSDPrint cannot start this file
	// between the check and the M30 (R-17).
	m.seq.Lock()
	drv, err := m.activeDriver()
	if err == nil {
		err = drv.Send("M30 " + short)
	}
	m.seq.Unlock()
	if err != nil {
		return err
	}
	m.Names.Forget(short)
	if err := m.Names.Save(); err != nil {
		log.Printf("forge: saving name map: %v", err)
	}
	_ = drv.RefreshFiles()
	return nil
}
