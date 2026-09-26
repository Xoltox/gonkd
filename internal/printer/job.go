package printer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Manager tracks the current job (upload/print/stream) on top of a Driver,
// and is the type Server talks to. It serializes job control so only one
// upload/print/stream runs at a time.
type Manager struct {
	Driver *Driver
	Names  *NameMap

	mu      sync.Mutex
	job     *Job
	cancel  context.CancelFunc
	paused  bool
	state   State
	lastErr string
}

func NewManager(d *Driver, names *NameMap) *Manager {
	return &Manager{Driver: d, Names: names, state: StateIdle}
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Manager) setState(s State) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

func (m *Manager) SetError(err error) {
	m.mu.Lock()
	m.state = StateError
	m.lastErr = err.Error()
	m.mu.Unlock()
}

// Snapshot builds the current point-in-time view for the JSON API.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	job := m.job
	state := m.state
	lastErr := m.lastErr
	m.mu.Unlock()

	var jobCopy *Job
	if job != nil {
		j := *job
		j.ElapsedSec = time.Since(j.StartedAt).Seconds()
		if j.Progress > 0 {
			j.ETASec = j.ElapsedSec/j.Progress*100 - j.ElapsedSec
		}
		jobCopy = &j
	}

	caps := m.Driver.Capabilities()
	capList := make([]string, 0, len(caps))
	for k, v := range caps {
		if v {
			capList = append(capList, k)
		}
	}

	return Snapshot{
		State:        state,
		Connected:    true,
		Temps:        m.Driver.Temps(),
		Job:          jobCopy,
		Capabilities: capList,
		LastError:    lastErr,
	}
}

// UploadAndMaybePrint handles the default "upload to SD" flow used by both
// the OctoPrint-compatible API and forge's own UI.
func (m *Manager) UploadAndMaybePrint(localPath, longName string, startPrint bool) (string, error) {
	m.mu.Lock()
	if m.job != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("a job is already active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	job := &Job{Filename: longName, Mode: ModeSDUpload, StartedAt: time.Now()}
	m.job = job
	m.mu.Unlock()

	m.setState(StateUploading)
	short, err := m.Driver.UploadToSD(ctx, localPath, longName, m.Names, func(sent, total int64) {
		m.mu.Lock()
		job.SentBytes = sent
		job.TotalBytes = total
		if total > 0 {
			job.Progress = float64(sent) / float64(total) * 100
		}
		m.mu.Unlock()
	})
	_ = os.Remove(localPath) // /tmp is precious on this box; always clean up

	if err != nil {
		m.clearJob()
		m.setState(StateIdle)
		return "", err
	}
	if err := m.Names.Save(); err != nil {
		// Non-fatal: the upload succeeded even if we couldn't persist the
		// long-name mapping this time.
		_ = err
	}

	if startPrint {
		m.Driver.Send(fmt.Sprintf("M23 %s", short))
		m.Driver.Send("M24")
		m.setState(StatePrinting)
	} else {
		m.clearJob()
		m.setState(StateIdle)
	}
	return short, nil
}

func (m *Manager) clearJob() {
	m.mu.Lock()
	m.job = nil
	m.cancel = nil
	m.mu.Unlock()
}

// PauseSD / ResumeSD / CancelSD control an SD-card print in progress.
func (m *Manager) PauseSD() {
	m.Driver.Send("M25")
	m.setState(StatePaused)
}

func (m *Manager) ResumeSD() {
	m.Driver.Send("M24")
	m.setState(StatePrinting)
}

// CancelSD stops an SD print: M524 (Marlin's SD-print abort) then a safety
// cooldown (heaters off, fan off) and a park/lift, per the spec.
func (m *Manager) CancelSD() {
	m.setState(StateCancelling)
	m.Driver.Send("M524")
	m.cooldownAndPark()
	m.clearJob()
	m.setState(StateIdle)
}

func (m *Manager) cooldownAndPark() {
	m.Driver.Send("M104 S0")
	m.Driver.Send("M140 S0")
	m.Driver.Send("M107")
	m.Driver.Send("G91")
	m.Driver.Send("G1 Z10 F600") // lift
	m.Driver.Send("G90")
}

// StreamPrint sends localPath line-by-line (mode 2), keeping Marlin's
// buffers full via the normal windowed queue, reporting progress with M73
// as it goes.
func (m *Manager) StreamPrint(localPath, longName string) error {
	m.mu.Lock()
	if m.job != nil {
		m.mu.Unlock()
		return fmt.Errorf("a job is already active")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	job := &Job{Filename: longName, Mode: ModeStream, StartedAt: time.Now()}
	m.job = job
	m.mu.Unlock()
	m.setState(StatePrinting)

	info, err := os.Stat(localPath)
	if err != nil {
		m.clearJob()
		m.setState(StateIdle)
		return err
	}
	job.TotalBytes = info.Size()

	go func() {
		defer os.Remove(localPath)
		f, err := os.Open(localPath)
		if err != nil {
			m.SetError(err)
			m.clearJob()
			return
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var sent int64
		lastPct := -1
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				m.clearJob()
				m.setState(StateIdle)
				return
			default:
			}
			m.mu.Lock()
			paused := m.paused
			m.mu.Unlock()
			for paused {
				time.Sleep(200 * time.Millisecond)
				select {
				case <-ctx.Done():
					m.clearJob()
					m.setState(StateIdle)
					return
				default:
				}
				m.mu.Lock()
				paused = m.paused
				m.mu.Unlock()
			}

			line := scanner.Text()
			clean := stripComment(line)
			sent += int64(len(line)) + 1
			if clean != "" {
				m.Driver.Send(clean)
			}
			m.mu.Lock()
			job.SentBytes = sent
			if job.TotalBytes > 0 {
				job.Progress = float64(sent) / float64(job.TotalBytes) * 100
			}
			pct := int(job.Progress)
			m.mu.Unlock()
			if pct != lastPct {
				lastPct = pct
				m.Driver.Send(fmt.Sprintf("M73 P%d", pct))
			}
		}
		m.clearJob()
		m.setState(StateIdle)
	}()
	return nil
}

// PauseStream / ResumeStream / CancelStream control a stream-mode print.
func (m *Manager) PauseStream() {
	m.mu.Lock()
	m.paused = true
	m.mu.Unlock()
	m.setState(StatePaused)
}

func (m *Manager) ResumeStream() {
	m.mu.Lock()
	m.paused = false
	m.mu.Unlock()
	m.setState(StatePrinting)
}

func (m *Manager) CancelStream() {
	m.setState(StateCancelling)
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.cooldownAndPark()
}

// EmergencyStop sends M112 immediately, bypassing everything.
func (m *Manager) EmergencyStop() {
	_ = m.Driver.SendEmergency("M112")
	m.clearJob()
	m.setState(StateError)
}
